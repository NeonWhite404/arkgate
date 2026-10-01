package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"arkgate/internal/provider"
)

// TestWantsStream 锁定流式判定：字符串型 stream 必须仍被判为流式（"true"），
// 而不是因解码失败静默回落非流式。
//
// 回归背景：旧实现用 `struct{ Stream bool }`，字符串会让整个解码报错并被
// 当成 false，请求于是走非流式路径、把字符串 stream 原样透传给上游，触发
// "stream 必须是布尔值" / "cannot unmarshal string into ... type bool"。
func TestWantsStream(t *testing.T) {
	cases := []struct {
		body string
		want bool
	}{
		{`{"stream":true}`, true},
		{`{"stream":false}`, false},
		{`{"stream":"true"}`, true}, // ← 旧实现返回 false
		{`{"stream":"false"}`, false},
		{`{"stream":"TRUE"}`, true},
		{`{"stream":" true "}`, true},
		{`{"stream":"1"}`, true},
		{`{"stream":"0"}`, false},
		{`{"stream":"yes"}`, true},
		{`{"stream":"off"}`, false},
		{`{"stream":1}`, true},
		{`{"stream":0}`, false},
		{`{"stream":2}`, true},
		{`{"stream":"maybe"}`, false},
		{`{"stream":null}`, false},
		{`{"model":"m"}`, false},
		{`not json`, false},
		{``, false},
	}
	for _, c := range cases {
		if got := wantsStream([]byte(c.body)); got != c.want {
			t.Fatalf("wantsStream(%s) = %v, want %v", c.body, got, c.want)
		}
	}
}

// TestWantsStreamAgreesWithNormalize 锁定两个独立实现的判定口径一致：
// 归一后的请求体，wantsStream 的结果必须与「归一前判定」相同。
// 若两者口径漂移，会出现「判为流式但归一成 false」的自相矛盾分流。
func TestWantsStreamAgreesWithNormalize(t *testing.T) {
	bodies := []string{
		`{"model":"m","stream":true}`,
		`{"model":"m","stream":"true"}`,
		`{"model":"m","stream":"false"}`,
		`{"model":"m","stream":"yes"}`,
		`{"model":"m","stream":"0"}`,
		`{"model":"m","stream":1}`,
		`{"model":"m","stream":"maybe"}`,
		`{"model":"m"}`,
	}
	for _, body := range bodies {
		before := wantsStream([]byte(body))
		normalized, _ := provider.NormalizeStreamFlag([]byte(body))
		after := wantsStream(normalized)
		if before != after {
			t.Fatalf("口径漂移 for %s: 归一前=%v 归一后=%v (归一结果=%s)",
				body, before, after, normalized)
		}
	}
}

// TestNormalizedBodyIsBooleanForUpstream 端到端锁定：无论下游怎么写法，
// 经入口归一后，非流式路径发给上游的 stream 必须是布尔（可被 bool 反序列化）。
func TestNormalizedBodyIsBooleanForUpstream(t *testing.T) {
	bodies := []string{
		`{"model":"m","stream":"false","messages":[]}`,
		`{"model":"m","stream":"maybe","messages":[]}`,
		`{"model":"m","stream":null,"messages":[]}`,
		`{"model":"m","stream":{"a":1},"messages":[]}`,
	}
	for _, body := range bodies {
		normalized, _ := provider.NormalizeStreamFlag([]byte(body))
		// 非流式转发路径（prepareBody 的等价物：只换 model、其余字段保留）。
		var m map[string]json.RawMessage
		if err := json.Unmarshal(normalized, &m); err != nil {
			t.Fatalf("归一结果非法: %s", normalized)
		}
		m["model"] = json.RawMessage(`"ep-1"`)
		forwarded, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		var probe struct {
			Stream bool `json:"stream"`
		}
		if err := json.Unmarshal(forwarded, &probe); err != nil {
			t.Fatalf("上游仍会收到非法 stream（复现原 bug）: body=%s err=%v", body, err)
		}
	}
}

