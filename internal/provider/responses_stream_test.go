package provider

import (
	"bufio"
	"bytes"
	"errors"
	"strings"
	"testing"
)

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

// newTestStream 构造一条以 body 为上游内容的 responses 流（首行为空，全部走 rdr）。
func newTestStream(body string) *Stream {
	return &Stream{rdr: bufio.NewReader(strings.NewReader(body)),
		sniff: responsesUsageFromEvent, terminal: responsesTerminal}
}

// TestStreamPumpCompletesTerminalFrame 锁定：终止事件所在帧必须写完整（含帧尾
// 空行）才收尾——SSE 事件以空行结尾，缺帧尾客户端会按规范丢弃 pending event，
// completed/failed 就送不到下游；终止后的后续事件不得再转发。
func TestStreamPumpCompletesTerminalFrame(t *testing.T) {
	body := "event: response.created\ndata: {\"type\":\"response.created\"}\n\n" +
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":3,\"output_tokens\":2}}}\n\n" +
		"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"x\"}\n\n" +
		"event: response.output_text.done\ndata: {\"type\":\"response.output_text.done\"}\n\n"
	st := newTestStream(body)
	var buf bytes.Buffer
	pt, ct, err := st.Pump(&buf)
	if err != nil {
		t.Fatalf("pump: %v", err)
	}
	if pt != 3 || ct != 2 {
		t.Fatalf("usage = %d/%d, want 3/2", pt, ct)
	}
	out := buf.String()
	if !strings.Contains(out, "response.completed") {
		t.Fatalf("completed 未转发: %q", out)
	}
	// 帧尾空行必须写出（输出以空行结尾），后续 delta/done 不得出现。
	if !strings.HasSuffix(out, "\n\n") {
		t.Fatalf("终止帧缺帧尾空行: %q", out)
	}
	if strings.Contains(out, "output_text.delta") || strings.Contains(out, "output_text.done") {
		t.Fatalf("终止后仍转发后续事件: %q", out)
	}
}

// TestStreamPumpFailedTerminal 锁定：response.failed 记失败（ErrStreamFailed）、
// 用量照常提取、失败帧本身已完整转发、之后的垃圾数据不再转发。
func TestStreamPumpFailedTerminal(t *testing.T) {
	body := "event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"usage\":{\"input_tokens\":5,\"output_tokens\":1}}}\n\n" +
		"data: {\"type\":\"should_not_forward\"}\n\n"
	st := newTestStream(body)
	var buf bytes.Buffer
	pt, ct, err := st.Pump(&buf)
	if !errors.Is(err, ErrStreamFailed) {
		t.Fatalf("err = %v, want ErrStreamFailed", err)
	}
	if pt != 5 || ct != 1 {
		t.Fatalf("usage = %d/%d, want 5/1", pt, ct)
	}
	out := buf.String()
	if !strings.Contains(out, "response.failed") || !strings.HasSuffix(out, "\n\n") {
		t.Fatalf("失败帧未完整转发: %q", out)
	}
	if strings.Contains(out, "should_not_forward") {
		t.Fatalf("失败终止后仍转发: %q", out)
	}
}
