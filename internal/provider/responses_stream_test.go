package provider

import "testing"

// TestResponsesUsageFromEvent 锁定用量提取覆盖三种终止事件（completed/
// incomplete/failed），非终止事件与缺 usage 的终止不提取。
func TestResponsesUsageFromEvent(t *testing.T) {
	cases := []struct {
		e      string
		pt, ct int64
		ok     bool
	}{
		{`{"type":"response.completed","response":{"usage":{"input_tokens":10,"output_tokens":5}}}`, 10, 5, true},
		{`{"type":"response.incomplete","response":{"usage":{"input_tokens":10,"output_tokens":3}}}`, 10, 3, true},
		{`{"type":"response.failed","response":{"usage":{"input_tokens":10,"output_tokens":0}}}`, 10, 0, true},
		{`{"type":"response.output_text.delta","response":{"usage":{"input_tokens":1}}}`, 0, 0, false},
		{`{"type":"response.completed","response":{}}`, 0, 0, false},
	}
	for _, c := range cases {
		pt, ct, ok := responsesUsageFromEvent([]byte(c.e))
		if ok != c.ok || pt != c.pt || ct != c.ct {
			t.Fatalf("%s → pt=%d ct=%d ok=%v, want %d/%d/%v", c.e, pt, ct, ok, c.pt, c.ct, c.ok)
		}
	}
}

// TestResponsesTerminal 锁定终止检测：completed/incomplete 正常收尾，
// failed/error 失败收尾，其余不终止。
func TestResponsesTerminal(t *testing.T) {
	cases := []struct {
		e          string
		stop, fail bool
	}{
		{`{"type":"response.completed"}`, true, false},
		{`{"type":"response.incomplete"}`, true, false},
		{`{"type":"response.failed"}`, true, true},
		{`{"type":"error","code":"server_error"}`, true, true},
		{`{"type":"response.output_text.delta"}`, false, false},
		{`{"type":"response.created"}`, false, false},
		{`not json`, false, false},
	}
	for _, c := range cases {
		stop, fail := responsesTerminal([]byte(c.e))
		if stop != c.stop || fail != c.fail {
			t.Fatalf("%s → stop=%v fail=%v, want %v/%v", c.e, stop, fail, c.stop, c.fail)
		}
	}
}
