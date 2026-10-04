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

// TestCheckinClaim9095IsDeviceScoped 9095「今日已签到」是**设备维度**信息：同一台设备可能
// 已经替另一个账号签过，所以不能据此判定本账号已签（Trae2api-cn 原注释：只有 status 能定论）。
// 这里的处置：换一套设备指纹（不换的话下一轮还会回 9095）+ 以 status 复核定论。
func TestCheckinClaim9095IsDeviceScoped(t *testing.T) {
	f := &fakeUpstream{claimResponse: `{"code":9095,"message":"今日已签到"}`, resourceRemain: 500}
	srv := f.server()
	defer srv.Close()

	a := &auth.Auth{UID: "u1", Domain: "trae.cn", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999}
	p := pool.New("")
	p.Add(a)
	s := newTestScheduler(f, p, srv)

	res, err := s.CheckinUID("u1")
	// 假上游的 status 在 claim 成功后才会回 checked_in=true；9095 没落账 → 复核必须判失败，
	// 绝不能凭 9095 就报「已签」（那正是这个测试要钉住的错误行为）。
	if err == nil || res.Status != "failed" {
		t.Fatalf("9095 不该当成已签：status=%q err=%v", res.Status, err)
	}
	if f.claimCalls.Load() != 1 {
		t.Errorf("claim 次数=%d 期望 1（不秒级重试）", f.claimCalls.Load())
	}
	if a.DeviceSeedValue() != 1 {
		t.Errorf("9095 后应换一套设备指纹，实得 seed=%d", a.DeviceSeedValue())
	}
}

// TestCheckin9074RotatesDeviceWithoutRetry 9074「当前参与用户太多」是设备维度限流：
// 换一套设备指纹让下一轮脱开旧标记，但**不立刻重试**——上游原注释说秒级内第二次 claim
// 会延长限流窗口（Trae2api-cn trae_client.py）。
func TestCheckin9074RotatesDeviceWithoutRetry(t *testing.T) {
	f := &fakeUpstream{claimResponse: `{"code":9074,"message":"当前参与用户太多，请稍后再试"}`, resourceRemain: 500}
	srv := f.server()
	defer srv.Close()

	a := &auth.Auth{UID: "u1", Domain: "trae.cn", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999}
	p := pool.New("")
	p.Add(a)
	s := newTestScheduler(f, p, srv)

	res, err := s.CheckinUID("u1")
	if err == nil || res.Status != "failed" {
		t.Fatalf("限流应如实报失败：status=%q err=%v", res.Status, err)
	}
	if f.claimCalls.Load() != 1 {
		t.Fatalf("claim 次数=%d 期望 1（不做秒级重试）", f.claimCalls.Load())
	}
	if a.DeviceSeedValue() != 1 {
		t.Errorf("9074 后应换一套设备指纹，实得 seed=%d", a.DeviceSeedValue())
	}
	if st, _ := p.Status("u1"); !st.CheckinCooling {
		t.Error("限流应记签到域冷却")
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
