package pool

import (
	"testing"
	"time"

	"traework2api/internal/auth"
)

// 快过期优先：窗口内有到期的号，就只在它们里选（哪怕积分更低）。
func TestExpiringSoonPriority(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "big"})
	p.Add(&auth.Auth{UID: "soon"})
	p.SetCredits("big", 9000)
	p.SetCredits("soon", 100)
	p.SetCreditsExpire("soon", 100, 100, time.Now().Add(24*time.Hour).Unix()) // 明天到期，窗口 168h
	if got := p.Pick(); got == nil || got.UID != "soon" {
		t.Fatalf("快过期号应优先被选中，got %v", got)
	}
	p.Release("soon")

	// 到期时间在窗口外 → 回到按积分挑。
	p.SetCreditsExpire("soon", 100, 100, time.Now().Add(720*time.Hour).Unix())
	if got := p.Pick(); got == nil || got.UID != "big" {
		t.Fatalf("窗口外应回到积分优先，got %v", got)
	}
}

// 在途上限：单号占满后不再被选中，Release 后恢复。
func TestMaxInFlight(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.ApplyLimits(Limits{MaxInFlight: 1})
	a := p.Pick()
	if a == nil || a.UID != "u1" {
		t.Fatalf("第一次应选中 u1: %v", a)
	}
	if got := p.Pick(); got != nil {
		t.Fatalf("在途已满不该再选中: %v", got)
	}
	p.Release("u1")
	if got := p.Pick(); got == nil {
		t.Fatal("Release 后应恢复可选")
	}
}

// 闲置补偿：久未使用的号在权重上被抬到前面（积分相同时）。
func TestIdleWeightPrefersColdAccount(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "hot"})
	p.Add(&auth.Auth{UID: "cold"})
	p.SetCredits("hot", 1000)
	p.SetCredits("cold", 1000)
	p.mu.Lock()
	p.byUID["hot"].lastUsed = time.Now()                       // 刚用过
	p.byUID["cold"].lastUsed = time.Now().Add(-10 * time.Hour) // 闲置 10 小时
	p.mu.Unlock()
	if got := p.Pick(); got == nil || got.UID != "cold" {
		t.Fatalf("闲置号应更优先，got %v", got)
	}
}

// 熔断退避有封顶：连续触发不会把冷却时间无限翻倍。
func TestBreakerBackoffCapped(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.ApplyLimits(Limits{
		BreakerThreshold:   1,
		BreakerCooldown:    time.Minute,
		BreakerCooldownMax: 5 * time.Minute,
	})
	for i := 0; i < 6; i++ {
		p.NoteError("u1")
	}
	st, _ := p.Status("u1")
	if d := time.Until(st.Until); d > 5*time.Minute+time.Second {
		t.Fatalf("冷却 %v 超过封顶 5m", d)
	}
}

// 降权：连败达阈临时出池；成功即清零。
func TestDegradeThenRecover(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.ApplyLimits(Limits{DegradeThreshold: 2, DegradeCooldown: time.Minute, DegradeCooldownMax: time.Hour})
	p.NoteDegrade("u1")
	if st, _ := p.Status("u1"); st.Cooling {
		t.Fatal("一次降权不该出池")
	}
	p.NoteDegrade("u1")
	st, _ := p.Status("u1")
	if !st.Cooling || st.Degraded != 0 {
		t.Fatalf("达阈应出池并清零计数: %+v", st)
	}
	p.NoteSuccess("u1")
	if _, ok := p.AcquireIfHealthy("u1"); ok {
		t.Fatal("冷却期内不该可用")
	}
}
