package scheduler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"traework2api/internal/auth"
	"traework2api/internal/pool"
)

// TestCheckinRefreshesTokenOnSessionDead 签到撞上过期/被吊销的 access token：
// 换一张新票再查一次，而不是把「票过期」记成签到失败（照 wild-work 的 DailyCheckin）。
func TestCheckinRefreshesTokenOnSessionDead(t *testing.T) {
	var statusCalls, refreshCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/checkin_credits/status"):
			// 第一次用旧票 → 401 session dead；换票后第二次 → 已签到。
			if statusCalls.Add(1) == 1 {
				w.WriteHeader(http.StatusUnauthorized)
				w.Write([]byte(`{"code":1001,"message":"login required"}`))
				return
			}
			w.Write([]byte(`{"checked_in":true,"credits":200,"enable":true}`))
		case strings.HasSuffix(r.URL.Path, "/ExchangeToken"):
			refreshCalls.Add(1)
			w.Write([]byte(`{"Result":{"Token":"newat","RefreshToken":"newrt","TokenExpireAt":1786805537}}`))
		case strings.HasSuffix(r.URL.Path, "/ide_user_ent_usage"), strings.HasSuffix(r.URL.Path, "/web_user_ent_usage"):
			w.Write([]byte(`{"user_entitlement_pack_list":[{"entitlement_base_info":{"quota":{"credits_limit":500}},"usage":{"credits_amount":100}}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	p := pool.New("")
	// ExpiresAt 足够久：不走「预刷新」那条，只验 session-dead 的补救路径。
	p.Add(&auth.Auth{UID: "u1", Domain: "trae.cn", AccessToken: "old", RefreshToken: "rt", ExpiresAt: 9999999999})
	s := newTestScheduler(&fakeUpstream{}, p, srv)

	res, err := s.CheckinUID("u1")
	if err != nil {
		t.Fatalf("换票后应签到成功，实得 err=%v res=%+v", err, res)
	}
	if res.Status != "already" {
		t.Errorf("status=%q 期望 already", res.Status)
	}
	if refreshCalls.Load() != 1 {
		t.Errorf("应换票 1 次，实得 %d", refreshCalls.Load())
	}
	if statusCalls.Load() != 2 {
		t.Errorf("应查 status 2 次（旧票失败 + 换票后重试），实得 %d", statusCalls.Load())
	}
}

// TestCheckinClaimAlreadyTreatedAsCheckedIn claim 回 9095（今日已签到）时按「已签」处理：
// 报成失败会让面板显示「签到失败：今日已签到」。
func TestCheckinClaimAlreadyTreatedAsCheckedIn(t *testing.T) {
	f := &fakeUpstream{claimResponse: `{"code":9095,"message":"今日已签到"}`, resourceRemain: 500}
	srv := f.server()
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", Domain: "trae.cn", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	s := newTestScheduler(f, p, srv)

	res, err := s.CheckinUID("u1")
	if err != nil {
		t.Fatalf("9095 不该报错：%v", err)
	}
	if res.Status != "already" {
		t.Fatalf("status=%q 期望 already（不是 failed）", res.Status)
	}
	if f.claimCalls.Load() != 1 {
		t.Errorf("claim 次数=%d 期望 1", f.claimCalls.Load())
	}
}

// TestCheckinStatusRateLimitIsReported 上游对 status 回限流信封时，必须如实报错并给签到
// 冷却——此前不校验 code，被读成 enable=false → 静默「上游未开签到」（不冷却、不报警）。
func TestCheckinStatusRateLimitIsReported(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/checkin_credits/status"):
			w.Write([]byte(`{"code":9074,"message":"当前参与用户太多，请稍后再试"}`))
		case strings.HasSuffix(r.URL.Path, "/ide_user_ent_usage"), strings.HasSuffix(r.URL.Path, "/web_user_ent_usage"):
			w.Write([]byte(`{"user_entitlement_pack_list":[{"entitlement_base_info":{"quota":{"credits_limit":500}},"usage":{"credits_amount":100}}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", Domain: "trae.cn", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	s := newTestScheduler(&fakeUpstream{}, p, srv)

	res, err := s.CheckinUID("u1")
	if err == nil {
		t.Fatalf("限流应报错，实得 res=%+v", res)
	}
	if !strings.Contains(err.Error(), "当前参与用户太多") {
		t.Errorf("错误应带上游原文：%v", err)
	}
	if res.Status == "skipped" {
		t.Error("限流不能被当成「上游未开启签到」跳过")
	}
	if st, ok := p.Status("u1"); !ok || !st.CheckinCooling {
		t.Errorf("限流应记签到域冷却，实得 %+v", st)
	}
}
