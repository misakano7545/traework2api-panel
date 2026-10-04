package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"traework2api/internal/pool"
	"traework2api/internal/upstream"
)

// loginFakeUpstream 假上游：ExchangeToken + GetUserInfo 两条（与 TestLoginWrites0600 同形）。
func loginFakeUpstream(t *testing.T) (*upstream.Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/ExchangeToken"):
			w.Write([]byte(`{"Result":{"Token":"at-new","RefreshToken":"rt-new","TokenExpireAt":1786805537}}`))
		case strings.HasSuffix(r.URL.Path, "/GetUserInfo"):
			w.Write([]byte(`{"Result":{"UserID":"user_1","ScreenName":"nick","EnterpriseID":"ent"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	up := upstream.New()
	up.HTTP = srv.Client()
	up.OAuthHost = srv.URL
	return up, srv
}

// TestLoginRefreshTokenDirect refreshToken 直登：不开浏览器，把一条 refreshToken 换成凭证
// 落盘进池（授权页回跳能不能落到面板与它无关，这是最稳的一条兜底）。
func TestLoginRefreshTokenDirect(t *testing.T) {
	up, srv := loginFakeUpstream(t)
	defer srv.Close()
	dir := t.TempDir()
	pl := pool.New("")
	p := New(Config{Pool: pl, Upstream: up, AuthDir: dir, APIKey: "k", Logs: NewRing(20)})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/panel/api/login/refresh",
		strings.NewReader(`{"refresh_token":"rt-from-user","realm":"cn"}`))
	req.Header.Set("Authorization", "Bearer k")
	p.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var out struct {
		OK  bool   `json:"ok"`
		UID string `json:"uid"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || !out.OK || out.UID != "user_1" {
		t.Fatalf("响应异常: %s (%v)", rec.Body, err)
	}
	if _, ok := pl.Status("user_1"); !ok {
		t.Error("账号没有进池")
	}
	if _, err := os.Stat(filepath.Join(dir, "trae-user_1.json")); err != nil {
		t.Errorf("auths 没有落盘: %v", err)
	}
	// 空 token 必须被挡（别拿空串去换票）
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest("POST", "/panel/api/login/refresh", strings.NewReader(`{"refresh_token":"  "}`))
	req2.Header.Set("Authorization", "Bearer k")
	p.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusBadRequest {
		t.Errorf("空 refreshToken 应 400，实得 %d", rec2.Code)
	}
}

// TestOAuthCallbackRoute 公网回跳路由：授权页把浏览器跳回 /panel/oauth/callback/{id}/authorize
// 时，凭证按与「粘贴回跳」完全相同的路径落盘；会话一次性（同一个 id 不能复用）。
func TestOAuthCallbackRoute(t *testing.T) {
	up, srv := loginFakeUpstream(t)
	defer srv.Close()
	dir := t.TempDir()
	pl := pool.New("")
	p := New(Config{Pool: pl, Upstream: up, AuthDir: dir, APIKey: "k", Logs: NewRing(20)})

	// 先开一次登录会话
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/panel/api/login/start", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer k")
	p.ServeHTTP(rec, req)
	var start struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &start); err != nil || start.ID == "" {
		t.Fatalf("拿不到会话 id: %s", rec.Body)
	}

	cb := "/panel/oauth/callback/" + start.ID + "/authorize?refreshToken=rt-cb&host=" + url.QueryEscape(srv.URL)
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest("GET", cb, nil) // 浏览器跳转：没有 Authorization 头
	p.ServeHTTP(rec2, req2)
	if rec2.Code != 200 {
		t.Fatalf("回跳应 200，实得 %d %s", rec2.Code, rec2.Body)
	}
	if !strings.Contains(rec2.Body.String(), "已加入账号池") {
		t.Errorf("回跳应回一个可读结果页: %s", rec2.Body)
	}
	if _, ok := pl.Status("user_1"); !ok {
		t.Error("回跳后账号没有进池")
	}
	// 会话一次性：同一 id 再来一次必须失败（别让一条回跳被反复重放）
	rec3 := httptest.NewRecorder()
	p.ServeHTTP(rec3, httptest.NewRequest("GET", cb, nil))
	if rec3.Code == 200 {
		t.Error("同一个会话 id 不该能重放")
	}
}

// TestLoginStartCallbackBase callback_base 只接受干净的 origin：带路径/查询串的地址会让
// 回跳落到意料之外的路径，直接拒掉比事后排查便宜。
func TestLoginStartCallbackBase(t *testing.T) {
	p := New(Config{Pool: pool.New(""), APIKey: "k", Logs: NewRing(20)})
	post := func(body string) (int, string) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/panel/api/login/start", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer k")
		p.ServeHTTP(rec, req)
		var out struct {
			ID  string `json:"id"`
			URL string `json:"url"`
		}
		json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out.URL
	}
	code, link := post(`{"callback_base":"https://panel.example.com"}`)
	if code != 200 {
		t.Fatalf("合法 callback_base 应 200，实得 %d", code)
	}
	if !strings.Contains(link, url.QueryEscape("/panel/oauth/callback/")) || !strings.Contains(link, url.QueryEscape("/authorize")) {
		t.Errorf("回跳应指向面板自己的 /panel/oauth/callback/<id>/authorize: %s", link)
	}
	for _, bad := range []string{`{"callback_base":"panel.example.com"}`, `{"callback_base":"ftp://x"}`, `{"callback_base":"https://x/y"}`, `{"callback_base":"https://x?a=1"}`} {
		if code, _ := post(bad); code != http.StatusBadRequest {
			t.Errorf("%s 应 400，实得 %d", bad, code)
		}
	}
	// 不传 callback_base → 仍是 TRAE 认的那条 127.0.0.1 回调（默认行为不变）
	if _, link := post(`{}`); !strings.Contains(link, url.QueryEscape(traeCallback)) {
		t.Errorf("缺省应保持 TRAE 那条回调: %s", link)
	}
}
