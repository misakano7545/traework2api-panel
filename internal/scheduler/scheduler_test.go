package scheduler

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"traework2api/internal/auth"
	"traework2api/internal/pool"
	"traework2api/internal/upstream"
)

func TestNextFire(t *testing.T) {
	loc := time.Local
	now := time.Date(2026, 7, 27, 10, 0, 0, 0, loc)
	next := nextFire(now, []int{9, 21})
	if next.Hour() != 21 || next.Day() != 27 {
		t.Errorf("next=%v want 21:00 same day", next)
	}
	now = time.Date(2026, 7, 27, 22, 0, 0, 0, loc)
	next = nextFire(now, []int{9, 21})
	if next.Hour() != 9 || next.Day() != 28 {
		t.Errorf("next=%v want 09:00 next day", next)
	}
	now = time.Date(2026, 7, 27, 9, 0, 0, 0, loc)
	next = nextFire(now, []int{9})
	if next.Day() != 28 {
		t.Errorf("exact match should roll to next day: %v", next)
	}
}

func TestNextFireMergesSchedules(t *testing.T) {
	now := time.Date(2026, 7, 27, 20, 0, 0, 0, time.Local)
	next := nextFire(now, []int{9, 21, 22})
	if next.Hour() != 21 {
		t.Errorf("next=%v want 21 (earliest of 21/22)", next)
	}
}

// fakeUpstream 同时模拟 checkin/ent_usage/refresh。
type fakeUpstream struct {
	checkinCalls   atomic.Int32
	claimCalls     atomic.Int32
	refreshCalls   atomic.Int32
	claimResponse  string
	resourceRemain int64
	resourceUsed   int64
}

func (f *fakeUpstream) server() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/checkin_credits/status"):
			f.checkinCalls.Add(1)
			w.Write([]byte(`{"checked_in":false,"credits":200,"enable":true}`))
		case strings.HasSuffix(r.URL.Path, "/checkin_credits/claim"):
			f.claimCalls.Add(1)
			body := f.claimResponse
			if body == "" {
				body = `{"code":0,"message":"success"}`
			}
			w.Write([]byte(body))
		case strings.HasSuffix(r.URL.Path, "/ide_user_ent_usage"):
			w.Write([]byte(`{"is_credits_billing":true,"user_entitlement_pack_list":[{"entitlement_base_info":{"quota":{"credits_limit":` +
				jsonI64(f.resourceRemain) + `}},"usage":{"credits_amount":` + jsonI64(f.resourceUsed) + `}}]}`))
		case strings.HasSuffix(r.URL.Path, "/ExchangeToken"):
			f.refreshCalls.Add(1)
			w.Write([]byte(`{"Result":{"Token":"newat","RefreshToken":"newrt","TokenExpireAt":1786805537,"TokenExpireDuration":1209600}}`))
		default:
			http.Error(w, "not found: "+r.URL.Path, 404)
		}
	}))
}

func jsonI64(v int64) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func newTestScheduler(f *fakeUpstream, p *pool.Pool, srv *httptest.Server) *Scheduler {
	up := &upstream.Client{
		HTTP:      srv.Client(),
		AgentHost: srv.URL,
		UgHost:    srv.URL,
		OAuthHost: srv.URL,
		ClientID:  upstream.ClientID,
	}
	return New(Config{
		Pool:           p,
		Upstream:       up,
		CheckinHours:   []int{9},
		KeepaliveHours: []int{3},
	})
}

func TestRunCheckinReenablesCoolingAccount(t *testing.T) {
	f := &fakeUpstream{resourceRemain: 500, resourceUsed: 120}
	srv := f.server()
	defer srv.Close()

	p := pool.New("")
	a := &auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999}
	p.Add(a)
	p.Cooldown("u1", pool.CoolPlan, time.Hour, "plan limit")

	s := newTestScheduler(f, p, srv)
	s.RunCheckinNow()
	if f.checkinCalls.Load() != 1 {
		t.Errorf("checkin status calls=%d", f.checkinCalls.Load())
	}
	if f.claimCalls.Load() != 1 {
		t.Errorf("claim calls=%d", f.claimCalls.Load())
	}
	st, _ := p.Status("u1")
	if st.Cooling {
		t.Errorf("account should be reenabled after checkin with credits: %+v", st)
	}
	if st.Credits != 380 {
		t.Errorf("credits=%d want 380", st.Credits)
	}
}

func TestCheckinUIDReturnsClaimBusinessError(t *testing.T) {
	f := &fakeUpstream{
		claimResponse:  `{"code":9074,"message":"当前参与用户太多，请稍后再试"}`,
		resourceRemain: 4500,
	}
	srv := f.server()
	defer srv.Close()
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})

	err := newTestScheduler(f, p, srv).CheckinUID("u1")
	if err == nil || !strings.Contains(err.Error(), "当前参与用户太多") {
		t.Fatalf("err=%v", err)
	}
}

