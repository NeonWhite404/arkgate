package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
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

// NewManager 构造传输器；timeout 为单次上游请求的兜底超时。
func NewManager(timeout time.Duration) *Manager {
	return &Manager{httpc: &http.Client{Timeout: timeout}}
}

// post 经 SDK 发送一次非流式调用，返回上游原始响应字节（错误响应体也完整保留）。
func (m *Manager) post(ctx context.Context, rt Route, path string, body []byte) ([]byte, error) {
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
func (m *Manager) Chat(ctx context.Context, rt Route, down []byte, upstreamModel string) ([]byte, *TextUsage, error) {
	body, err := prepareBody(down, upstreamModel)
	if err != nil {
		return nil, nil, err
	}
	raw, err := m.post(ctx, rt, "chat/completions", body)
	if err != nil {
		return nil, nil, err
	}
	return raw, ExtractChatUsage(raw), nil
}

// ChatStream 转发流式 chat/completions（强制 include_usage），SSE 原样回传。
func (m *Manager) ChatStream(ctx context.Context, rt Route, down []byte, upstreamModel string, sink io.Writer) (*TextUsage, error) {
	body, err := prepareStreamBody(down, upstreamModel)
	if err != nil {
		return nil, err
	}
	pt, ct, err := m.streamForward(ctx, rt, "chat/completions", body, chatUsageFromChunk, sink)
	return &TextUsage{PromptTokens: pt, CompletionTokens: ct}, err
}

// ─────────────────────────── responses ───────────────────────────

// Responses 转发非流式 responses，返回上游原始响应与用量（input/output → prompt/completion）。
func (m *Manager) Responses(ctx context.Context, rt Route, down []byte, upstreamModel string) ([]byte, *TextUsage, error) {
	body, err := prepareBody(down, upstreamModel)
	if err != nil {
		return nil, nil, err
	}
	raw, err := m.post(ctx, rt, "responses", body)
	if err != nil {
		return nil, nil, err
	}
	return raw, ExtractResponsesUsage(raw), nil
}

// ResponsesStream 转发流式 responses；从 response.completed 事件提取用量。
// 客户端请求 stream 时才会走此路径，故透传体不再强制补 stream 字段。
func (m *Manager) ResponsesStream(ctx context.Context, rt Route, down []byte, upstreamModel string, sink io.Writer) (*TextUsage, error) {
	body, err := prepareBody(down, upstreamModel)
	if err != nil {
		return nil, err
	}
	pt, ct, err := m.streamForward(ctx, rt, "responses", body, responsesUsageFromEvent, sink)
	return &TextUsage{PromptTokens: pt, CompletionTokens: ct}, err
}

// ─────────────────────────── images/generations ───────────────────────────

// Images 转发非流式 images/generations，返回原始响应与张数计量。
func (m *Manager) Images(ctx context.Context, rt Route, down []byte, upstreamModel string) ([]byte, *ImageUsage, error) {
	body, err := prepareBody(down, upstreamModel)
	if err != nil {
		return nil, nil, err
	}
	raw, err := m.post(ctx, rt, "images/generations", body)
	if err != nil {
		return nil, nil, err
	}
	return raw, ExtractImageUsage(raw), nil
}

// ImagesStream 转发流式 images/generations（partial images）。无 usage 事件，
// 张数按请求里的 n 计量；失败返回 0（部分图片可能已送达，但按失败不计费）。
func (m *Manager) ImagesStream(ctx context.Context, rt Route, down []byte, upstreamModel string, sink io.Writer) (int64, error) {
	body, err := prepareBody(down, upstreamModel)
	if err != nil {
		return 0, err
	}
	if _, _, err := m.streamForward(ctx, rt, "images/generations", body, nil, sink); err != nil {
		return 0, err
	}
	return ExtractN(down), nil
}

// ─────────────────────────── 流式转发（唯一自研 HTTP） ───────────────────────────

// streamForward 用共享连接池做 SSE 字节级转发：逐行写给 sink，
// sniff（可为 nil）用于从 data: 载荷里提取用量。
func (m *Manager) streamForward(ctx context.Context, rt Route, path string, body []byte,
	sniff func(payload []byte) (pt, ct int64, ok bool), sink io.Writer) (pt, ct int64, err error) {

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(rt.BaseURL, "/")+"/"+path, bytes.NewReader(body))
	if err != nil {
		return 0, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+rt.Key)
	req.Header.Set("Accept", "text/event-stream")

	resp, err := m.httpc.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return 0, 0, errors.New("upstream timeout")
		}
		return 0, 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return 0, 0, &HTTPError{Code: resp.StatusCode, Body: respBody}
	}

	reader := bufio.NewReader(resp.Body)
	for {
		line, rerr := reader.ReadBytes('\n')
		if len(line) > 0 {
			if p, c, ok := sniffDataLine(line, sniff); ok {
				pt += p
				ct += c
			}
			if _, werr := sink.Write(line); werr != nil {
				return pt, ct, werr
			}
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				break
			}
			return pt, ct, rerr
		}
	}
	return pt, ct, nil
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
