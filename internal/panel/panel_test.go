package panel

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"traework2api/internal/auth"
	"traework2api/internal/pool"
	"traework2api/internal/scheduler"
	"traework2api/internal/upstream"
	"traework2api/internal/usage"
)

func TestSecurityAndAuth(t *testing.T) {
	p := New(Config{Pool: pool.New(""), APIKey: "test-key", Version: "test"})
	page := httptest.NewRecorder()
	p.ServeHTTP(page, httptest.NewRequest("GET", "/panel/", nil))
	if page.Code != 200 || !strings.Contains(page.Body.String(), `<script src="app.js"></script>`) {
		t.Fatalf("page code=%d body=%s", page.Code, page.Body.String())
	}
	if strings.Contains(page.Body.String(), "<script>") {
		t.Fatal("inline script")
	}
	csp := page.Header().Get("Content-Security-Policy")
	for _, must := range []string{"script-src 'self'", "frame-ancestors 'none'", "base-uri 'none'", "default-src 'none'"} {
		if !strings.Contains(csp, must) {
			t.Errorf("CSP missing %q: %s", must, csp)
		}
	}
	if page.Header().Get("X-Frame-Options") != "DENY" || page.Header().Get("Referrer-Policy") != "no-referrer" || page.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("headers: %v", page.Header())
	}

	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/api/overview", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no key code=%d", rec.Code)
	}
	if rec.Header().Get("Content-Security-Policy") == "" {
		t.Fatal("401 missing CSP")
	}
	bad := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/panel/api/overview", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	p.ServeHTTP(bad, req)
	if bad.Code != http.StatusUnauthorized {
		t.Fatalf("wrong key code=%d", bad.Code)
	}
}

func TestEmptyKeyLocalOnly(t *testing.T) {
	p := New(Config{Pool: pool.New("")})
	remote := httptest.NewRequest("GET", "/panel/api/overview", nil)
	remote.RemoteAddr = "10.1.1.1:9"
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, remote)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("remote code=%d", rec.Code)
	}
	local := httptest.NewRequest("GET", "/panel/api/overview", nil)
	local.RemoteAddr = "127.0.0.1:9"
	rec = httptest.NewRecorder()
	p.ServeHTTP(rec, local)
	if rec.Code != 200 {
		t.Fatalf("loopback code=%d body=%s", rec.Code, rec.Body)
	}
}

func TestOverviewOmitsTokens(t *testing.T) {
	pl := pool.New("")
	pl.Add(&auth.Auth{UID: "u1", Nickname: "n", AccessToken: "super-secret-access", RefreshToken: "super-secret-refresh"})
	pl.SetCreditsExpire("u1", 4940, 4941, 0)
	p := New(Config{Pool: pl, APIKey: "k"})
	req := httptest.NewRequest("GET", "/panel/api/overview", nil)
	req.Header.Set("Authorization", "Bearer k")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatal(rec.Body)
	}
	body := rec.Body.String()
	if strings.Contains(body, "super-secret") || strings.Contains(body, "accessToken") || strings.Contains(body, "refreshToken") {
		t.Fatal(body)
	}
	// 「积分」列显示 剩余/总额：总额要原样透出去，前端才有分母。
	if !strings.Contains(body, `"credits":4940`) || !strings.Contains(body, `"credits_total":4941`) {
		t.Fatal(body)
	}
}

func TestAuthPathAndRemove(t *testing.T) {
	dir := t.TempDir()
	for _, uid := range []string{"../x", `..\x`, "a/b", "", "..", "a b", "a.b", strings.Repeat("a", 65)} {
		if _, err := authPath(dir, uid); err == nil {
			t.Fatalf("uid %q accepted", uid)
		}
	}
	got, err := authPath(dir, "ok_1")
	if err != nil || filepath.Base(got) != "trae-ok_1.json" {
		t.Fatal(got, err)
	}
	keep := filepath.Join(dir, "other.json")
	os.WriteFile(keep, []byte("keep"), 0o600)
	target := filepath.Join(dir, "trae-ok_1.json")
	os.WriteFile(target, []byte(`{"auth":{"accessToken":"x"}}`), 0o600)
	pl := pool.New("")
	pl.Add(&auth.Auth{UID: "ok_1", FilePath: filepath.Join(dir, "..", "pwned.json")})
	p := New(Config{Pool: pl, AuthDir: dir, APIKey: "k"})
	req := httptest.NewRequest("POST", "/panel/api/accounts/a.b/remove", nil)
	req.Header.Set("Authorization", "Bearer k")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("dot uid code=%d", rec.Code)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatal("sentinel removed")
	}
	req = httptest.NewRequest("POST", "/panel/api/accounts/ok_1/remove", nil)
	req.Header.Set("Authorization", "Bearer k")
	rec = httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("remove %d %s", rec.Code, rec.Body)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatal("target still there")
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatal("other.json removed")
	}
	outside := filepath.Join(dir, "..", "pwned.json")
	if _, err := os.Stat(outside); !os.IsNotExist(err) {
		t.Fatal("escaped path")
	}
}

