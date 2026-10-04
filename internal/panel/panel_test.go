package panel

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
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
			_, _ = w.Write([]byte(`{"user_entitlement_pack_list":[{"entitlement_base_info":{"quota":{"credits_limit":500}},"usage":{"credits_amount":100}}]}`))
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

// 签到成功要如实回报本次到账积分——前端 toast 读的就是这个数，不能只回一句"签到完成"。
func TestCheckinAllReportsEarnedCredits(t *testing.T) {
	// claim 成功后 status 要报 checked_in=true：调度器现在有 claim 后复核（status→claim→status），
	// 假上游永远回 false 会让复核永远失败——假的就得照真实上游的序说话。
	var claimed atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/checkin_credits/status"):
			checked := "false"
			if claimed.Load() {
				checked = "true"
			}
			_, _ = w.Write([]byte(`{"checked_in":` + checked + `,"credits":150,"extra_credits":50,"enable":true}`))
		case strings.HasSuffix(r.URL.Path, "/checkin_credits/claim"):
			claimed.Store(true)
			_, _ = w.Write([]byte(`{"code":0,"message":"success"}`))
		case strings.HasSuffix(r.URL.Path, "/ide_user_ent_usage"):
			_, _ = w.Write([]byte(`{"user_entitlement_pack_list":[{"entitlement_base_info":{"quota":{"credits_limit":500}},"usage":{"credits_amount":100}}]}`))
		}
	}))
	defer srv.Close()
	pl := pool.New("")
	pl.Add(&auth.Auth{UID: "u1", Nickname: "甲", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := &upstream.Client{HTTP: srv.Client(), UgHost: srv.URL}
	sch := scheduler.New(scheduler.Config{Pool: pl, Upstream: up})
	p := New(Config{Pool: pl, Upstream: up, Scheduler: sch, APIKey: "k"})
	req := httptest.NewRequest(http.MethodPost, "/panel/api/checkin", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer k")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var resp struct {
		Checkin []struct {
			Nickname string `json:"nickname"`
			Status   string `json:"status"`
			Credits  int64  `json:"credits"`
		} `json:"checkin"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Checkin) != 1 || resp.Checkin[0].Status != "ok" ||
		resp.Checkin[0].Credits != 200 || resp.Checkin[0].Nickname != "甲" {
		t.Fatalf("回报不对: %+v（body=%s）", resp.Checkin, rec.Body)
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
		// 添加账号弹窗：复制链接旁的「在浏览器中打开」
		{"/panel/", `id="btnOpenUrl"`},
		{"/panel/app.js", "$('btnOpenUrl').onclick"},
	} {
		w := httptest.NewRecorder()
		p.ServeHTTP(w, httptest.NewRequest("GET", c.path, nil))
		if w.Code != 200 || !strings.Contains(w.Body.String(), c.want) {
			t.Fatalf("%s 缺少 %q (code=%d)", c.path, c.want, w.Code)
		}
	}
}

// 思考列渲染自检：真跑 node 里那份 thinkcell_check.mjs（它从 app.js 抠函数，不测副本）。
// 没装 node 就跳过——显示层断言不该挡住 go test。
func TestThinkCellRendering(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("没装 node，跳过思考列渲染自检")
	}
	if out, err := exec.Command("node", "thinkcell_check.mjs").CombinedOutput(); err != nil {
		t.Fatalf("thinkcell_check.mjs 失败: %v\n%s", err, out)
	}
}

func TestRingScrubsSecrets(t *testing.T) {
	r := NewRing(8)
	_, _ = r.Write([]byte("panel: Bearer super-secret-access\n"))
	_, _ = r.Write([]byte("http://127.0.0.1:18080/authorize?refreshToken=super-secret-refresh&x=1\n"))
	// 面板自己的回跳路径也得脱敏：只认参数名之后，任何路径都盖得住（旧正则只认 …/authorize?）。
	_, _ = r.Write([]byte("GET https://panel.example.com/panel/oauth/callback/deadbeef?userInfo=%7B%7D&refreshToken=super-secret-refresh\n"))
	_, _ = r.Write([]byte("panel: 首次运行 api_key = sk-AbCdEfGh1234567890XyZ\n"))
	got := r.Snapshot()
	raw, _ := json.Marshal(got)
	if strings.Contains(string(raw), "super-secret") {
		t.Fatal(string(raw))
	}
}

// 自动回调：TRAE 只认它自己的本机回调地址，所以登录链接里必须就是那一条（换任何别的地址
// 授权页直接「登录失败 / 网络错误」），面板拿不到回跳，只能靠粘贴。
func TestLoginLinkUsesTraeCallback(t *testing.T) {
	p := New(Config{Pool: pool.New(""), APIKey: "k"})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/panel/api/login/start", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer k")
	p.ServeHTTP(rec, req)
	var start struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &start); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(start.URL, url.QueryEscape(traeCallback)) {
		t.Fatalf("登录链接没带 TRAE 认的那条回调: %s", start.URL)
	}
}

// TestPackagesEndpoint 积分构成取数：逐账号结果 + 单号失败不拖累整页 + 无密钥 401。
// 用假上游（固定响应体）而不是真网络：这条要验的是面板的组装与错误隔离，不是上游连通性。
func TestPackagesEndpoint(t *testing.T) {
	up := upstream.New()
	up.HTTP = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(`{"user_entitlement_pack_list":[
			  {"display_desc":"老用户福利","group_name":"用户福利","expire_time":1792968453,
			   "entitlement_base_info":{"start_time":1790290053,"quota":{"credits_limit":4000}},
			   "usage":{"credits_amount":183.75}}
			]}`)),
		}, nil
	})}
	pl := pool.New("")
	pl.Add(&auth.Auth{UID: "u1", Nickname: "甲"})
	p := New(Config{Pool: pl, Upstream: up, APIKey: "k"})

	req := httptest.NewRequest("GET", "/panel/api/packages", nil)
	req.Header.Set("Authorization", "Bearer k")
	w := httptest.NewRecorder()
	p.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("code=%d body=%s", w.Code, w.Body)
	}
	var got struct {
		Accounts []struct {
			UID      string `json:"uid"`
			Remain   int64  `json:"remain"`
			Size     int64  `json:"size"`
			Packages []struct {
				Name string `json:"name"`
				Size int64  `json:"size"`
			} `json:"packages"`
			Error string `json:"error"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("不是 JSON: %v body=%s", err, w.Body)
	}
	if len(got.Accounts) != 1 || got.Accounts[0].UID != "u1" {
		t.Fatalf("accounts=%+v", got.Accounts)
	}
	if got.Accounts[0].Error != "" {
		t.Fatalf("这次不该有逐账号错误: %q", got.Accounts[0].Error)
	}
	if len(got.Accounts[0].Packages) != 1 || got.Accounts[0].Packages[0].Size != 4000 {
		t.Fatalf("逐包明细不对: %+v", got.Accounts[0])
	}
	if got.Accounts[0].Remain != 3817 || got.Accounts[0].Size != 4000 {
		t.Fatalf("聚合 remain=%d size=%d，期望 3817/4000（used 先截断为 183，再相减）", got.Accounts[0].Remain, got.Accounts[0].Size)
	}

	noKey := httptest.NewRecorder()
	p.ServeHTTP(noKey, httptest.NewRequest("GET", "/panel/api/packages", nil))
	if noKey.Code != http.StatusUnauthorized {
		t.Fatalf("未鉴权 code=%d", noKey.Code)
	}
}

// roundTripFunc 让单测把上游钉成固定响应（不碰真网络）。
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

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
