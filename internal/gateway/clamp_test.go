package gateway

import (
	"encoding/json"
	"testing"
)

// TestClampOutput 锁定输出上限裁剪：只动超限的数值字段，其余字节原样保留；
// 未超限/非数值/非法 JSON 一律原样透传。
func TestClampOutput(t *testing.T) {
	body := []byte(`{"model":"m","max_tokens":999999,"max_completion_tokens":500,"stream":true,"seed":42}`)
	out, changed := clampOutput(body, chatOutputKeys, 16384)
	if !changed {
		t.Fatalf("expected clamp to apply")
	}
	var v map[string]any
	if err := json.Unmarshal(out, &v); err != nil {
		t.Fatalf("re-marshal broken: %v", err)
	}
	if v["max_tokens"].(float64) != 16384 {
		t.Fatalf("max_tokens = %v, want 16384", v["max_tokens"])
	}
	if v["max_completion_tokens"].(float64) != 500 {
		t.Fatalf("max_completion_tokens must stay: %v", v["max_completion_tokens"])
	}
	if v["seed"].(float64) != 42 || v["stream"].(bool) != true {
		t.Fatalf("unrelated fields must survive: %v", v)
	}

	// 未超限：字节透传。
	if out, changed = clampOutput(body, chatOutputKeys, 1000000); changed || string(out) != string(body) {
		t.Fatalf("below limit must pass through unchanged")
	}

	// 非数值字段不碰。
	bad := []byte(`{"model":"m","max_tokens":"many"}`)
	if out, changed = clampOutput(bad, chatOutputKeys, 10); changed {
		t.Fatalf("non-numeric field must not be touched")
	}

	// 非法 JSON：原样返回。
	junk := []byte("not-json")
	if out, changed = clampOutput(junk, chatOutputKeys, 10); changed || string(out) != "not-json" {
		t.Fatalf("invalid json must pass through")
	}

	// responses 字段名。
	rb := []byte(`{"model":"m","max_output_tokens":999999}`)
	out, changed = clampOutput(rb, responsesOutputKeys, 4096)
	if !changed {
		t.Fatalf("responses clamp expected")
	}
	var rv map[string]any
	if err := json.Unmarshal(out, &rv); err != nil {
		t.Fatalf("responses re-marshal broken: %v", err)
	}
	if rv["max_output_tokens"].(float64) != 4096 {
		t.Fatalf("max_output_tokens = %v, want 4096", rv["max_output_tokens"])
	}
}
