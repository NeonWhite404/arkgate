package model

import "testing"

// TestQuotaWeightedVsRawAreDifferentMetrics 锁定「加权配额」与「原始 token」
// 是两个口径，且必须能各自被正确算出。
//
// 背景（实测复现的真实困惑）：某子 Key 限额 100M（缓存读倍率 0.1x），
// 界面同时显示「Tokens 117M」和「缓存读取 112M」，看起来超限了。
// 实际上：
//   - 117M 是上游原始 total_tokens（含 112M 缓存读），用于计费/展示；
//   - 限额判定用的是加权配额计数 ≈ 112M×0.1 + 余量 ≈ 12M，远未超限。
//
// 两个口径都对，把原始量摆在「限额」旁边才产生误导。
//
// 这条测试的价值：如果有人把限额判定改成用原始 total_tokens（或把展示改成
// 加权值），这里立刻会失败——两个数字的差异是本测试的核心断言。
func TestQuotaWeightedVsRawAreDifferentMetrics(t *testing.T) {
	rule := QuotaRule{Period: QuotaPeriodDay, Weight: true,
		ReadPermille: 100, WritePermille: 1250} // 0.1x 读 / 1.25x 写

	// 典型长会话形态：大量缓存读 + 少量新输入 + 少量输出。
	const (
		cacheRead  = 118_800_000
		plainInput = 1_200_000
		output     = 1_200
		prompt     = plainInput + cacheRead // 上游 prompt_tokens 含缓存
	)
	rawTotal := int64(prompt + output) // 与上游 total_tokens 同口径

	weighted := QuotaCount(rule, prompt, output, cacheRead, 0)

	// ① 原始量必须显著大于加权量（缓存读只按 0.1 计）。
	if rawTotal <= weighted {
		t.Fatalf("原始量 %d 应远大于加权量 %d", rawTotal, weighted)
	}
	// ② 加权量必须远低于 100M 限额——这是「不该被拦」的量化依据。
	if weighted >= 100_000_000 {
		t.Fatalf("加权配额 %d 不应达到 100M 限额（说明倍率没生效）", weighted)
	}
	// ③ 精确值：plainInput + output + round(118.8M×0.1) = 1.2M + 1200 + 11.88M
	want := int64(plainInput + output + 11_880_000)
	if weighted != want {
		t.Fatalf("加权配额 = %d, want %d", weighted, want)
	}
	// ④ 原始量本身必须超 100M（否则复现不出用户的困惑场景）。
	if rawTotal <= 100_000_000 {
		t.Fatalf("用例应构造出「原始超限」的形态, got %d", rawTotal)
	}
}

// TestQuotaUnweightedUsesRawTotal 未启用倍率时，配额计数就是原始口径。
//
// 这条是「升级不得改变既有子 Key 额度」的语义基线：老 Key 没有倍率配置，
// 其额度消耗必须与引入加权之前逐字一致（否则等于静默放宽/收紧了配额）。
func TestQuotaUnweightedUsesRawTotal(t *testing.T) {
	rule := QuotaRule{Period: QuotaPeriodDay} // Weight=false
	const (
		prompt = 118_800_000 + 1_200_000
		output = 1_200
	)
	got := QuotaCount(rule, prompt, output, 118_800_000, 0)
	if got != int64(prompt+output) {
		t.Fatalf("未启用倍率时应返回原始口径 %d, got %d", int64(prompt+output), got)
	}
}
