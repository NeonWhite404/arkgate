package admin

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestRawFieldOfKeepsBigInt 大整数必须逐字节保真。
//
// 这是实测抓到的真实缺陷：handler 为做「部分更新」把请求体解成
// map[string]any，数字变成 float64，12345678901234567890 被舍入成
// 1.2345678901234567e+19 并**静默落库**——管理端看似保存成功，上游实际
// 收到另一个数值。rawFieldOf 就是为了绕开这条 any 路径。
func TestRawFieldOfKeepsBigInt(t *testing.T) {
	body := []byte(`{"weight":5,"default_body_params":{"big":12345678901234567890,"f":0.1}}`)
	raw, ok := rawFieldOf(body, "default_body_params")
	if !ok {
		t.Fatal("字段未找到")
	}
	if !bytes.Contains(raw, []byte("12345678901234567890")) {
		t.Fatalf("大整数被舍入: %s", raw)
	}
	// 对照：走 any 路径就会丢（若哪天 Go/JSON 行为变了，这条会提醒我们
	// 「可以不用绕路了」，而不是让绕过逻辑悄悄变成无用代码）。
	var asAny map[string]any
	if err := json.Unmarshal(body, &asAny); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(asAny["default_body_params"])
	if bytes.Contains(b, []byte("12345678901234567890")) {
		t.Log("注意：any 路径现在也不丢精度了，rawFieldOf 或许可以简化")
	}
	// 字段不存在 / 非法 JSON 的处理。
	if _, ok := rawFieldOf([]byte(`{"a":1}`), "default_body_params"); ok {
		t.Error("字段不存在时应返回 false")
	}
	if _, ok := rawFieldOf([]byte(`{bad`), "default_body_params"); ok {
		t.Error("非法 JSON 应返回 false")
	}
}

// TestReadAllBodyThenUnmarshalTwice 同一个请求体要能解两次
//（① any 做部分更新、② RawMessage 保精度）。
//
// 这条锁的是顺序陷阱：先 decode（读干 r.Body）再读原始字节会拿到空的，
// 表现为「保存时报非法 JSON / 配置被静默清空」。修复前实测就是这个症状。
func TestReadAllBodyThenUnmarshalTwice(t *testing.T) {
	payload := `{"weight":7,"default_body_params":{"persona":"Woolf","big":12345678901234567890}}`
	r := httptest.NewRequest(http.MethodPut, "/api/endpoints/x", bytes.NewReader([]byte(payload)))

	body, err := readAllBody(r)
	if err != nil {
		t.Fatalf("readAllBody: %v", err)
	}
	// ① any 路径
	var probe map[string]any
	if err := json.Unmarshal(body, &probe); err != nil {
		t.Fatalf("any 解码: %v", err)
	}
	if probe["weight"].(float64) != 7 {
		t.Fatalf("weight 丢失: %v", probe["weight"])
	}
	// ② RawMessage 路径（同一份字节，仍可解）
	raw, ok := rawFieldOf(body, "default_body_params")
	if !ok {
		t.Fatal("RawMessage 路径取不到字段")
	}
	if !bytes.Contains(raw, []byte("12345678901234567890")) {
		t.Fatalf("精度丢失: %s", raw)
	}
	var parsed map[string]json.RawMessage
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("解字段: %v", err)
	}
	if string(parsed["persona"]) != `"Woolf"` {
		t.Fatalf("persona 值错误: %s", parsed["persona"])
	}
}
