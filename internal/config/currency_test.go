package config

import "testing"

// 币种是**展示层**参数：单价与成本始终以美元存储，这里只做换算。
// 这组测试锁定「零值安全」与「0 = 不换算」两条语义。
func TestCurrencyZeroValueSafe(t *testing.T) {
	var c Currency
	// 零值必须可用：rate=0 表示不换算，符号回落 $，代码回落 USD。
	if got := c.Rate(); got != 0 {
		t.Fatalf("零值 rate 应为 0, got %v", got)
	}
	if got := c.Symbol(); got != "$" {
		t.Fatalf("零值 symbol 应回落 $, got %q", got)
	}
	if got := c.Code(); got != "USD" {
		t.Fatalf("零值 code 应回落 USD, got %q", got)
	}
	// rate=0 时 Convert 必须原值返回，而不是乘以 0（否则所有金额变 0）。
	if got := c.Convert(12.5); got != 12.5 {
		t.Fatalf("rate=0 时 Convert 应原值返回, got %v", got)
	}
}

func TestCurrencyConvert(t *testing.T) {
	var c Currency
	c.Set(7.2, "¥", "CNY")
	if got := c.Convert(1); got != 7.2 {
		t.Fatalf("1 美元应换算为 7.2, got %v", got)
	}
	// 用容差而非 ==：浮点乘法必然有末位误差（0.002955*7.2 = 0.021276000000000003），
	// 断言精确相等会变成「测试浮点实现」而不是「测试换算逻辑」。
	if got := c.Convert(0.002955); diff(got, 0.021276) > 1e-12 {
		t.Fatalf("换算结果不对: %v", got)
	}
	// 换算必须线性（否则分项之和 != 总额，界面会出现对不上账）。
	a, b, whole := c.Convert(0.3), c.Convert(0.7), c.Convert(1.0)
	if d := diff(a+b, whole); d > 1e-12 {
		t.Fatalf("换算应线性: 0.3+0.7=%v != 1.0=%v (差 %v)", a+b, whole, d)
	}
}

func TestCurrencySetNormalizes(t *testing.T) {
	var c Currency
	// 负汇率按 0（不换算）处理，避免出现负数金额。
	c.Set(-1, "¥", "CNY")
	if got := c.Rate(); got != 0 {
		t.Fatalf("负汇率应归零, got %v", got)
	}
	if got := c.Convert(5); got != 5 {
		t.Fatalf("归零后应原值返回, got %v", got)
	}
	// 符号/代码为空时回落默认值。
	c.Set(7.2, "", "")
	if got := c.Symbol(); got != "$" {
		t.Fatalf("空符号应回落 $, got %q", got)
	}
	if got := c.Code(); got != "USD" {
		t.Fatalf("空代码应回落 USD, got %q", got)
	}
}

// TestCurrencyHotReload 热改汇率必须立即生效（管理端不重启网关）。
func TestCurrencyHotReload(t *testing.T) {
	var c Currency
	c.Set(7.2, "¥", "CNY")
	if got := c.Convert(1); got != 7.2 {
		t.Fatalf("初始换算错误: %v", got)
	}
	c.Set(7.3, "¥", "CNY")
	if got := c.Convert(1); got != 7.3 {
		t.Fatalf("热改后应生效: %v", got)
	}
	// 改回 0 = 停止换算。
	c.Set(0, "$", "USD")
	if got := c.Convert(1); got != 1 {
		t.Fatalf("rate 改回 0 后应原值返回: %v", got)
	}
}

// TestCurrencyEnvDefaults 环境变量只在未设置 DB 值时生效（优先级由 admin 保证）。
func TestCurrencyEnvDefaults(t *testing.T) {
	t.Setenv("ARKGATE_CURRENCY_RATE", "7.2")
	t.Setenv("ARKGATE_CURRENCY_SYMBOL", "¥")
	t.Setenv("ARKGATE_CURRENCY_CODE", "cny")
	c := Load()
	if got := c.Currency.Rate(); got != 7.2 {
		t.Fatalf("环境变量汇率未生效: %v", got)
	}
	if got := c.Currency.Symbol(); got != "¥" {
		t.Fatalf("环境变量符号未生效: %q", got)
	}
	// getFloat 对非法值回落默认（不把配置错误变成 0 汇率而静默按美元显示）。
	t.Setenv("ARKGATE_CURRENCY_RATE", "not-a-number")
	c2 := Load()
	if got := c2.Currency.Rate(); got != 0 {
		t.Fatalf("非法汇率应回落默认 0, got %v", got)
	}
}

// diff 浮点比较辅助。
func diff(a, b float64) float64 {
	d := a - b
	if d < 0 {
		return -d
	}
	return d
}