func TestRunCheckinNowReturnsFailures(t *testing.T) {
	f := &fakeUpstream{claimResponse: `{"code":9074,"message":"当前参与用户太多，请稍后再试"}`}
	srv := f.server()
	defer srv.Close()
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})

	errs := newTestScheduler(f, p, srv).RunCheckinNow()
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "u1") {
		t.Fatalf("errs=%v", errs)
	}
}

func TestRunCheckinSkipsDisabled(t *testing.T) {
	f := &fakeUpstream{}
	srv := f.server()
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	p.Disable("u1", "session dead")

	s := newTestScheduler(f, p, srv)
	s.RunCheckinNow()
	if f.checkinCalls.Load() != 0 {
		t.Errorf("disabled account should be skipped, calls=%d", f.checkinCalls.Load())
	}
}

func TestRunRefreshRefreshesTokens(t *testing.T) {
	f := &fakeUpstream{}
	srv := f.server()
	defer srv.Close()

	p := pool.New("")
	a := &auth.Auth{UID: "u1", AccessToken: "old", RefreshToken: "rt", ExpiresAt: 1, ApiHost: srv.URL}
	p.Add(a)

	s := newTestScheduler(f, p, srv)
	s.RunRefreshNow()
	if f.refreshCalls.Load() != 1 {
		t.Errorf("refresh calls=%d", f.refreshCalls.Load())
	}
	if a.AccessToken != "newat" {
		t.Errorf("token not updated: %s", a.AccessToken)
	}
}

func TestRunRefreshRefreshesFreshTokenToo(t *testing.T) {
	// 保活不看剩余有效期：access token 活 14 天，只在过期前 24h 刷等于 refresh 链
	// 十天半个月没人碰，上游一过期就只能重登（掉线）。每天刷一遍才活得久。
	f := &fakeUpstream{}
	srv := f.server()
	defer srv.Close()

	p := pool.New("")
	a := &auth.Auth{UID: "u1", AccessToken: "fresh", RefreshToken: "rt", ExpiresAt: 9999999999, ApiHost: srv.URL}
	p.Add(a)

	s := newTestScheduler(f, p, srv)
	s.RunRefreshNow()
	if f.refreshCalls.Load() != 1 {
		t.Errorf("保活应无条件刷一遍, calls=%d", f.refreshCalls.Load())
	}
	if a.AccessToken != "newat" {
		t.Errorf("token 未更新: %s", a.AccessToken)
	}
}

func TestRunRefreshSessionDeadNeedsThreeStrikes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		w.Write([]byte(`{"code":20101,"msg":"login required"}`))
	}))
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "old", RefreshToken: "rt", ExpiresAt: 1, ApiHost: srv.URL})

	up := &upstream.Client{HTTP: srv.Client(), AgentHost: srv.URL, UgHost: srv.URL, OAuthHost: srv.URL, ClientID: upstream.ClientID}
	s := New(Config{Pool: p, Upstream: up})
	for i := 1; i <= 2; i++ {
		s.RunRefreshNow()
		if st, _ := p.Status("u1"); st.Disabled {
			t.Fatalf("第 %d 次 session 失效就禁用了（一次 401 不该杀号）: %+v", i, st)
		}
	}
	s.RunRefreshNow()
	st, _ := p.Status("u1")
	if !st.Disabled {
		t.Errorf("连续 3 次 session 失效仍不禁用: %+v", st)
	}
}

// failFirstTransport 让第 n 次 claim 在网络层失败（模拟连接被拒）。
type failFirstTransport struct {
	inner    http.RoundTripper
	fail     atomic.Int32
	attempts atomic.Int32
}

func (t *failFirstTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if !strings.Contains(r.URL.Path, "/claim") {
		return t.inner.RoundTrip(r)
	}
	t.attempts.Add(1)
	if t.fail.Add(-1) >= 0 {
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
	}
	return t.inner.RoundTrip(r)
}

func flakyClient(srv *httptest.Server, failures int32) (*upstream.Client, *failFirstTransport) {
	tr := &failFirstTransport{inner: srv.Client().Transport}
	tr.fail.Store(failures)
	return &upstream.Client{
		HTTP:      &http.Client{Transport: tr},
		AgentHost: srv.URL,
		UgHost:    srv.URL,
		OAuthHost: srv.URL,
		ClientID:  upstream.ClientID,
	}, tr
}