func TestLoginWrites0600AndHidesToken(t *testing.T) {
	const access = "super-secret-access"
	const refresh = "super-secret-refresh"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/ExchangeToken"):
			w.Write([]byte(`{"Result":{"Token":"` + access + `","RefreshToken":"` + refresh + `-new","TokenExpireAt":1786805537000}}`))
		case strings.HasSuffix(r.URL.Path, "/GetUserInfo"):
			w.Write([]byte(`{"Result":{"UserID":"user_1","ScreenName":"nick","EnterpriseID":"ent"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	up := upstream.New()
	up.HTTP = srv.Client()
	up.OAuthHost = srv.URL
	dir := t.TempDir()
	pl := pool.New("")
	p := New(Config{Pool: pl, Upstream: up, AuthDir: dir, APIKey: "k", Logs: NewRing(20)})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/panel/api/login/start", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer k")
	p.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatal(rec.Body)
	}
	var start struct {
		ID  string `json:"id"`
		URL string `json:"url"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &start); err != nil || !strings.Contains(start.URL, "machine_id=") {
		t.Fatal(rec.Body)
	}
	body, _ := json.Marshal(map[string]string{
		"id":       start.ID,
		"callback": "http://127.0.0.1:18080/authorize?refreshToken=" + refresh,
	})
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/panel/api/login/finish", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer k")
	p.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("finish %d %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), access) || strings.Contains(rec.Body.String(), refresh) {
		t.Fatal("response leaked token")
	}
	fp := filepath.Join(dir, "trae-user_1.json")
	fi, err := os.Stat(fp)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("perm %o", fi.Mode().Perm())
	}
	raw, _ := os.ReadFile(fp)
	if !strings.Contains(string(raw), access) {
		t.Fatal("file missing access token")
	}
	if pl.AuthByUID("user_1") == nil {
		t.Fatal("not hot-loaded")
	}
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/panel/api/overview", nil)
	req.Header.Set("Authorization", "Bearer k")
	p.ServeHTTP(rec, req)
	if strings.Contains(rec.Body.String(), access) || strings.Contains(rec.Body.String(), refresh) {
		t.Fatal("overview leaked")
	}
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/panel/api/logs", nil)
	req.Header.Set("Authorization", "Bearer k")
	p.ServeHTTP(rec, req)
	if strings.Contains(rec.Body.String(), access) || strings.Contains(rec.Body.String(), refresh) {
		t.Fatal("logs leaked")
	}
}

