package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
)

// Manager 是上游传输器。
//
// 职责边界（设计决策 D10）：SDK 负责「我们作为客户端」的行为（鉴权 / base URL /
// 瞬时重试 / 超时 / 连接复用）；原始字节负责「我们作为代理」的转发（请求体与
// 响应均只替换 model，其余字段原样穿越）。
//
// 非流式：经 client.Post 泛型入口发送 json.RawMessage（SDK 按其 MarshalJSON
// 原样序列化），res *[]byte 直接取回原始响应字节。
// 流式：SDK 的 typed iterator 面向消费者而非代理（重组事件有丢字段风险），
// 因此用共享连接池做 SSE 字节级转发——这是唯一保留的「自研 HTTP」。
type Manager struct {
	httpc *http.Client
}

// NewManager 构造传输器。
// 注意：http.Client.Timeout 故意留空——单次上游请求的超时由调用方按当前配置
// 用 context 施加（见 Manager.post / openStream），这样管理端热改超时后立即生效，
// 无需重建客户端或重启进程；连接池与 keep-alive 仍复用同一客户端。
func NewManager() *Manager {
	return &Manager{httpc: &http.Client{}}
}

// post 经 SDK 发送一次非流式调用，返回上游原始响应字节（错误响应体也完整保留）。
// timeout > 0 时对本次调用施加整体超时（含读完响应体）。
func (m *Manager) post(ctx context.Context, rt Route, path string, body []byte, timeout time.Duration) ([]byte, error) {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	var (
		captured []byte // 上游响应原始字节（含错误响应，供原样透传）
		status   int
	)
	client := openai.NewClient(
		option.WithAPIKey(rt.Key),
		option.WithBaseURL(rt.BaseURL),
		option.WithHTTPClient(m.httpc),
		option.WithMaxRetries(1), // 同叶子瞬时重试；跨叶子重试由网关负责
	)
	var raw []byte
	err := client.Post(ctx, path, json.RawMessage(body), &raw,
		option.WithMiddleware(func(req *http.Request, next option.MiddlewareNext) (*http.Response, error) {
			resp, err := next(req)
			if err != nil {
				return resp, err
			}
			if resp != nil && resp.Body != nil {
				// 复制暂存后回放，保证 SDK 内部（错误解析/重试）与我们都可读。
				data, rerr := io.ReadAll(resp.Body)
				resp.Body.Close()
				if rerr == nil {
					captured, status = data, resp.StatusCode
					resp.Body = io.NopCloser(bytes.NewReader(data))
				} else {
					resp.Body = io.NopCloser(bytes.NewReader(nil))
				}
			}
			return resp, nil
		}),
	)
	if err != nil {
		if status >= 400 {
			return nil, &HTTPError{Code: status, Body: captured}
		}
		return nil, err
	}
	if raw == nil { // 防御：res *[]byte 正常必已填充
		raw = captured
	}
	return raw, nil
}

// ─────────────────────────── chat/completions ───────────────────────────

// Chat 转发非流式 chat/completions，返回上游原始响应与用量。
// timeout 为本次调用的整体超时（0 = 不限）。
func (m *Manager) Chat(ctx context.Context, rt Route, down []byte, upstreamModel string, timeout time.Duration) ([]byte, *TextUsage, error) {
	body, err := prepareBody(down, upstreamModel)
	if err != nil {
		return nil, nil, err
	}
	raw, err := m.post(ctx, rt, "chat/completions", body, timeout)
	if err != nil {
		return nil, nil, err
	}
	return raw, ExtractChatUsage(raw), nil
}

// ─────────────────────────── responses ───────────────────────────

// Responses 转发非流式 responses，返回上游原始响应与用量（input/output → prompt/completion）。
// timeout 为本次调用的整体超时（0 = 不限）。
func (m *Manager) Responses(ctx context.Context, rt Route, down []byte, upstreamModel string, timeout time.Duration) ([]byte, *TextUsage, error) {
	body, err := prepareBody(down, upstreamModel)
	if err != nil {
		return nil, nil, err
	}
	raw, err := m.post(ctx, rt, "responses", body, timeout)
	if err != nil {
		return nil, nil, err
	}
	return raw, ExtractResponsesUsage(raw), nil
}

// ─────────────────────────── images/generations ───────────────────────────

// Images 转发非流式 images/generations，返回原始响应与张数计量。
// timeout 为本次调用的整体超时（0 = 不限）。
func (m *Manager) Images(ctx context.Context, rt Route, down []byte, upstreamModel string, timeout time.Duration) ([]byte, *ImageUsage, error) {
	body, err := prepareBody(down, upstreamModel)
	if err != nil {
		return nil, nil, err
	}
	raw, err := m.post(ctx, rt, "images/generations", body, timeout)
	if err != nil {
		return nil, nil, err
	}
	return raw, ExtractImageUsage(raw), nil
}

