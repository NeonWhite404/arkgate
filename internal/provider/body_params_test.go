package provider

import (
	"encoding/json"
	"testing"
)

// TestPrepareBodyInjectsDefaultsWhenAbsent 下游没发的字段由接入点默认参数补上。
//
// 场景来源：某上游方言要求额外参数（例如 echo 模型必须带 persona），
// 而下游是机器人框架——只能发 model/messages 这类标准字段，没法用
// extra_body 传自定义参数，请求直接被上游拒。
func TestPrepareBodyInjectsDefaultsWhenAbsent(t *testing.T) {
	defaults := map[string]json.RawMessage{
		"persona": json.RawMessage(`"Virginia Woolf"`),
	}
	// 下游只发最基础的两个字段（机器人框架的典型形态）。
	out, err := prepareBody([]byte(`{"model":"echo","input":"hi"}`), "ep-echo", defaults)
	if err != nil {
		t.Fatalf("prepareBody: %v", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("上游将收到非法 JSON: %s", out)
	}
	if string(m["persona"]) != `"Virginia Woolf"` {
		t.Fatalf("默认参数未注入: %s", out)
	}
	if string(m["model"]) != `"ep-echo"` {
		t.Fatalf("model 应为上游标识: %s", m["model"])
	}
	if string(m["input"]) != `"hi"` {
		t.Fatalf("下游字段丢失: %s", out)
	}
}

// TestPrepareBodyDownstreamWins 下游显式给了同名参数时**不覆盖**。
//
// 这是与「补上缺失字段」配套的另一半语义，缺了它默认参数会顶掉调用方的
// 显式意图（例如用户在会话里指定了另一个 persona）——比缺省值更糟，
// 因为请求成功但结果不是用户要的，极难排查。
func TestPrepareBodyDownstreamWins(t *testing.T) {
	defaults := map[string]json.RawMessage{
		"persona":     json.RawMessage(`"默认人设"`),
		"temperature": json.RawMessage(`0.1`),
	}
	out, err := prepareBody([]byte(`{"model":"m","input":"x","persona":"调用方人设"}`), "ep-1", defaults)
	if err != nil {
		t.Fatalf("prepareBody: %v", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	if string(m["persona"]) != `"调用方人设"` {
		t.Fatalf("下游显式值被覆盖了: %s", m["persona"])
	}
	// 下游没给 temperature，仍应补上默认。
	if string(m["temperature"]) != `0.1` {
		t.Fatalf("缺失字段应补默认: %s", m["temperature"])
	}
}

// TestPrepareBodyReservedKeysNeverInjected 保留键即使在 defaults 里也不得生效。
//
// 写入端（ValidateBodyParams）已拦一道，这里是第二道防线：
// 直接构造 defaults 绕过管理端也不该破坏路由/流式语义。
func TestPrepareBodyReservedKeysNeverInjected(t *testing.T) {
	defaults := map[string]json.RawMessage{
		"model":          json.RawMessage(`"劫持的模型"`),
		"stream":         json.RawMessage(`true`),
		"stream_options": json.RawMessage(`{"include_usage":false}`),
		"persona":        json.RawMessage(`"ok"`),
	}
	out, err := prepareBody([]byte(`{"model":"m","input":"x"}`), "ep-real", defaults)
	if err != nil {
		t.Fatalf("prepareBody: %v", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	if string(m["model"]) != `"ep-real"` {
		t.Fatalf("model 被默认参数劫持: %s", m["model"])
	}
	if string(m["stream"]) != "false" && string(m["stream"]) != "" {
		// 下游没发 stream，注入 true 会让下游拿到 SSE 而解析失败。
		if string(m["stream"]) == "true" {
			t.Fatalf("stream 被注入了: %s", out)
		}
	}
	if _, exists := m["stream_options"]; exists {
		t.Fatalf("stream_options 不应被注入: %s", out)
	}
	// 普通字段照常注入。
	if string(m["persona"]) != `"ok"` {
		t.Fatalf("普通字段应注入: %s", out)
	}
}

// TestPrepareBodyDefaultsFidelity 默认参数的复杂结构必须保真。
//
// RawMessage 的意义就在这里：嵌套对象/数组、大整数、小数都不能在往返中变形。
// 若把 defaults 存成 map[string]any，12345678901234567890 会变成
// 1.2345678901234567e+19（float64 精度丢失），上游可能因此报类型错误。
func TestPrepareBodyDefaultsFidelity(t *testing.T) {
	defaults := map[string]json.RawMessage{
		"nested": json.RawMessage(`{"a":[1,true,null,"s"],"b":{"c":0.1}}`),
		"big":    json.RawMessage(`12345678901234567890`),
		"flag":   json.RawMessage(`false`),
		"nul":    json.RawMessage(`null`),
	}
	out, err := prepareBody([]byte(`{"model":"m","input":"x"}`), "ep-1", defaults)
	if err != nil {
		t.Fatalf("prepareBody: %v", err)
	}
	// 用 string 比对而不是反序列化后比对：反序列化到 any 自己就会丢精度，
	// 那样测试就查不出精度问题了（初版差点这么写）。
	raw := string(out)
	for _, want := range []string{
		`"nested":{"a":[1,true,null,"s"],"b":{"c":0.1}}`,
		`"big":12345678901234567890`,
		`"flag":false`,
		`"nul":null`,
	} {
		if !contains(raw, want) {
			t.Errorf("默认参数在往返中变形，缺少 %s\n实际: %s", want, raw)
		}
	}
}

// TestPrepareBodyNilDefaultsUnchanged 没有配置默认参数时行为与从前完全一致。
// 这条是「加功能不改既有行为」的安全网。
func TestPrepareBodyNilDefaultsUnchanged(t *testing.T) {
	in := `{"model":"m","messages":[{"role":"user","content":"hi"}],"temperature":0.7}`
	out, err := prepareBody([]byte(in), "ep-9", nil)
	if err != nil {
		t.Fatalf("prepareBody: %v", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	if len(m) != 3 {
		t.Fatalf("字段数应仍为 3（model+messages+temperature）, got %d: %s", len(m), out)
	}
	if string(m["model"]) != `"ep-9"` || string(m["temperature"]) != "0.7" {
		t.Fatalf("既有透传行为被改变: %s", out)
	}
	// 空 map 与 nil 等价。
	out2, err := prepareBody([]byte(in), "ep-9", map[string]json.RawMessage{})
	if err != nil {
		t.Fatalf("prepareBody(空 map): %v", err)
	}
	if string(out2) != string(out) {
		t.Fatalf("空 map 与 nil 行为应一致:\n%s\n%s", out, out2)
	}
}

// TestPrepareStreamBodyInjectsDefaults 流式路径同样注入。
//
// 机器人框架经常用流式；只在非流式路径注入会造成「一样的配置，
// 流式失败非流式成功」这种最难查的现象。
func TestPrepareStreamBodyInjectsDefaults(t *testing.T) {
	defaults := map[string]json.RawMessage{"persona": json.RawMessage(`"Woolf"`)}
	out, err := prepareStreamBody([]byte(`{"model":"m","messages":[]}`), "ep-1", defaults)
	if err != nil {
		t.Fatalf("prepareStreamBody: %v", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	if string(m["persona"]) != `"Woolf"` {
		t.Fatalf("流式路径未注入默认参数: %s", out)
	}
	// 流式强制字段仍必须生效（注入不能破坏它们）。
	if string(m["stream"]) != "true" {
		t.Fatalf("stream 应为 true: %s", m["stream"])
	}
	var so map[string]any
	if err := json.Unmarshal(m["stream_options"], &so); err != nil {
		t.Fatalf("stream_options 非法: %s", m["stream_options"])
	}
	if so["include_usage"] != true {
		t.Fatalf("include_usage 应为 true: %s", m["stream_options"])
	}
}

// TestPrepareStreamBodyDefaultsCannotBreakStreaming 默认参数带 stream=false
// 也不能把流式请求变回非流式（保留键保护）。
func TestPrepareStreamBodyDefaultsCannotBreakStreaming(t *testing.T) {
	defaults := map[string]json.RawMessage{"stream": json.RawMessage(`false`)}
	out, err := prepareStreamBody([]byte(`{"model":"m","messages":[]}`), "ep-1", defaults)
	if err != nil {
		t.Fatalf("prepareStreamBody: %v", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	if string(m["stream"]) != "true" {
		t.Fatalf("默认参数破坏了流式强制: %s", out)
	}
}

// TestValidateBodyParams 写入端校验：保留键与非法值都要明确拒绝。
//
// 必须报错而不是静默忽略——静默忽略会让管理员以为配置生效了，
// 直到上游继续报错才发现（且不知道该查哪里）。
func TestValidateBodyParams(t *testing.T) {
	if err := ValidateBodyParams(nil); err != nil {
		t.Errorf("空配置应通过: %v", err)
	}
	if err := ValidateBodyParams(map[string]json.RawMessage{}); err != nil {
		t.Errorf("空 map 应通过: %v", err)
	}
	// 合法值：各种 JSON 形态都该接受（上游方言不可预测，不做值白名单）。
	ok := map[string]json.RawMessage{
		"persona": json.RawMessage(`"Woolf"`),
		"nested":  json.RawMessage(`{"a":[1,2]}`),
		"num":     json.RawMessage(`0.7`),
		"flag":    json.RawMessage(`true`),
		"nul":     json.RawMessage(`null`),
	}
	if err := ValidateBodyParams(ok); err != nil {
		t.Errorf("合法配置不应报错: %v", err)
	}
	// 保留键必须拒绝。
	for _, key := range []string{"model", "stream", "stream_options"} {
		err := ValidateBodyParams(map[string]json.RawMessage{key: json.RawMessage(`1`)})
		if err == nil {
			t.Errorf("保留键 %s 应被拒绝", key)
		}
	}
	// 空字段名 / 空值 / 非法 JSON。
	if err := ValidateBodyParams(map[string]json.RawMessage{" ": json.RawMessage(`1`)}); err == nil {
		t.Error("空字段名应被拒绝")
	}
	if err := ValidateBodyParams(map[string]json.RawMessage{"a": json.RawMessage("  ")}); err == nil {
		t.Error("空值应被拒绝")
	}
	if err := ValidateBodyParams(map[string]json.RawMessage{"a": json.RawMessage(`{bad`)}); err == nil {
		t.Error("非法 JSON 应被拒绝")
	}
}

func contains(haystack, needle string) bool {
	return len(needle) == 0 || (len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0)
}

func indexOf(h, n string) int {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return i
		}
	}
	return -1
}
