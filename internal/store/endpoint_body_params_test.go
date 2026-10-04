package store

import (
	"encoding/json"
	"testing"

	"arkgate/internal/model"
)

// newTestStore 建一个临时库（Windows 上必须显式关闭，否则 TempDir 清理失败）。
func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// TestEndpointBodyParamsRoundTrip 默认请求体参数的回环读写。
//
// 重点在**保真**：值里可能有嵌套对象/大整数，经 any 往返会丢精度。
// 存储层用 json.RawMessage 存取，这里逐字节比对确认。
func TestEndpointBodyParamsRoundTrip(t *testing.T) {
	s := newTestStore(t)

	ep := &model.Endpoint{
		ID: "ep-bp", AccountID: "acc-1", Model: "echo", EP: "echo",
		Enabled: true, Weight: 1,
		DefaultBodyParams: map[string]json.RawMessage{
			"persona": json.RawMessage(`"Virginia Woolf"`),
			"nested":  json.RawMessage(`{"a":[1,true,null,"s"],"b":{"c":0.1}}`),
			"big":     json.RawMessage(`12345678901234567890`),
			"zero":    json.RawMessage(`0`),
			"nul":     json.RawMessage(`null`),
		},
	}
	if err := s.UpsertEndpoint(ep); err != nil {
		t.Fatalf("写入: %v", err)
	}

	got, err := s.GetEndpoint("ep-bp")
	if err != nil {
		t.Fatalf("读取: %v", err)
	}
	if len(got.DefaultBodyParams) != 5 {
		t.Fatalf("字段数应为 5, got %d: %+v", len(got.DefaultBodyParams), got.DefaultBodyParams)
	}
	// 逐字节比对（不能反序列化成 any 再比——那样自己就先丢精度了）。
	for k, want := range ep.DefaultBodyParams {
		if string(got.DefaultBodyParams[k]) != string(want) {
			t.Errorf("字段 %s 往返变形: got %s, want %s", k, got.DefaultBodyParams[k], want)
		}
	}
	// `0` 不能被当成「未设置」而丢掉（0 是合法值）。
	if string(got.DefaultBodyParams["zero"]) != "0" {
		t.Errorf("零值应保留: %s", got.DefaultBodyParams["zero"])
	}
	if string(got.DefaultBodyParams["nul"]) != "null" {
		t.Errorf("null 值应保留: %s", got.DefaultBodyParams["nul"])
	}
}

// TestEndpointBodyParamsEmptyConvention nil 与空 map 都落库为 "{}"，
// 读回来是空 map（不是 nil 也不是 "null"）。
//
// 若落成 "null"，scanEndpoint 反序列化会把 map 置为 nil；虽然行为上等价，
// 但会让「区分未配置与显式清空」在将来变得不可能，也与 request_headers 的
// 既有约定不一致。
func TestEndpointBodyParamsEmptyConvention(t *testing.T) {
	s := newTestStore(t)
	for _, e := range []*model.Endpoint{
		{ID: "e-nil", AccountID: "a", Model: "m1", EP: "p1", Enabled: true},
		{ID: "e-empty", AccountID: "a", Model: "m2", EP: "p2", Enabled: true,
			DefaultBodyParams: map[string]json.RawMessage{}},
	} {
		if err := s.UpsertEndpoint(e); err != nil {
			t.Fatalf("写入 %s: %v", e.ID, err)
		}
		got, err := s.GetEndpoint(e.ID)
		if err != nil {
			t.Fatalf("读取 %s: %v", e.ID, err)
		}
		if len(got.DefaultBodyParams) != 0 {
			t.Errorf("%s 应为空配置, got %+v", e.ID, got.DefaultBodyParams)
		}
	}
}

// TestEndpointBodyParamsUpdateAndClear 编辑覆盖与显式清空。
//
// 覆盖路径（updateEndpointByID）与插入路径是两段独立 SQL，都要写到该列——
// 只在插入路径写会让「编辑接入点后默认参数丢失」这类 bug 藏起来。
func TestEndpointBodyParamsUpdateAndClear(t *testing.T) {
	s := newTestStore(t)
	ep := &model.Endpoint{
		ID: "ep-u", AccountID: "a", Model: "m", EP: "p", Enabled: true,
		DefaultBodyParams: map[string]json.RawMessage{"persona": json.RawMessage(`"A"`)},
	}
	if err := s.UpsertEndpoint(ep); err != nil {
		t.Fatalf("写入: %v", err)
	}

	// ① 改值（走按 id 覆盖路径）。
	ep.DefaultBodyParams = map[string]json.RawMessage{
		"persona": json.RawMessage(`"B"`),
		"extra":   json.RawMessage(`1`),
	}
	if err := s.UpsertEndpoint(ep); err != nil {
		t.Fatalf("更新: %v", err)
	}
	got, _ := s.GetEndpoint("ep-u")
	if string(got.DefaultBodyParams["persona"]) != `"B"` || len(got.DefaultBodyParams) != 2 {
		t.Fatalf("更新未生效: %+v", got.DefaultBodyParams)
	}

	// ② 清空（nil → "{}"）。
	ep.DefaultBodyParams = nil
	if err := s.UpsertEndpoint(ep); err != nil {
		t.Fatalf("清空: %v", err)
	}
	got, _ = s.GetEndpoint("ep-u")
	if len(got.DefaultBodyParams) != 0 {
		t.Fatalf("清空未生效: %+v", got.DefaultBodyParams)
	}
}

// TestEndpointBodyParamsSurvivesSiblings 同账号同模型的多接入点各自独立。
//
// 叶节点级配置的核心不变量：一个接入点的默认参数不能串到兄弟接入点
//（否则会给不支持的版本注入参数，把本来好的请求打坏）。
func TestEndpointBodyParamsSurvivesSiblings(t *testing.T) {
	s := newTestStore(t)
	mk := func(id, ep string, params map[string]json.RawMessage) *model.Endpoint {
		return &model.Endpoint{ID: id, AccountID: "acc", Model: "m", EP: ep,
			Enabled: true, DefaultBodyParams: params}
	}
	if err := s.UpsertEndpoint(mk("s1", "v1", map[string]json.RawMessage{
		"persona": json.RawMessage(`"one"`)})); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertEndpoint(mk("s2", "v2", nil)); err != nil {
		t.Fatal(err)
	}
	got1, _ := s.GetEndpoint("s1")
	got2, _ := s.GetEndpoint("s2")
	if string(got1.DefaultBodyParams["persona"]) != `"one"` {
		t.Fatalf("s1 配置丢失: %+v", got1.DefaultBodyParams)
	}
	if len(got2.DefaultBodyParams) != 0 {
		t.Fatalf("s2 不应继承 s1 的参数: %+v", got2.DefaultBodyParams)
	}
	// ListEndpoints（全表加载路径）也必须带上该列。
	all, err := s.ListEndpoints()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range all {
		if e.ID == "s1" {
			found = true
			if string(e.DefaultBodyParams["persona"]) != `"one"` {
				t.Fatalf("ListEndpoints 丢列: %+v", e.DefaultBodyParams)
			}
		}
	}
	if !found {
		t.Fatal("ListEndpoints 未返回 s1")
	}
}
