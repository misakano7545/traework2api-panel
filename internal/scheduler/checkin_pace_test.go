package scheduler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"traework2api/internal/auth"
	"traework2api/internal/pool"
)

// TestRunCheckinPacesAccounts 多账号签到之间要有间隔（上游按设备/时段限流，背靠背连签
// 容易吃 9074）。第一个账号不等，最后一个签完也不再等。
func TestRunCheckinPacesAccounts(t *testing.T) {
	f := &fakeUpstream{resourceRemain: 500}
	srv := f.server()
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", Domain: "trae.cn", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	p.Add(&auth.Auth{UID: "u2", Domain: "trae.cn", AccessToken: "at2", RefreshToken: "rt2", ExpiresAt: 9999999999})
	s := newTestScheduler(f, p, srv)
	checkinGap, checkinGapJitter = 60*time.Millisecond, 0
	t.Cleanup(func() { checkinGap, checkinGapJitter = 0, 0 })

	start := time.Now()
	s.RunCheckinNow()
	// 两个账号 → 一次间隔（60ms）。给足容差，只验「确实等了」。
	if elapsed := time.Since(start); elapsed < 60*time.Millisecond {
		t.Fatalf("两个账号之间没有间隔：%v", elapsed)
	}
	if f.claimCalls.Load() != 2 {
		t.Fatalf("两个账号都该签：claim=%d", f.claimCalls.Load())
	}
}

// TestCheckinVerifyFailureIsFailure claim 回 code 0 但复核时上游仍报未签到 → 这次签到
// 算失败（不能只看 claim 的回执：上游偶发只回执不落账）。
func TestCheckinVerifyFailureIsFailure(t *testing.T) {
	var claimOK atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/checkin_credits/status"):
			w.Write([]byte(`{"checked_in":false,"credits":150,"enable":true}`)) // 永远未签
		case strings.HasSuffix(r.URL.Path, "/checkin_credits/claim"):
			claimOK.Store(true)
			w.Write([]byte(`{"code":0,"message":"success"}`)) // 回执说成功
		case strings.HasSuffix(r.URL.Path, "/ide_user_ent_usage"):
			w.Write([]byte(`{"user_entitlement_pack_list":[{"entitlement_base_info":{"quota":{"credits_limit":500}},"usage":{"credits_amount":100}}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", Domain: "trae.cn", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	res, err := newTestScheduler(&fakeUpstream{}, p, srv).CheckinUID("u1")
	if !claimOK.Load() {
		t.Fatal("claim 没被调用")
	}
	if err == nil || res.Status != "failed" {
		t.Fatalf("复核未通过应算失败：status=%q err=%v", res.Status, err)
	}
	if st, _ := p.Status("u1"); !st.CheckinCooling {
		t.Error("复核失败与 claim 失败同口径：应记签到域冷却")
	}
}
