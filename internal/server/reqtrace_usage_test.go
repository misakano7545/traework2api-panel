package server

import (
	"testing"
	"time"

	"traework2api/internal/usage"
)

// TestReqTraceAddUsage 请求记录的用量累加口径。
//
// 抓的是「算错会让面板显示假数」的三件事：
//  1. 换号重试的多次尝试 token 相加（一条请求的记录 = 各次尝试之和）；
//  2. 上游没报的维度保持零值（面板显示「—」，不能变成 0）；
//  3. 积分只用收录了单价的模型估算，没收录的不编数（HasCredit=false）。
func TestReqTraceAddUsage(t *testing.T) {
	tr := &reqTrace{ID: "r1", Start: time.Now()}
	tr.addUsage("cn:glm-5.2", usage.Delta{
		PromptTokens: 1000, HasPromptTokens: true,
		CompletionTokens: 200, HasCompletion: true,
		TotalTokens: 1200, HasTotal: true,
		CacheHitTokens: 800, HasCacheHit: true,
	})
	if tr.PromptTokens != 1000 || tr.CompletionTokens != 200 || tr.TotalTokens != 1200 {
		t.Fatalf("首次尝试 token 记错: %+v", tr)
	}
	if !tr.HasCredit || tr.Credit <= 0 {
		t.Fatalf("已收录单价的模型应算出积分: credit=%v has=%v", tr.Credit, tr.HasCredit)
	}
	first := tr.Credit

	// 第二次尝试：只报了 completion（上游常在错误响应里回半截 usage）——没报的维度不能被清零。
	tr.addUsage("cn:glm-5.2", usage.Delta{CompletionTokens: 300, HasCompletion: true})
	if tr.PromptTokens != 1000 || tr.CompletionTokens != 500 {
		t.Fatalf("二次尝试应累加到 prompt=1000 completion=500，实得 %+v", tr)
	}
	if tr.CacheHitTokens != 800 {
		t.Fatalf("第二次没报缓存，命中量应保持 800，实得 %d", tr.CacheHitTokens)
	}
	if tr.Credit <= first {
		t.Fatalf("第二次也有 completion 产出，积分应继续累加: %v → %v", first, tr.Credit)
	}

	// 未收录单价的模型：token 照记，积分不编。
	unk := &reqTrace{}
	unk.addUsage("cn:完全不存在的模型", usage.Delta{
		PromptTokens: 10, HasPromptTokens: true, CompletionTokens: 5, HasCompletion: true,
	})
	if unk.HasCredit || unk.Credit != 0 {
		t.Fatalf("未收录单价的模型不该有积分: %+v", unk)
	}
	if unk.PromptTokens != 10 {
		t.Fatalf("未收录单价的模型 token 仍要记: %+v", unk)
	}

	// nil 游标（未开请求记录时 traceFrom 返回 nil）不能 panic。
	var nilTrace *reqTrace
	nilTrace.addUsage("cn:glm-5.2", usage.Delta{PromptTokens: 1, HasPromptTokens: true})
}

// TestReqTraceCacheMiss 缓存未命中量 = prompt − 命中（上游只报命中量）。
// prompt 缺失或命中超过 prompt 时都不猜，回 0 = 面板不显示命中率。
func TestReqTraceCacheMiss(t *testing.T) {
	cases := []struct {
		name  string
		trace reqTrace
		want  int64
	}{
		{"正常", reqTrace{PromptTokens: 1000, CacheHitTokens: 800}, 200},
		{"无缓存信息", reqTrace{PromptTokens: 1000}, 1000},
		{"无 prompt 不猜", reqTrace{CacheHitTokens: 800}, 0},
		{"命中超过 prompt 夹到 0", reqTrace{PromptTokens: 100, CacheHitTokens: 300}, 0},
	}
	for _, c := range cases {
		if got := c.trace.cacheMissTokens(); got != c.want {
			t.Errorf("%s: cacheMissTokens()=%d，期望 %d", c.name, got, c.want)
		}
	}
}