func TestLoginRejectsBadUID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/ExchangeToken"):
			w.Write([]byte(`{"Result":{"Token":"tok","RefreshToken":"rt","TokenExpireAt":1786805537}}`))
		case strings.HasSuffix(r.URL.Path, "/GetUserInfo"):
			w.Write([]byte(`{"Result":{"UserID":"../evil","ScreenName":"x"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	up := upstream.New()
	up.HTTP = srv.Client()
	up.OAuthHost = srv.URL
	dir := t.TempDir()
	p := New(Config{Pool: pool.New(""), Upstream: up, AuthDir: dir, APIKey: "k"})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/panel/api/login/start", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer k")
	p.ServeHTTP(rec, req)
	var start struct {
		ID string `json:"id"`
	}
	json.Unmarshal(rec.Body.Bytes(), &start)
	body, _ := json.Marshal(map[string]string{"id": start.ID, "callback": "http://127.0.0.1:18080/authorize?refreshToken=rt"})
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/panel/api/login/finish", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer k")
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d %s", rec.Code, rec.Body)
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	if len(matches) != 0 {
		t.Fatal(matches)
	}
}

func TestCheckinAllSurfacesUpstreamFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/checkin_credits/status"):
			_, _ = w.Write([]byte(`{"checked_in":false,"credits":150,"enable":true}`))
		case strings.HasSuffix(r.URL.Path, "/checkin_credits/claim"):
			_, _ = w.Write([]byte(`{"code":9074,"message":"当前参与用户太多，请稍后再试"}`))
		case strings.HasSuffix(r.URL.Path, "/ide_user_ent_usage"):
			_, _ = w.Write([]byte(`{"user_entitlement_pack_list":[]}`))
		}
	}))
	defer srv.Close()
	pl := pool.New("")
	pl.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := &upstream.Client{HTTP: srv.Client(), UgHost: srv.URL}
	sch := scheduler.New(scheduler.Config{Pool: pl, Upstream: up})
	p := New(Config{Pool: pl, Upstream: up, Scheduler: sch, APIKey: "k"})
	req := httptest.NewRequest(http.MethodPost, "/panel/api/checkin", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer k")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "当前参与用户太多") {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
}

// 用量端点：带鉴权可读聚合、昵称带出；记录器缺失时 501 而不是空数据；未鉴权 401。
func TestUsageEndpoint(t *testing.T) {
	rec := usage.New("")
	rec.Add(time.Now(), "u1", "glm-5.2", usage.Delta{
		PromptTokens: 3, HasPromptTokens: true,
		CompletionTokens: 4, HasCompletion: true,
		TotalTokens: 7, HasTotal: true,
	}, true)

	pl := pool.New("")
	pl.Add(&auth.Auth{UID: "u1", Nickname: "甲"})
	p := New(Config{Pool: pl, APIKey: "k", Usage: rec})

	req := httptest.NewRequest("GET", "/panel/api/usage?hours=72", nil)
	req.Header.Set("Authorization", "Bearer k")
	w := httptest.NewRecorder()
	p.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("code=%d body=%s", w.Code, w.Body)
	}
	var snap usage.Snapshot
	if err := json.Unmarshal(w.Body.Bytes(), &snap); err != nil {
		t.Fatalf("不是 JSON: %v body=%s", err, w.Body)
	}
	if snap.Totals.Requests != 1 || snap.Totals.TotalTokens != 7 || snap.Totals.PromptTokens != 3 {
		t.Fatalf("totals=%+v", snap.Totals)
	}
	if len(snap.ByAccount) != 1 || snap.ByAccount[0].Key != "u1" || snap.ByAccount[0].Extra != "甲" {
		t.Fatalf("by_account=%+v", snap.ByAccount)
	}

	noKey := httptest.NewRecorder()
	p.ServeHTTP(noKey, httptest.NewRequest("GET", "/panel/api/usage", nil))
	if noKey.Code != http.StatusUnauthorized {
		t.Fatalf("未鉴权 code=%d", noKey.Code)
	}

	none := New(Config{Pool: pool.New(""), APIKey: "k"})
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest("GET", "/panel/api/usage", nil)
	req2.Header.Set("Authorization", "Bearer k")
	none.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusNotImplemented {
		t.Fatalf("无记录器 code=%d", rec2.Code)
	}
}

func TestOverviewCarriesAccountUsage(t *testing.T) {
	rec := usage.New("")
	rec.Add(time.Now(), "u1", "glm-5.2", usage.Delta{
		PromptTokens: 3, HasPromptTokens: true,
		CompletionTokens: 4, HasCompletion: true,
		TotalTokens: 7, HasTotal: true,
	}, true)
	rec.Add(time.Now(), "u1", "glm-5.2", usage.Delta{HasTotal: true, TotalTokens: 5}, true)
	rec.Add(time.Now(), "u1", "glm-5.2", usage.Delta{}, false)

	pl := pool.New("")
	pl.Add(&auth.Auth{UID: "u1", Nickname: "甲"})
	pl.Add(&auth.Auth{UID: "u2", Nickname: "乙"})
	p := New(Config{Pool: pl, APIKey: "k", Usage: rec})

	req := httptest.NewRequest("GET", "/panel/api/overview", nil)
	req.Header.Set("Authorization", "Bearer k")
	w := httptest.NewRecorder()
	p.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("code=%d body=%s", w.Code, w.Body)
	}
	var got struct {
		UsageHours int `json:"usage_hours"`
		Accounts   []struct {
			UID          string     `json:"uid"`
			Usage        *usage.Agg `json:"usage"`
			SuccessCount int64      `json:"success_count"`
			ErrTotal     int64      `json:"err_total"`
			LastSuccess  time.Time  `json:"last_success"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("不是 JSON: %v body=%s", err, w.Body)
	}
	if got.UsageHours != usagePanelHours {
		t.Fatalf("usage_hours=%d", got.UsageHours)
	}
	byUID := map[string]*usage.Agg{}
	for _, a := range got.Accounts {
		byUID[a.UID] = a.Usage
	}
	// 有调用的账号带汇总；没调过的账号 usage 缺席（前端显示「—」，不是「0 次」）。
	if u := byUID["u1"]; u == nil || u.Requests != 3 || u.Errors != 1 || u.TotalTokens != 12 {
		t.Fatalf("u1 usage=%+v", u)
	}
	if byUID["u2"] != nil {
		t.Fatalf("u2 不该有用量: %+v", byUID["u2"])
	}
	// 三列（成功/失败 + 最近成功）：累计口径，来自台账的 Life/LastOK。
	for _, a := range got.Accounts {
		if a.UID != "u1" {
			continue
		}
		if a.SuccessCount != 2 || a.ErrTotal != 1 {
			t.Fatalf("成功/失败 = %d/%d，期望 2/1", a.SuccessCount, a.ErrTotal)
		}
		if a.LastSuccess.IsZero() {
			t.Fatal("缺最近成功时间")
		}
	}

	// 没接台账（nil）时 overview 仍可用，只是没有用量。
	noUsage := New(Config{Pool: pl, APIKey: "k"})
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest("GET", "/panel/api/overview", nil)
	req2.Header.Set("Authorization", "Bearer k")
	noUsage.ServeHTTP(rec2, req2)
	if rec2.Code != 200 || !strings.Contains(rec2.Body.String(), `"uid":"u1"`) {
		t.Fatalf("无台账 code=%d body=%s", rec2.Code, rec2.Body)
	}
	if strings.Contains(rec2.Body.String(), `"usage"`) {
		t.Fatalf("无台账不该出现 usage: %s", rec2.Body)
	}
}