// ─────────────────────────── 流式转发（唯一自研 HTTP） ───────────────────────────
//
// 流式被拆成两段，让「首字节之前」的失败可以安全换叶子重试：
//  1. openStream：建连 + 等响应头 + 等首个数据行（首 token）。此阶段失败时
//     未向客户端写过任何字节，网关可排除当前叶子换下一个重试。
//  2. Stream.Pump：首字节到手后由网关提交 SSE 响应头，再把整条流写给 sink。
//     一旦开始 Pump 就无法重试（客户端已收到数据）。

// ErrFirstToken 表示流式请求在产出任何字节前失败（首 token 超时等），
// 未向客户端写过数据，调用方可安全重试。
var ErrFirstToken = errors.New("上游首字节超时")

// Stream 是一条已打开（已收到首字节）的上游流式响应。
type Stream struct {
	resp   *http.Response
	rdr    *bufio.Reader
	first  []byte // openStream 已读出的首个数据行
	sniff  func(payload []byte) (pt, ct int64, ok bool)
	cancel context.CancelFunc // 首 token 超时场景下创建的子 ctx（可能为 nil）
}

// Close 关闭流并释放底层资源。
func (s *Stream) Close() {
	if s == nil {
		return
	}
	if s.resp != nil && s.resp.Body != nil {
		s.resp.Body.Close()
	}
	if s.cancel != nil {
		s.cancel()
	}
}

// Pump 把整条流写给 sink（含 openStream 已读出的首行），返回从流中提取的用量。
func (s *Stream) Pump(sink io.Writer) (pt, ct int64, err error) {
	line := s.first
	for {
		if len(line) > 0 {
			if p, c, ok := sniffDataLine(line, s.sniff); ok {
				pt += p
				ct += c
			}
			if _, werr := sink.Write(line); werr != nil {
				return pt, ct, werr
			}
		}
		line, err = s.rdr.ReadBytes('\n')
		if err != nil {
			if errors.Is(err, io.EOF) {
				return pt, ct, nil
			}
			return pt, ct, err
		}
	}
}

// openStream 发起流式请求并等待响应头 + 首个数据行（首 token）。
// firstTokenTimeout > 0 时，该时长内未收到任何字节即取消请求并返回 ErrFirstToken
// 包装错误。非 2xx 返回 HTTPError（错误体原样保留）。
func (m *Manager) openStream(ctx context.Context, rt Route, path string, body []byte,
	firstTokenTimeout time.Duration, sniff func(payload []byte) (int64, int64, bool)) (_ *Stream, err error) {

	reqCtx := ctx
	var cancel context.CancelFunc
	var fired atomic.Bool
	var timer *time.Timer
	if firstTokenTimeout > 0 {
		reqCtx, cancel = context.WithCancel(ctx)
		defer func() {
			if err != nil {
				cancel() // 失败路径释放子 ctx；成功路径由 Stream.Close 负责
			}
		}()
		timer = time.AfterFunc(firstTokenTimeout, func() {
			// CAS 抢占：只有抢到的一方才能取消请求；首字节到手后主流程
			// 也会 CAS 置位，使迟到的定时器回调变成 no-op。
			if fired.CompareAndSwap(false, true) {
				cancel()
			}
		})
		defer timer.Stop()
	}

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost,
		strings.TrimRight(rt.BaseURL, "/")+"/"+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+rt.Key)
	req.Header.Set("Accept", "text/event-stream")

	resp, err := m.httpc.Do(req)
	if err != nil {
		if timer != nil && fired.Load() {
			return nil, fmt.Errorf("%w（等待 %s 无输出）", ErrFirstToken, firstTokenTimeout)
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, errors.New("upstream timeout")
		}
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, &HTTPError{Code: resp.StatusCode, Body: respBody}
	}

	reader := bufio.NewReader(resp.Body)
	first, rerr := reader.ReadBytes('\n')
	if rerr != nil && len(first) == 0 {
		// 一个字节都没拿到：按「首 token 前失败」处理（可重试）。
		resp.Body.Close()
		if timer != nil && fired.Load() {
			return nil, fmt.Errorf("%w（等待 %s 无输出）", ErrFirstToken, firstTokenTimeout)
		}
		return nil, fmt.Errorf("上游未返回任何数据: %w", rerr)
	}
	if timer != nil {
		// 首字节已到手。CAS 失败说明定时器恰好同时触发并已取消请求——
		// 宁可放弃这条已到手的流（极小概率窗口），也不让它中途被取消。
		if !fired.CompareAndSwap(false, true) {
			resp.Body.Close()
			return nil, fmt.Errorf("%w（首字节与超时同时到达）", ErrFirstToken)
		}
		timer.Stop()
	}
	return &Stream{resp: resp, rdr: reader, first: first, sniff: sniff, cancel: cancel}, nil
}

// ─────────────────────────── 流式打开（网关用） ───────────────────────────

