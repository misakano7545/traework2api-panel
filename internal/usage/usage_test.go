package usage

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func delta(pt, ct, tt int64, latMs int64) Delta {
	d := Delta{
		PromptTokens: pt, HasPromptTokens: true,
		CompletionTokens: ct, HasCompletion: true,
		LatencyMs: latMs, HasLatency: true,
	}
	if tt > 0 {
		d.TotalTokens, d.HasTotal = tt, true
	}
	return d
}

// 分桶与聚合口径：窗口内的桶进汇总，窗口外的桶不进来但仍在库里。
func TestSnapshotWindowAndTotals(t *testing.T) {
	r := New("") // 不落盘
	now := time.Now()
	r.Add(now, "u1", "glm-5.2", delta(10, 5, 15, 100), true)
	r.Add(now, "u1", "glm-5.2", delta(20, 5, 25, 300), false)
	r.Add(now, "u2", "kimi-k2.6", delta(1, 1, 2, 10), true)
	// 100 小时前（窗口外）
	r.Add(now.Add(-100*time.Hour), "u1", "glm-5.2", delta(999, 999, 1998, 0), true)

	win := r.Snapshot(72, map[string]string{"u1": "甲"})
	tot := win.Totals
	if tot.Requests != 3 || tot.Errors != 1 {
		t.Fatalf("窗口内请求/失败 = %d/%d，期望 3/1", tot.Requests, tot.Errors)
	}
	if tot.PromptTokens != 31 || tot.CompletionTok != 11 || tot.TotalTokens != 42 {
		t.Fatalf("token 汇总 = %d/%d/%d，期望 31/11/42", tot.PromptTokens, tot.CompletionTok, tot.TotalTokens)
	}
	// 加权均值： (100+300+10)/3，不是各桶均值的平均
	if got := tot.AvgLatencyMs; got < 136.6 || got > 136.7 {
		t.Fatalf("平均延迟 = %v，期望 136.67", got)
	}
	if len(win.ByAccount) != 2 || win.ByAccount[0].Key != "u1" || win.ByAccount[0].TotalTokens != 40 {
		t.Fatalf("按账号聚合 = %+v", win.ByAccount)
	}
	if win.ByAccount[0].Extra != "甲" {
		t.Fatalf("昵称未带出: %+v", win.ByAccount[0])
	}
	if len(win.ByModel) != 2 || win.ByModel[0].Key != "glm-5.2" {
		t.Fatalf("按模型聚合 = %+v", win.ByModel)
	}

	all := r.Snapshot(0, nil)
	if all.Totals.Requests != 4 || all.Totals.TotalTokens != 2040 {
		t.Fatalf("全部历史 = %+v", all.Totals)
	}
}

// 上游不给 total 时用 pt+ct 兜底，保证总量口径连续。
func TestTotalFallback(t *testing.T) {
	r := New("")
	r.Add(time.Now(), "u1", "m", delta(7, 3, 0, 1), true)
	if got := r.Snapshot(72, nil).Totals.TotalTokens; got != 10 {
		t.Fatalf("兜底总量 = %d，期望 10", got)
	}
}

// 折叠幂等：同一小时反复折叠不重复计数。
func TestRollupIdempotent(t *testing.T) {
	r := New("")
	now := time.Now()
	old := now.Add(-200 * 24 * time.Hour)
	r.Add(old, "u1", "m", delta(5, 5, 10, 0), true)
	r.Rollup(now)
	first := r.Snapshot(0, nil).Totals.TotalTokens
	r.Rollup(now)
	if second := r.Snapshot(0, nil).Totals.TotalTokens; second != first || first != 10 {
		t.Fatalf("折叠后总量 %d → %d，期望两次都是 10", first, second)
	}
	if b := r.Snapshot(0, nil).Buckets; b != 1 {
		t.Fatalf("折叠后桶数 = %d，期望 1", b)
	}
}

// 折叠进**已存在的**日桶时要连缓存命中量一起累加：漏加会让命中率偏低，且计费公式里
// (PT-CH) 变大 → 积分被多算，全程静默（新建日桶的分支是整块拷贝，丢不了）。
func TestRollupMergeKeepsCacheHit(t *testing.T) {
	r := New("")
	now := time.Now()
	y, mo, d := now.Add(-200 * 24 * time.Hour).Date()
	h1 := time.Date(y, mo, d, 9, 0, 0, 0, time.Local)
	h2 := time.Date(y, mo, d, 10, 0, 0, 0, time.Local) // 同一天 → 第二个小时桶走合并分支
	dd := delta(4, 4, 8, 0)
	dd.CacheHitTokens, dd.HasCacheHit = 3, true
	r.Add(h1, "u1", "m", dd, true)
	r.Add(h2, "u1", "m", dd, true)
	r.Rollup(now)
	got := r.Snapshot(0, nil).Totals
	if got.CacheHitTokens != 6 || got.CacheSamples != 2 {
		t.Fatalf("折叠后缓存命中 = %d/%d，期望 6/2", got.CacheHitTokens, got.CacheSamples)
	}
	if got.TotalTokens != 16 {
		t.Fatalf("折叠后总量 = %d，期望 16", got.TotalTokens)
	}
}

// 落盘 → 重启读回：用量不能因重启丢失。
func TestSaveAndReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.json")
	r := New(path)
	r.Add(time.Now(), "u9", "glm-5.2", delta(4, 6, 10, 50), true)
	r.Save()

	back := New(path)
	if got := back.Snapshot(72, nil).Totals; got.Requests != 1 || got.TotalTokens != 10 || got.PromptTokens != 4 {
		t.Fatalf("重启读回 = %+v", got)
	}
}

// 并发落盘不得写坏台账：30s 后台 ticker 与面板「刷新」的 Save 会真的同时落盘，
// 两者共写固定名 usage.json.tmp —— rename 撞车就会把另一个写了一半的内容装上去，
// 重启时 load() 解析失败，整个用量/积分台账从零开始（见 flush 注释）。
func TestConcurrentFlushKeepsFileValid(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.json")
	r := New(path)
	const n = 800 // 快照够大，把单次 WriteFile 拉长到能被并发撞上
	now := time.Now()
	for i := 0; i < n; i++ {
		r.Add(now, "u1", fmt.Sprintf("m%04d", i), delta(1, 1, 2, 1), true)
	}

	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 12; j++ {
				r.flush(true)
				// 自己这次 rename 已经回来了，文件必然存在。
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Errorf("读回落盘文件失败: %v", err)
					return
				}
				var f file
				if err := json.Unmarshal(raw, &f); err != nil {
					t.Errorf("台账被写坏（解析失败）: %v", err)
					return
				}
				if len(f.Buckets) != n {
					t.Errorf("桶数丢了: %d，期望 %d", len(f.Buckets), n)
					return
				}
			}
		}()
	}
	wg.Wait()
}