// 账号池「用量」列的契约：表头一列 + 渲染函数在，避免后续重构把列静默丢掉。
func TestPanelServesUsageColumn(t *testing.T) {
	p := New(Config{Pool: pool.New(""), APIKey: "k"})
	for _, c := range []struct{ path, want string }{
		{"/panel/", "<th>用量</th>"},
		{"/panel/", "<th>成功 / 失败</th>"},
		{"/panel/", "<th>在途</th>"},
		{"/panel/", "<th>最近成功</th>"},
		{"/panel/app.js", "function ago("},
		{"/panel/app.js", "a.last_success"},
		{"/panel/app.js", "credits_total"},
		{"/panel/", ".cred .n .tot"},
		{"/panel/", "usage-cell"},
		{"/panel/app.js", "usageCell("},
		{"/panel/app.js", "usage_hours"},
		// 密钥可改 + 模型思考列：表头/字段名都在，别被后续重构静默丢掉。
		{"/panel/", `name="api_key"`},
		{"/panel/", "<th>思考</th>"},
		{"/panel/app.js", "function thinkCell("},
		{"/panel/app.js", "reasoning_effort_config"},
	} {
		w := httptest.NewRecorder()
		p.ServeHTTP(w, httptest.NewRequest("GET", c.path, nil))
		if w.Code != 200 || !strings.Contains(w.Body.String(), c.want) {
			t.Fatalf("%s 缺少 %q (code=%d)", c.path, c.want, w.Code)
		}
	}
}

func TestRingScrubsSecrets(t *testing.T) {
	r := NewRing(8)
	_, _ = r.Write([]byte("panel: Bearer super-secret-access\n"))
	_, _ = r.Write([]byte("http://127.0.0.1:18080/authorize?refreshToken=super-secret-refresh&x=1\n"))
	_, _ = r.Write([]byte("panel: 首次运行 api_key = sk-AbCdEfGh1234567890XyZ\n"))
	got := r.Snapshot()
	raw, _ := json.Marshal(got)
	if strings.Contains(string(raw), "super-secret") {
		t.Fatal(string(raw))
	}
}

// 面板改密钥后自己也要认新密钥，否则保存完立刻把自己挡在门外。
func TestApplyAPIKeyHotSwap(t *testing.T) {
	p := New(Config{Pool: pool.New(""), APIKey: "old"})
	call := func(k string) int {
		req := httptest.NewRequest("GET", "/panel/api/overview", nil)
		if k != "" {
			req.Header.Set("Authorization", "Bearer "+k)
		}
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		return rec.Code
	}
	if code := call("new"); code != http.StatusUnauthorized {
		t.Fatalf("改之前新密钥不该通过: %d", code)
	}
	p.ApplyAPIKey("new")
	if code := call("old"); code != http.StatusUnauthorized {
		t.Fatalf("旧密钥应失效: %d", code)
	}
	if code := call("new"); code != http.StatusOK {
		t.Fatalf("新密钥应通过: %d", code)
	}
}