// OpenChatStream 打开 chat/completions 流（强制 stream + include_usage）。
// 返回成功即已收到首字节；失败时未向客户端写过任何字节，可换叶子重试。
func (m *Manager) OpenChatStream(ctx context.Context, rt Route, down []byte, upstreamModel string, firstTokenTimeout time.Duration) (*Stream, error) {
	body, err := prepareStreamBody(down, upstreamModel)
	if err != nil {
		return nil, err
	}
	return m.openStream(ctx, rt, "chat/completions", body, firstTokenTimeout, chatUsageFromChunk)
}

// OpenResponsesStream 打开 responses 流（从 response.completed 提取用量）。
func (m *Manager) OpenResponsesStream(ctx context.Context, rt Route, down []byte, upstreamModel string, firstTokenTimeout time.Duration) (*Stream, error) {
	body, err := prepareBody(down, upstreamModel)
	if err != nil {
		return nil, err
	}
	return m.openStream(ctx, rt, "responses", body, firstTokenTimeout, responsesUsageFromEvent)
}

// OpenImagesStream 打开 images/generations 流（partial images）。无 usage 事件。
func (m *Manager) OpenImagesStream(ctx context.Context, rt Route, down []byte, upstreamModel string, firstTokenTimeout time.Duration) (*Stream, error) {
	body, err := prepareBody(down, upstreamModel)
	if err != nil {
		return nil, err
	}
	return m.openStream(ctx, rt, "images/generations", body, firstTokenTimeout, nil)
}

// ─────────────────────────── 流式便捷封装（打开 + 一次泵完） ───────────────────────────

// ChatStream 转发流式 chat/completions（强制 include_usage），SSE 原样回传。
func (m *Manager) ChatStream(ctx context.Context, rt Route, down []byte, upstreamModel string, sink io.Writer) (*TextUsage, error) {
	st, err := m.OpenChatStream(ctx, rt, down, upstreamModel, 0)
	if err != nil {
		return nil, err
	}
	defer st.Close()
	pt, ct, err := st.Pump(sink)
	return &TextUsage{PromptTokens: pt, CompletionTokens: ct}, err
}

// ResponsesStream 转发流式 responses；从 response.completed 事件提取用量。
// 客户端请求 stream 时才会走此路径，故透传体不再强制补 stream 字段。
func (m *Manager) ResponsesStream(ctx context.Context, rt Route, down []byte, upstreamModel string, sink io.Writer) (*TextUsage, error) {
	st, err := m.OpenResponsesStream(ctx, rt, down, upstreamModel, 0)
	if err != nil {
		return nil, err
	}
	defer st.Close()
	pt, ct, err := st.Pump(sink)
	return &TextUsage{PromptTokens: pt, CompletionTokens: ct}, err
}

// ImagesStream 转发流式 images/generations（partial images）。无 usage 事件，
// 张数按请求里的 n 计量；失败返回 0（部分图片可能已送达，但按失败不计费）。
func (m *Manager) ImagesStream(ctx context.Context, rt Route, down []byte, upstreamModel string, sink io.Writer) (int64, error) {
	st, err := m.OpenImagesStream(ctx, rt, down, upstreamModel, 0)
	if err != nil {
		return 0, err
	}
	defer st.Close()
	if _, _, err := st.Pump(sink); err != nil {
		return 0, err
	}
	return ExtractN(down), nil
}

// sniffDataLine 判断一行 SSE 是否携带 data: 载荷，并交给 sniff 提取用量。
func sniffDataLine(line []byte, sniff func(payload []byte) (int64, int64, bool)) (int64, int64, bool) {
	if sniff == nil {
		return 0, 0, false
	}
	s := string(line)
	if !strings.HasPrefix(s, "data:") {
		return 0, 0, false
	}
	payload := strings.TrimSpace(strings.TrimPrefix(s, "data:"))
	if payload == "" || payload == "[DONE]" {
		return 0, 0, false
	}
	return sniff([]byte(payload))
}

// chatUsageFromChunk 从 chat 流式 chunk（include_usage 的 final chunk）提取用量。
func chatUsageFromChunk(payload []byte) (int64, int64, bool) {
	var parsed struct {
		Usage *struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(payload, &parsed) == nil && parsed.Usage != nil {
		return parsed.Usage.PromptTokens, parsed.Usage.CompletionTokens, true
	}
	return 0, 0, false
}

// responsesUsageFromEvent 从 response.completed 事件提取用量。
func responsesUsageFromEvent(payload []byte) (int64, int64, bool) {
	var parsed struct {
		Type string `json:"type"`
		Response *struct {
			Usage *struct {
				InputTokens  int64 `json:"input_tokens"`
				OutputTokens int64 `json:"output_tokens"`
			} `json:"usage"`
		} `json:"response"`
	}
	if json.Unmarshal(payload, &parsed) == nil &&
		parsed.Type == "response.completed" && parsed.Response != nil && parsed.Response.Usage != nil {
		return parsed.Response.Usage.InputTokens, parsed.Response.Usage.OutputTokens, true
	}
	return 0, 0, false
}
