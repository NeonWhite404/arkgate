// Package upstream 封装对火山方舟 Ark API 的调用：v3 协议 → OpenAI 兼容协议转换。
//
// 关键点：
//   - 火山 chat/completions 的请求体字段与 OpenAI 高度一致（messages/model/stream/
//     temperature/top_p/max_tokens/max_completion_tokens/tools/tool_choice 等）。
//   - 转换策略：**以透传为主**。除「model 替换为真实 ep」「流式强制开启并注入
//     include_usage」外，其余字段原样透传，避免对 `max_tokens`/`max_completion_tokens`/
//     `thinking`/`reasoning_effort` 之类做模型不敏感的猜测性改写（那类改写既会误伤
//     非思考模型，也会漏掉真正的思考模型）。
//   - 思维链字段火山用 reasoning_content；OpenAI 新生态用 reasoning。这里不做字段改写，
//     保证标准 OpenAI 请求与火山请求双向语义一致。
package upstream

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

	"arkgate/internal/config"
)

// Client 是火山上游客户端。并发安全：每个请求独立构造，无共享可变状态。
type Client struct {
	baseURL string
	httpc   *http.Client
}

// New 构造上游客户端。
func New(cfg *config.Config) *Client {
	return &Client{
		baseURL: cfg.ArkBaseURL,
		httpc:   &http.Client{Timeout: cfg.RequestTimeout},
	}
}

// Result 表示一次非流式响应的剥离结果，用于提取用量。
type Result struct {
	PromptTokens     int64
	CompletionTokens int64
	TotalTokens      int64
	Model            string
}

// ─────────────────────────── 请求构造 ───────────────────────────

// prepareRequest 把下游 OpenAI 请求改装成火山请求（透传 + 强制 model=ep）。
func prepareRequest(down []byte, ep string) ([]byte, error) {
	raw := map[string]any{}
	dec := json.NewDecoder(bytes.NewReader(down))
	dec.UseNumber()
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("invalid json: %w", err)
	}
	// 写死 model = 真实 EP。其余字段透传，不做猜测性改写。
	raw["model"] = ep
	out, err := json.Marshal(raw)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ─────────────────────────── 非流式调用 ───────────────────────────

// ChatCompletion 转发非流式 chat/completions，返回完整响应体与用量。
func (c *Client) ChatCompletion(ctx context.Context, apiKey string, body []byte, ep string) ([]byte, *Result, error) {
	reqBody, err := prepareRequest(body, ep)
	if err != nil {
		return nil, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(reqBody))
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := c.httpc.Do(req)
	if err != nil {
		return nil, nil, wrapNetErr(err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		return respBody, nil, &HTTPError{Code: resp.StatusCode, Body: respBody}
	}

	res := &Result{}
	var parsed struct {
		Usage *struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
			TotalTokens      int64 `json:"total_tokens"`
		} `json:"usage"`
		Model string `json:"model"`
	}
	if json.Unmarshal(respBody, &parsed) == nil {
		if parsed.Usage != nil {
			res.PromptTokens = parsed.Usage.PromptTokens
			res.CompletionTokens = parsed.Usage.CompletionTokens
			res.TotalTokens = parsed.Usage.TotalTokens
		}
		res.Model = parsed.Model
	}
	return respBody, res, nil
}

// ─────────────────────────── 流式调用 ───────────────────────────

// Stream 转发流式 chat/completions，逐行把 SSE 数据写给 sink。
// 返回累计用量（由 include_usage 的 final chunk 提供）。
func (c *Client) Stream(ctx context.Context, apiKey string, body []byte, ep string, sink io.Writer) (pt, ct int64, err error) {
	reqBody, err := prepareRequest(body, ep)
	if err != nil {
		return 0, 0, err
	}
	// 强制流式 + 强制 include_usage（向下兼容下游自行传入的 stream_options 其它字段）。
	var raw map[string]any
	_ = json.Unmarshal(reqBody, &raw)
	raw["stream"] = true
	so, _ := raw["stream_options"].(map[string]any)
	if so == nil {
		so = map[string]any{}
	}
	so["include_usage"] = true
	raw["stream_options"] = so
	reqBody, _ = json.Marshal(raw)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(reqBody))
	if err != nil {
		return 0, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := c.httpc.Do(req)
	if err != nil {
		return 0, 0, wrapNetErr(err)
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
			if p, c2, ok := scanSSELine(line); ok {
				pt += p
				ct += c2
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

// scanSSELine 从一行 SSE 里提取 usage（若存在）。
func scanSSELine(line []byte) (pt, ct int64, ok bool) {
	s := string(line)
	if !strings.HasPrefix(s, "data:") {
		return 0, 0, false
	}
	payload := strings.TrimSpace(strings.TrimPrefix(s, "data:"))
	if payload == "" || payload == "[DONE]" {
		return 0, 0, false
	}
	var parsed struct {
		Usage *struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal([]byte(payload), &parsed) == nil && parsed.Usage != nil {
		return parsed.Usage.PromptTokens, parsed.Usage.CompletionTokens, true
	}
	return 0, 0, false
}

// ─────────────────────────── 错误类型 ───────────────────────────

// HTTPError 表示上游返回了非 200。
type HTTPError struct {
	Code int
	Body []byte
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("upstream http %d: %s", e.Code, truncate(e.Body, 300))
}

// AsHTTPError 判断 err 是否为上游 HTTP 错误，便于调用方透传真实状态码与响应体。
func AsHTTPError(err error) (*HTTPError, bool) {
	var he *HTTPError
	if errors.As(err, &he) {
		return he, true
	}
	return nil, false
}

func wrapNetErr(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return errors.New("upstream timeout")
	}
	return err
}

func truncate(b []byte, n int) string {
	s := string(b)
	if len(s) > n {
		return s[:n]
	}
	return s
}