// 业务失败必须给账号上冷却，否则下一轮或用户连点会立刻再打一次上游。
func TestCheckinBusinessErrorCoolsAccount(t *testing.T) {
	f := &fakeUpstream{claimResponse: `{"code":9074,"message":"当前参与用户太多，请稍后再试"}`, resourceRemain: 4500}
	srv := f.server()
	defer srv.Close()
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})

	_ = newTestScheduler(f, p, srv).CheckinUID("u1")
	st, _ := p.Status("u1")
	if !st.CheckinCooling {
		t.Fatalf("business error must set checkin cooldown: %+v", st)
	}
	// 9074 只是签到接口拥塞，账号的对话额度没事，不能踢出代理选号。
	if st.Cooling {
		t.Fatalf("签到失败不得影响网关选号: %+v", st)
	}
}

// 签到失败不得给网关冷却域塞东西：即便余额为 0（「签到解冻」不生效），
// 也不该因为签到失败把账号踢出代理选号。
func TestCheckinFailureAddsNoGatewayCooldown(t *testing.T) {
	f := &fakeUpstream{claimResponse: `{"code":9074,"message":"congested"}`}
	srv := f.server()
	defer srv.Close()
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})

	_ = newTestScheduler(f, p, srv).CheckinUID("u1")
	st, _ := p.Status("u1")
	if st.Cooling {
		t.Fatalf("签到失败把账号踢出了代理选号: %+v", st)
	}
	if !st.CheckinCooling {
		t.Fatalf("签到冷却未生效: %+v", st)
	}
}

// 网络层失败重试一次；业务失败绝不重试（重试只会成倍打上游、触发风控）。
func TestClaimRetriesNetworkError(t *testing.T) {
	f := &fakeUpstream{resourceRemain: 500}
	srv := f.server()
	defer srv.Close()
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})

	up, tr := flakyClient(srv, 1)
	s := New(Config{Pool: p, Upstream: up, CheckinHours: []int{9}})
	if err := s.CheckinUID("u1"); err != nil {
		t.Fatalf("retry should recover: %v", err)
	}
	if got := tr.attempts.Load(); got != 2 {
		t.Fatalf("transport attempts=%d want 2 (1 fail + 1 retry)", got)
	}
	if got := f.claimCalls.Load(); got != 1 {
		t.Fatalf("仅重试成功的那次才落到服务端：claim calls=%d want 1", got)
	}
}

func TestClaimNotRetriedOnBusinessError(t *testing.T) {
	f := &fakeUpstream{claimResponse: `{"code":9074,"message":"too many"}`, resourceRemain: 500}
	srv := f.server()
	defer srv.Close()
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})

	_ = newTestScheduler(f, p, srv).CheckinUID("u1")
	if got := f.claimCalls.Load(); got != 1 {
		t.Fatalf("claim calls=%d want 1, business failure must not retry", got)
	}
}

// 排程定在 9 点而进程 10 点才起来是常态，不补跑等于当天整天不签。
func TestCatchUpRunsAfterCheckinHour(t *testing.T) {
	f := &fakeUpstream{resourceRemain: 500}
	srv := f.server()
	defer srv.Close()
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})

	s := newTestScheduler(f, p, srv)
	s.mu.Lock()
	s.cfg.CheckinHours = []int{time.Now().Hour()} // 已到点
	s.cfg.CheckinEnabled = true
	s.mu.Unlock()
	s.CatchUp()
	if got := f.checkinCalls.Load(); got != 1 {
		t.Fatalf("catch-up must check in: status calls=%d", got)
	}
}

func TestCatchUpSkipsBeforeCheckinHour(t *testing.T) {
	now := time.Now().Hour()
	if now >= 23 {
		t.Skip("当天已无更晚的整点可构造")
	}
	f := &fakeUpstream{resourceRemain: 500}
	srv := f.server()
	defer srv.Close()
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})

	s := newTestScheduler(f, p, srv)
	s.mu.Lock()
	s.cfg.CheckinHours = []int{now + 1} // 还没到点
	s.mu.Unlock()
	s.CatchUp()
	if got := f.checkinCalls.Load(); got != 0 {
		t.Fatalf("catch-up must not run before the hour: status calls=%d", got)
	}
}

// perAccountUpstream 按 access token 区分账号，记录 claim 顺序。
type perAccountUpstream struct {
	expire map[string]int64 // token -> 最早到期时间
	claims []string         // claim 顺序（token）
}

func (f *perAccountUpstream) server() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Cloud-IDE-JWT ")
		switch {
		case strings.HasSuffix(r.URL.Path, "/checkin_credits/status"):
			w.Write([]byte(`{"checked_in":false,"credits":100,"enable":true}`))
		case strings.HasSuffix(r.URL.Path, "/checkin_credits/claim"):
			f.claims = append(f.claims, token)
			w.Write([]byte(`{"code":0,"message":"success"}`))
		case strings.HasSuffix(r.URL.Path, "/ide_user_ent_usage"):
			w.Write([]byte(`{"is_credits_billing":true,"user_entitlement_pack_list":[{"entitlement_base_info":{"quota":{"credits_limit":100}},"usage":{"credits_amount":0},"expire_time":` +
				jsonI64(f.expire[token]) + `}]}`))
		default:
			http.Error(w, "not found: "+r.URL.Path, 404)
		}
	}))
}

