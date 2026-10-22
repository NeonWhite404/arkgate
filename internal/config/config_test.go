package config

import (
	"testing"
	"time"
)

// TestTimeoutsAtomicSetGet 锁定运行时超时的读写语义：可热改、负值归零（关闭）。
func TestTimeoutsAtomicSetGet(t *testing.T) {
	var to Timeouts
	if to.Request() != 0 || to.FirstToken() != 0 {
		t.Fatalf("zero value must be 0/0")
	}
	to.SetRequest(90 * time.Second)
	to.SetFirstToken(7 * time.Second)
	if to.Request() != 90*time.Second || to.FirstToken() != 7*time.Second {
		t.Fatalf("set/get mismatch: %v %v", to.Request(), to.FirstToken())
	}
	// 热改：后续读立即拿到新值。
	to.SetRequest(5 * time.Second)
	if to.Request() != 5*time.Second {
		t.Fatalf("hot update failed: %v", to.Request())
	}
	// 负值视为关闭（0），不出现负超时导致请求立即失败。
	to.SetRequest(-3 * time.Second)
	to.SetFirstToken(-1)
	if to.Request() != 0 || to.FirstToken() != 0 {
		t.Fatalf("negative must clamp to 0: %v %v", to.Request(), to.FirstToken())
	}
}

// TestLoadTimeoutDefaults 锁定环境变量口径：未设置用内置默认，
// 显式 0 表示关闭（getDurationSec 对非正值返回 0）。
func TestLoadTimeoutDefaults(t *testing.T) {
	t.Setenv("ARKGATE_REQUEST_TIMEOUT", "")
	t.Setenv("ARKGATE_FIRST_TOKEN_TIMEOUT", "")
	c := Load()
	if c.Timeouts.Request() != DefaultRequestTimeout {
		t.Fatalf("default request timeout = %v", c.Timeouts.Request())
	}
	if c.Timeouts.FirstToken() != DefaultFirstTokenTimeout {
		t.Fatalf("default first-token timeout = %v", c.Timeouts.FirstToken())
	}

	t.Setenv("ARKGATE_REQUEST_TIMEOUT", "45")
	t.Setenv("ARKGATE_FIRST_TOKEN_TIMEOUT", "0")
	c = Load()
	if c.Timeouts.Request() != 45*time.Second {
		t.Fatalf("env request timeout = %v", c.Timeouts.Request())
	}
	if c.Timeouts.FirstToken() != 0 {
		t.Fatalf("explicit 0 must disable first-token timeout, got %v", c.Timeouts.FirstToken())
	}
}

// TestRetriesSetGet 锁定跨接入点重试次数的读写语义：可热改、
// 非法值（负数/超硬上限）回落内置默认而不是静默接受。
func TestRetriesSetGet(t *testing.T) {
	var r Retries
	r.SetMax(3)
	if r.Max() != 3 {
		t.Fatalf("set/get mismatch: %d", r.Max())
	}
	// 热改：后续读立即拿到新值（网关热路径并发读，原子存取）。
	r.SetMax(0)
	if r.Max() != 0 {
		t.Fatalf("hot update to 0 failed: %d", r.Max())
	}
	// 0 是合法值（只打一发不重试），不得被回落。
	r.SetMax(MaxRetriesHardLimit)
	if r.Max() != MaxRetriesHardLimit {
		t.Fatalf("hard limit must be accepted: %d", r.Max())
	}
	// 超上限与负值回落默认：环境变量/手滑输入不该让网关进入
	// 「无限重试」或「意外零重试」状态。
	r.SetMax(MaxRetriesHardLimit + 1)
	if r.Max() != DefaultMaxRetries {
		t.Fatalf("over-limit must fall back to default: %d", r.Max())
	}
	r.SetMax(-2)
	if r.Max() != DefaultMaxRetries {
		t.Fatalf("negative must fall back to default: %d", r.Max())
	}
}

// TestLoadRetriesEnv 锁定环境变量口径：未设置用内置默认，
// 合法值透传，非法值（非数字/负数）回落默认。
func TestLoadRetriesEnv(t *testing.T) {
	t.Setenv("ARKGATE_MAX_RETRIES", "")
	c := Load()
	if c.Retries.Max() != DefaultMaxRetries {
		t.Fatalf("default retries = %d", c.Retries.Max())
	}

	t.Setenv("ARKGATE_MAX_RETRIES", "7")
	c = Load()
	if c.Retries.Max() != 7 {
		t.Fatalf("env retries = %d", c.Retries.Max())
	}

	// 0 = 关闭重试，是合法语义。
	t.Setenv("ARKGATE_MAX_RETRIES", "0")
	c = Load()
	if c.Retries.Max() != 0 {
		t.Fatalf("explicit 0 must disable retries, got %d", c.Retries.Max())
	}

	// 非法输入按未设置处理。
	t.Setenv("ARKGATE_MAX_RETRIES", "abc")
	c = Load()
	if c.Retries.Max() != DefaultMaxRetries {
		t.Fatalf("invalid env must fall back to default: %d", c.Retries.Max())
	}
}