// TestTruthyJSON 直接锁定布尔判定助手对各类 JSON 标量的取值。
func TestTruthyJSON(t *testing.T) {
	cases := []struct {
		raw  string
		want bool
	}{
		{`true`, true}, {`false`, false},
		{`"true"`, true}, {`"false"`, false},
		{`"yes"`, true}, {`"no"`, false},
		{`"on"`, true}, {`"off"`, false},
		{`"enabled"`, true}, {`"disabled"`, false},
		{`1`, true}, {`0`, false}, {`1.0`, true}, {`0.0`, false},
		{`2`, true}, {`-1`, true},
		{`"maybe"`, false}, {`null`, false}, {`{}`, false}, {`[]`, false},
		{``, false},
	}
	for _, c := range cases {
		if got := truthyJSON([]byte(c.raw)); got != c.want {
			t.Fatalf("truthyJSON(%s) = %v, want %v", c.raw, got, c.want)
		}
	}
}

// TestCancelAndTypeErrorsAreClientSide 锁定 recordAttempt 的失败分类口径：
// 「下游断开」与「参数类型错误」都必须被判为 clientErr（不计端点熔断），
// 而真实上游 5xx 必须仍然计入熔断——否则熔断永不生效。
//
// 这里复刻 recordAttempt 的分类表达式，锁定它与 provider 层分类函数的一致性；
// 若分类口径变更（如 IsRequestFault 收窄 5xx 白名单），本测试会先失败。
func TestCancelAndTypeErrorsAreClientSide(t *testing.T) {
	cases := []struct {
		name            string
		err             error
		wantClientErr   bool // 不计熔断
		wantClientCance bool
	}{
		{"下游断开", context.Canceled, true, true},
		{"下游断开（包装）", fmt.Errorf("Post \"http://x\": %w", context.Canceled), true, true},
		{
			"Ark 类型错误 400",
			&provider.HTTPError{Code: 400, Body: []byte(`{"error":{"message":"stream 必须是布尔值"}}`)},
			true, false,
		},
		{
			"Go 反序列化失败 500",
			&provider.HTTPError{Code: 500, Body: []byte(`{"error":{"message":"json: cannot unmarshal string into Go struct field .stream of type bool"}}`)},
			true, false,
		},
		{
			"真实上游 500",
			&provider.HTTPError{Code: 500, Body: []byte(`{"error":{"message":"internal server error"}}`)},
			false, false,
		},
		{
			"上游 429 限流",
			&provider.HTTPError{Code: 429, Body: []byte(`{"error":{"message":"rate limit exceeded"}}`)},
			false, false,
		},
		{
			"首 token 超时",
			provider.ErrFirstToken,
			false, false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// 与 recordAttempt 中的表达式保持一致。
			clientErr := provider.IsRequestFault(c.err) || provider.IsClientCancel(c.err)
			cancelErr := provider.IsClientCancel(c.err)
			if clientErr != c.wantClientErr {
				t.Fatalf("clientErr = %v, want %v", clientErr, c.wantClientErr)
			}
			if cancelErr != c.wantClientCance {
				t.Fatalf("cancelErr = %v, want %v", cancelErr, c.wantClientCance)
			}
		})
	}
}

// TestUpstreamCtxFollowsClientCancellation 锁定上游 ctx 不被切断下游取消。
//
// 回归背景：曾用 context.WithoutCancel 让非流式请求「下游断开也把上游跑完」，
// 实测被断开的请求会持续持有叶节点并发槽位直到整体超时（默认 300s），下游
// 高频断开时会耗尽 max_concurrency 配额。此测试锁定该决定，防止回退。
func TestUpstreamCtxFollowsClientCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	r := (&http.Request{}).WithContext(ctx)

	cancel()
	select {
	case <-upstreamCtx(r).Done():
		// 期望：上游 ctx 随下游一起取消（槽位得以及时释放）。
	default:
		t.Fatal("upstreamCtx 必须随下游取消，否则断开的下游会长期占用并发槽位")
	}
}
