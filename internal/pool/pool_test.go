package pool

import (
	"path/filepath"
	"testing"
	"time"

	"traework2api/internal/auth"
)

func TestPickHighestCredits(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.Add(&auth.Auth{UID: "u3"})
	p.SetCredits("u1", 100)
	p.SetCredits("u2", 500)
	p.SetCredits("u3", 300)
	got := p.Pick()
	if got == nil || got.UID != "u2" {
		t.Fatalf("pick=%+v want u2", got)
	}
}

func TestPickSkipsCooling(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.SetCredits("u1", 100)
	p.SetCredits("u2", 50)
	p.Cooldown("u1", CoolPlan, time.Hour, "plan limit")
	got := p.Pick()
	if got == nil || got.UID != "u2" {
		t.Fatalf("pick=%+v want u2", got)
	}
}

func TestPickExpiredCooldownReturnsToHealthy(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 100)
	p.Cooldown("u1", CoolSoft, time.Millisecond, "429")
	time.Sleep(5 * time.Millisecond)
	got := p.Pick()
	if got == nil || got.UID != "u1" {
		t.Fatalf("pick=%+v want u1 after cooldown expiry", got)
	}
}

func TestPickNilWhenAllCooling(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Cooldown("u1", CoolPlan, time.Hour, "x")
	if got := p.Pick(); got != nil {
		t.Fatalf("want nil, got %+v", got)
	}
}

func TestPickExcluding(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.SetCredits("u1", 100)
	p.SetCredits("u2", 50)
	tried := map[string]bool{"u1": true}
	got := p.PickExcluding(tried)
	if got == nil || got.UID != "u2" {
		t.Fatalf("pick=%+v want u2", got)
	}
	tried["u2"] = true
	if got := p.PickExcluding(tried); got != nil {
		t.Fatalf("want nil, got %+v", got)
	}
}

func TestCooldownPersists(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.Cooldown("u1", CoolPlan, time.Hour, "plan limit")
	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	if p2.Pick() != nil {
		t.Fatal("cooldown lost after reload")
	}
	st, ok := p2.Status("u1")
	if !ok || st.Reason != "plan limit" {
		t.Errorf("status=%+v ok=%v", st, ok)
	}
}

func TestDisablePersists(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.Disable("u1", "session dead")
	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	if p2.Pick() != nil {
		t.Fatal("disabled account picked after reload")
	}
	st, _ := p2.Status("u1")
	if !st.Disabled || st.Reason != "session dead" {
		t.Errorf("status=%+v", st)
	}
}

