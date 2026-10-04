package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