// 签到顺序不是池内顺序，而是积分到期紧迫度：最早到期先签。
// 上游对每日签发存在限频约束，把快过期的账号排前面，限频窗口用尽时有效积分留存最大。
func TestCheckinOrdersByEarliestExpiry(t *testing.T) {
	now := time.Now().Unix()
	// u2 更早到期；UID 排序是 u1 在前，所以「池内顺序」与「紧迫度顺序」不同。
	f := &perAccountUpstream{expire: map[string]int64{"tok1": now + 86400*30, "tok2": now + 86400}}
	srv := f.server()
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "tok1", RefreshToken: "rt", ExpiresAt: 9999999999})
	p.Add(&auth.Auth{UID: "u2", AccessToken: "tok2", RefreshToken: "rt", ExpiresAt: 9999999999})

	up := &upstream.Client{HTTP: srv.Client(), AgentHost: srv.URL, UgHost: srv.URL, OAuthHost: srv.URL, ClientID: upstream.ClientID}
	New(Config{Pool: p, Upstream: up, CheckinHours: []int{9}}).RunCheckinNow()

	if len(f.claims) != 2 || f.claims[0] != "tok2" {
		t.Fatalf("claim order=%v want tok2 先（最早到期）", f.claims)
	}
}

// 已过期的包不参与紧迫度：只有 expire_time > now 的包算。
func TestCheckinIgnoresExpiredPackageForOrdering(t *testing.T) {
	now := time.Now().Unix()
	// u1 唯一的包已过期，u2 有未来到期 → u2 更紧迫。
	f := &perAccountUpstream{expire: map[string]int64{"tok1": now - 86400, "tok2": now + 86400*30}}
	srv := f.server()
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "tok1", RefreshToken: "rt", ExpiresAt: 9999999999})
	p.Add(&auth.Auth{UID: "u2", AccessToken: "tok2", RefreshToken: "rt", ExpiresAt: 9999999999})

	up := &upstream.Client{HTTP: srv.Client(), AgentHost: srv.URL, UgHost: srv.URL, OAuthHost: srv.URL, ClientID: upstream.ClientID}
	New(Config{Pool: p, Upstream: up, CheckinHours: []int{9}}).RunCheckinNow()

	if len(f.claims) != 2 || f.claims[0] != "tok2" {
		t.Fatalf("claim order=%v want tok2 先（u1 的包已过期）", f.claims)
	}
}

// 自动轮次跳过签到冷却中的账号，避免对拥塞的上游反复重试。
func TestRunCheckinSkipsAccountsInCheckinCooldown(t *testing.T) {
	f := &fakeUpstream{resourceRemain: 500}
	srv := f.server()
	defer srv.Close()
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	p.CooldownCheckin("u1", 5*time.Minute, "congested")

	newTestScheduler(f, p, srv).RunCheckinNow()
	if got := f.checkinCalls.Load(); got != 0 {
		t.Fatalf("签到冷却中的账号应被跳过: status calls=%d", got)
	}
}

// 手动单账号签到是明确意图，不被自动轮次的冷却过滤挡住。
func TestManualCheckinIgnoresCheckinCooldown(t *testing.T) {
	f := &fakeUpstream{resourceRemain: 500}
	srv := f.server()
	defer srv.Close()
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	p.CooldownCheckin("u1", 5*time.Minute, "congested")

	if err := newTestScheduler(f, p, srv).CheckinUID("u1"); err != nil {
		t.Fatalf("manual checkin must run: %v", err)
	}
	if got := f.checkinCalls.Load(); got != 1 {
		t.Fatalf("status calls=%d want 1", got)
	}
}

// 周期余额刷新只更新余额，不解除冷却：否则熔断/降权/计划冷却每 5 分钟被清一次。
func TestBalanceRefreshDoesNotUnfreeze(t *testing.T) {
	f := &fakeUpstream{resourceRemain: 500}
	srv := f.server()
	defer srv.Close()
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	p.CooldownPlan("u1") // 计划冷却中

	s := newTestScheduler(f, p, srv)
	s.RunBalanceRefresh()

	st, _ := p.Status("u1")
	if !st.Cooling {
		t.Fatalf("余额刷新不该解冻: %+v", st)
	}
	if st.Credits != 500 {
		t.Fatalf("余额应被刷新: %+v", st)
	}
}