func TestSessionDeadNeedsThreeStrikesAndPersists(t *testing.T) {
	fp := filepath.Join(t.TempDir(), "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})

	if p.NoteSessionDead("u1") {
		t.Fatal("第 1 次 session 失效不该禁用")
	}
	if st, _ := p.Status("u1"); st.Disabled {
		t.Fatalf("第 1 次就禁用了: %+v", st)
	}
	if p.NoteSessionDead("u1") {
		t.Fatal("第 2 次不该禁用")
	}
	if !p.NoteSessionDead("u1") {
		t.Fatal("第 3 次应触发禁用")
	}
	if st, _ := p.Status("u1"); !st.Disabled || st.Reason != "session dead" {
		t.Fatalf("status=%+v", st)
	}

	// 计数与禁用状态落盘：重启不该给故障号免费重试。
	p2 := New(fp)
	if st, _ := p2.Status("u1"); !st.Disabled {
		t.Fatalf("禁用未持久化: %+v", st)
	}

	// 刷新成功清零后，再失败一次不该禁用（误判有复活路径）。
	p2.ClearSessionDead("u1")
	if p2.NoteSessionDead("u1") {
		t.Fatal("计数清零后第 1 次就禁用了")
	}
}

func TestEnableRevivesDisabled(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Disable("u1", "session dead")
	if p.Pick() != nil {
		t.Fatal("禁用号不该被选中")
	}
	if !p.Enable("u1") {
		t.Fatal("Enable 应返回 true")
	}
	if got := p.Pick(); got == nil || got.UID != "u1" {
		t.Fatalf("启用后应能被选中, pick=%+v", got)
	}
	if st, _ := p.Status("u1"); st.Disabled || st.Reason != "" {
		t.Fatalf("启用后状态应干净: %+v", st)
	}
}

func TestReenableIfCredits(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Cooldown("u1", CoolPlan, time.Hour, "plan limit")
	p.ReenableIfCredits("u1", 500)
	got := p.Pick()
	if got == nil || got.UID != "u1" {
		t.Fatalf("should reenable, pick=%+v", got)
	}
}

func TestReenableZeroCreditsKeepsCooling(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Cooldown("u1", CoolPlan, time.Hour, "plan limit")
	p.ReenableIfCredits("u1", 0)
	if p.Pick() != nil {
		t.Fatal("zero credits should stay cooling")
	}
}

func TestReenableDoesNotTouchDisabled(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Disable("u1", "session dead")
	p.ReenableIfCredits("u1", 500)
	if p.Pick() != nil {
		t.Fatal("disabled must not auto-reenable")
	}
}

func TestNoteErrorThreshold(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	for i := 0; i < 2; i++ {
		p.NoteError("u1")
		if p.Pick() == nil {
			t.Fatalf("cooling too early at %d", i+1)
		}
		p.Release("u1") // Pick 会占在途名额，测试里用完就放
	}
	p.NoteError("u1")
	if p.Pick() != nil {
		t.Fatal("threshold 3 should cool the account")
	}
}

func TestNoteSuccessResetsCounter(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.NoteError("u1")
	p.NoteError("u1")
	p.NoteSuccess("u1")
	p.NoteError("u1")
	p.NoteError("u1")
	if p.Pick() == nil {
		t.Fatal("success should reset error counter")
	}
}

func TestList(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1", Nickname: "nick1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.SetCredits("u1", 42)
	p.Cooldown("u2", CoolSoft, time.Minute, "429")
	list := p.List()
	if len(list) != 2 {
		t.Fatalf("list=%d", len(list))
	}
	var s1, s2 Status
	for _, s := range list {
		if s.UID == "u1" {
			s1 = s
		}
		if s.UID == "u2" {
			s2 = s
		}
	}
	if s1.Credits != 42 || s1.Nickname != "nick1" || s1.Disabled || s1.Cooling {
		t.Errorf("s1=%+v", s1)
	}
	if !s2.Cooling || s2.Reason != "429" {
		t.Errorf("s2=%+v", s2)
	}
}

func TestClearCooldownAndRemove(t *testing.T) {
	p := New(filepath.Join(t.TempDir(), "state.json"))
	p.Add(&auth.Auth{UID: "u1", Nickname: "n"})
	p.Cooldown("u1", CoolSoft, time.Hour, "429")
	if !p.ClearCooldown("u1") {
		t.Fatal("clear")
	}
	st, _ := p.Status("u1")
	if st.Cooling || st.Reason != "" {
		t.Fatalf("%+v", st)
	}
	p.Disable("u1", "nope")
	if p.ClearCooldown("u1") {
		t.Fatal("disabled account must stay disabled")
	}
	if _, ok := p.Remove("u1"); !ok {
		t.Fatal("remove")
	}
	if _, ok := p.Status("u1"); ok {
		t.Fatal("u1 still present")
	}
	if _, ok := p.Remove("missing"); ok {
		t.Fatal("missing uid removed")
	}
}

func TestSyncToDirRemovesMissing(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.SyncToDir([]*auth.Auth{{UID: "u2"}})
	if p.Pick() == nil || p.Pick().UID != "u2" {
		t.Fatal("u1 should be removed")
	}
	if _, ok := p.Status("u1"); ok {
		t.Fatal("u1 should not exist")
	}
}

func TestCooldownCheckin(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.CooldownCheckin("u1", 5*time.Minute, "congested")
	st, _ := p.Status("u1")
	if !st.CheckinCooling || st.CheckinReason != "congested" {
		t.Fatalf("want checkin cooling, got %+v", st)
	}
	// 签到接口拥塞不代表对话额度不可用，绝不能把账号踢出代理选号。
	if st.Cooling {
		t.Fatalf("签到冷却不得影响网关选号: %+v", st)
	}
}

// 签到冷却不得覆盖更长的网关冷却，也不得被更短的签到冷却缩短。
func TestCooldownCheckinDoesNotTouchGatewayCooldown(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Cooldown("u1", CoolPlan, 12*time.Hour, "plan limit")
	p.CooldownCheckin("u1", time.Minute, "congested")

	st, _ := p.Status("u1")
	if st.Reason != "plan limit" || time.Until(st.Until) < 11*time.Hour {
		t.Fatalf("网关冷却被改动: %+v", st)
	}
	if !st.CheckinCooling || st.CheckinReason != "congested" {
		t.Fatalf("签到冷却未生效: %+v", st)
	}
}

// 已在签到冷却中的账号，更短的签到冷却不得把它缩短。
func TestCooldownCheckinDoesNotShorten(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.CooldownCheckin("u1", 10*time.Minute, "first")
	p.CooldownCheckin("u1", time.Minute, "second")
	st, _ := p.Status("u1")
	if st.CheckinReason != "first" || time.Until(st.CheckinUntil) < 9*time.Minute {
		t.Fatalf("签到冷却被缩短: %+v", st)
	}
}

// 签到成功要清掉签到冷却。
func TestClearCheckinCooldown(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.CooldownCheckin("u1", 5*time.Minute, "congested")
	p.ClearCheckinCooldown("u1")
	st, _ := p.Status("u1")
	if st.CheckinCooling {
		t.Fatalf("签到冷却未清除: %+v", st)
	}
}

// 禁用账号不参与签到冷却。
func TestCooldownCheckinSkipsDisabled(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Disable("u1", "session dead")
	p.CooldownCheckin("u1", time.Minute, "congested")
	st, _ := p.Status("u1")
	if st.CheckinCooling {
		t.Fatalf("disabled account must not be cooled: %+v", st)
	}
}

// 「积分」列的分母要能扛住重启，且不被只改剩余值的 SetCredits 冲掉。
func TestCreditsTotalPersists(t *testing.T) {
	fp := filepath.Join(t.TempDir(), "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCreditsExpire("u1", 4940, 4941, 0)

	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	if st, _ := p2.Status("u1"); st.Credits != 4940 || st.CreditsTotal != 4941 {
		t.Fatalf("重启读回 %d/%d，期望 4940/4941", st.Credits, st.CreditsTotal)
	}
	p2.SetCredits("u1", 4000) // 只改剩余
	if st, _ := p2.Status("u1"); st.Credits != 4000 || st.CreditsTotal != 4941 {
		t.Fatalf("SetCredits 后 %d/%d，分母不该变", st.Credits, st.CreditsTotal)
	}
}
