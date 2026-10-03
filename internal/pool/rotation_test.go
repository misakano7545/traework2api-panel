package pool

import (
	"testing"
	"time"

	"traework2api/internal/auth"
)

// fixedPick 让「谁权重更高」的用例保持确定性：抽签源恒返回 0 = 权重最高者中签
// （等价于改造前的取最大语义）。每个 Pool 各注入一次，不跨用例共享。
func fixedPick(t *testing.T, p *Pool) {
	t.Helper()
	p.SetRandInt64N(func(int64) int64 { return 0 })
}

// 选号是**加权随机**，不是取最大：积分最高的号只该分到更多流量，不该独占。
// 这是「面板里只有一个账号被调用」的回归网——旧实现取 argmax，积分最高者永远中签。
func TestPickSpreadsAcrossAccounts(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.SetCredits("u1", 4417) // 实测口径：两个国内号 4417 / 4600，旧实现 4600 那个吃掉全部流量
	p.SetCredits("u2", 4600)

	const n = 400
	hits := map[string]int{}
	for i := 0; i < n; i++ {
		// 清掉防撞号窗口与闲置加成的影响：每次都把两个号都置成「刚用过」状态不可行，
		// 这里直接推进 lastUsed，只考察权重分布本身。
		p.mu.Lock()
		for _, e := range p.byUID {
			e.lastUsed = time.Now().Add(-time.Hour)
		}
		p.mu.Unlock()
		a := p.Pick()
		if a == nil {
			t.Fatal("应能选中")
		}
		hits[a.UID]++
		p.Release(a.UID)
	}
	if hits["u1"] == 0 || hits["u2"] == 0 {
		t.Fatalf("两个号都该被分到流量，实际 %v", hits)
	}
	// 权重比 u2:u1 = (1+10):(1+4417/4600*10) ≈ 11:10.6，两者都该拿到可观份额。
	for uid, n := range hits {
		if n < 400/6 {
			t.Fatalf("%s 只拿到 %d/%d，分流不均：%v", uid, n, 400, hits)
		}
	}
}

// 防撞号：minPickGap 内刚被选中的号不进抽签池——否则同一瞬间的并发请求会各自抽签、
// 全部落到权重最高的那个号上（正是「只调用一个账号」的另一种成因）。
func TestPickAvoidsImmediateRepeat(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.SetCredits("u1", 500) // u1 权重明显更高
	p.SetCredits("u2", 100)

	first := p.Pick()
	if first == nil {
		t.Fatal("应能选中")
	}
	p.Release(first.UID)
	// 紧接着再选：刚被选中的号落在 minPickGap 窗口内，应被挤到另一个号。
	second := p.Pick()
	if second == nil {
		t.Fatal("应能选中")
	}
	if second.UID == first.UID {
		t.Fatalf("刚用过的号不该立刻重复中签：first=%s second=%s", first.UID, second.UID)
	}
	p.Release(second.UID)
}

// 候选全部刚被用过 → LRU 兜底：按 usedSeq 取最久未被选中的那个，不许恒取同一个。
func TestPickFallsBackToLRUByUsedSeq(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.SetCredits("u1", 500)
	p.SetCredits("u2", 100)

	// 手工把两个号都标成「刚用过」，且 u1 的序号更新（更晚被用过）→ 应选 u2。
	p.mu.Lock()
	p.byUID["u1"].lastUsed = time.Now()
	p.byUID["u2"].lastUsed = time.Now()
	p.byUID["u1"].usedSeq = 2
	p.byUID["u2"].usedSeq = 1
	p.mu.Unlock()

	got := p.PickExcluding(nil)
	if got == nil || got.UID != "u2" {
		t.Fatalf("LRU 兜底应选最久未被选中的 u2，got %v", got)
	}
}

// 国际版免费号 credits 恒 0（上游没有积分只会返回 credits_limit=0 的套餐），
// 但它是 healthy 的，必须仍能被选中——旧实现的乘性权重在 credits=0 时恒为 0，
// 这类号会被永久饿死（连 healthy 判据都进不了抽签）。
func TestPickZeroCreditAccountStillSelectable(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "paid"})
	p.Add(&auth.Auth{UID: "free", Domain: "trae.ai"})
	p.SetCredits("paid", 4600)
	p.SetCredits("free", 0)

	hits := map[string]int{}
	for i := 0; i < 200; i++ {
		p.mu.Lock()
		for _, e := range p.byUID {
			e.lastUsed = time.Now().Add(-time.Hour)
		}
		p.mu.Unlock()
		a := p.Pick()
		if a == nil {
			t.Fatal("应能选中")
		}
		hits[a.UID]++
		p.Release(a.UID)
	}
	if hits["free"] == 0 {
		t.Fatalf("0 积分但 healthy 的号不该被饿死：%v", hits)
	}
}
