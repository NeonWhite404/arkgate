package gateway

import (
	"arkgate/internal/model"
	"testing"
)

// TestResolveUsagePrefersUpstream 锁定硬约束：上游给了真实值就绝不估算。
//
// 这是整个兜底机制的边界——一旦估算值能覆盖真实值，账单与命中率就都不可信了，
// 比「记 0」严重得多（记 0 是漏收，估算是凭空收）。
func TestResolveUsagePrefersUpstream(t *testing.T) {
	cases := []struct {
		name    string
		pt, ct  int64
		est     int64
		wantPt  int64
		wantCt  int64
		wantSrc int
	}{
		{"上游给了完整 usage", 1200, 340, 5000, 1200, 340, model.UsageSourceUpstream},
		{"上游只给了输入", 1200, 0, 5000, 1200, 0, model.UsageSourceUpstream},
		{"上游只给了输出", 0, 340, 5000, 0, 340, model.UsageSourceUpstream},
		{"上游什么都没给，有估算值", 0, 0, 5000, 5000, 0, model.UsageSourceEstimated},
		{"上游什么都没给，也没有估算值", 0, 0, 0, 0, 0, model.UsageSourceUnknown},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			meta := reqMeta{estPrompt: c.est}
			pt, ct, src := resolveUsage(c.pt, c.ct, meta)
			if pt != c.wantPt || ct != c.wantCt || src != c.wantSrc {
				t.Fatalf("pt=%d ct=%d src=%d, want pt=%d ct=%d src=%d",
					pt, ct, src, c.wantPt, c.wantCt, c.wantSrc)
			}
		})
	}
}

// TestResolveUsageNeverInventsOutput 输出侧没有估算值时不得凭空造数：
// 上游没吐内容（或内容为空串）时，输出 token 必须保持 0，只兜输入侧。
func TestResolveUsageNeverInventsOutput(t *testing.T) {
	pt, ct, src := resolveUsage(0, 0, reqMeta{estPrompt: 900})
	if pt != 900 {
		t.Fatalf("输入侧应兜底为 900, got %d", pt)
	}
	if ct != 0 {
		t.Fatalf("输出侧无依据时必须为 0, got %d", ct)
	}
	if src != model.UsageSourceEstimated {
		t.Fatalf("来源应为估算, got %d", src)
	}
}
