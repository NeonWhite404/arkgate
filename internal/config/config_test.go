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
