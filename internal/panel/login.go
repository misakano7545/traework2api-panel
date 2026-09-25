package panel

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"traework2api/internal/auth"
	"traework2api/internal/upstream"
)

const (
	loginTTL = 15 * time.Minute
	// CallbackPath 登录回跳路径，后面接一次性登录会话 id（32 位 hex）。
	CallbackPath = "/panel/oauth/callback/"
)

// errLoginSession 登录会话不存在或过期。文案直接给浏览器看，不夹内部细节。
var errLoginSession = errors.New("登录链接已失效，请回面板重新点「登录账号」")

type loginSession struct {
	Machine string
	Device  string
	At      time.Time
}

type callbackCreds struct {
	Refresh  string
	Access   string
	UID      string
	Nickname string
	Ent      string
	Expires  int64
}

func randHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func loginURL(machine, device, callback string) (string, error) {
	trace, err := randHex(8)
	if err != nil {
		return "", err
	}
	q := url.Values{}
	q.Set("login_version", "1")
	q.Set("auth_from", "solo")
	q.Set("login_channel", "native_ide")
	q.Set("plugin_version", "2.3.62834")
	q.Set("auth_type", "local")
	q.Set("client_id", upstream.ClientID)
	q.Set("redirect", "0")
	q.Set("login_trace_id", trace)
	q.Set("auth_callback_url", callback)
	q.Set("machine_id", machine)
	q.Set("device_id", device)
	q.Set("x_device_id", device)
	q.Set("x_machine_id", machine)
	q.Set("x_device_brand", "PC")
	q.Set("x_device_type", "PC")
	q.Set("x_os_version", "1.0")
	q.Set("x_app_version", upstream.IdeVersion)
	q.Set("x_app_type", "stable")
	return "https://www.trae.cn/authorization?" + q.Encode(), nil
}

// callbackURL 登录回跳地址：配了 login.callback_url（面板对外地址）就用它——异机/隧道场景
// 必须填浏览器够得着的那一个；没配就按 listen 拼本机地址（面板和浏览器同机时直接可用）。
// 地址末尾的一次性 id 是这条路免鉴权的兜底：只有刚点过登录的那次会话认得它，15 分钟过期、成功即删。
func (p *Panel) callbackURL(id string) string {
	base := p.loginBase()
	if base == "" {
		base = "http://" + loopbackAddr(p.cfg.Listen)
	}
	return strings.TrimRight(base, "/") + CallbackPath + id
}

func (p *Panel) loginBase() string {
	if p.cfg.LoginCallback == nil {
		return ""
	}
	return strings.TrimSpace(p.cfg.LoginCallback())
}

// loopbackAddr listen → 浏览器同机时够得着的地址：":7864"/"0.0.0.0:7864" 都落到 127.0.0.1。
func loopbackAddr(listen string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "127.0.0.1:7864"
	}
	if ip := net.ParseIP(host); ip == nil || ip.IsUnspecified() {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}

func (p *Panel) loginStart(w http.ResponseWriter, r *http.Request) {
	machine, err := randHex(16)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "random failed")
		return
	}
	device, err := randHex(16)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "random failed")
		return
	}
	id, err := randHex(16)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "random failed")
		return
	}
	link, err := loginURL(machine, device, p.callbackURL(id))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "login url failed")
		return
	}
	p.loginMu.Lock()
	if p.logins == nil {
		p.logins = map[string]loginSession{}
	}
	now := time.Now()
	for k, s := range p.logins {
		if now.Sub(s.At) > loginTTL {
			delete(p.logins, k)
		}
	}
	p.logins[id] = loginSession{Machine: machine, Device: device, At: now}
	p.loginMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "url": link})
}

func (p *Panel) loginFinish(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<16+1))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "read body failed")
		return
	}
	if len(raw) > 1<<16 {
		writeErr(w, http.StatusRequestEntityTooLarge, "callback too long")
		return
	}
	var req struct {
		ID       string `json:"id"`
		Callback string `json:"callback"`
	}
	if json.Unmarshal(raw, &req) != nil || req.ID == "" {
		writeErr(w, http.StatusBadRequest, "missing login id")
		return
	}
	cb, err := parseCallback(req.Callback)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "callback missing refreshToken")
		return
	}
	uid, nick, code, err := p.completeLogin(req.ID, cb)
	if err != nil {
		writeErr(w, code, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "uid": uid, "nickname": nick})
}

// oauthCallback 登录回跳：浏览器从 trae.cn 302 过来带不了 Authorization 头，所以这条路免鉴权
// （不套 withAuth）。兜底就是地址里的一次性 id + loginTTL + 成功即删——和粘贴那条路共用同一个会话表，
// 不新增状态机。只回文案，绝不再吐出任何 token。
func (p *Panel) oauthCallback(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	cb, err := parseCallback(r.URL.String())
	if err != nil {
		p.callbackPage(w, http.StatusBadRequest, "没拿到登录凭据", "请回面板重新点「登录账号」。")
		return
	}
	uid, nick, code, err := p.completeLogin(id, cb)
	if err != nil {
		log.Printf("panel: oauth callback failed id=%s", id) // id 不是凭据；URL 本身（带 token）不进日志
		p.callbackPage(w, code, "登录未完成", err.Error())
		return
	}
	name := nick
	if name == "" {
		name = uid
	}
	p.callbackPage(w, http.StatusOK, "登录完成", "已加入："+name+"。可以关闭本页回面板了。")
}

// callbackPage 回调结果页。纯文案，不回显任何凭据。
func (p *Panel) callbackPage(w http.ResponseWriter, code int, title, detail string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_, _ = fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><title>%s</title>`+
		`<body style="font:15px/1.7 system-ui;margin:0;height:100vh;display:grid;place-items:center;background:#0f1115;color:#e8e8e8">`+
		`<div style="text-align:center;max-width:34em;padding:0 20px"><h1 style="font-size:19px;margin:0 0 8px">%s</h1>`+
		`<p style="margin:0;opacity:.72">%s</p></div>`,
		html.EscapeString(title), html.EscapeString(title), html.EscapeString(detail))
}

// completeLogin 换票 → GetUserInfo → 落盘 → 进池 → 删会话。粘贴回调和自动回调共用这一份内核，
// 所以两条路的行为（文件权限、去重、池热加载）永远一致。code 给调用方决定 HTTP 状态。
func (p *Panel) completeLogin(id string, cb callbackCreds) (uid, nick string, code int, err error) {
	p.loginMu.Lock()
	sess, ok := p.logins[id]
	if ok && time.Since(sess.At) > loginTTL {
		delete(p.logins, id)
		ok = false
	}
	p.loginMu.Unlock()
	if !ok {
		return "", "", http.StatusBadRequest, errLoginSession
	}
	if p.cfg.Upstream == nil {
		return "", "", http.StatusServiceUnavailable, errors.New("上游不可用")
	}
	a := &auth.Auth{
		RefreshToken: cb.Refresh,
		AccessToken:  cb.Access,
		ExpiresAt:    cb.Expires,
		MachineID:    sess.Machine,
		DeviceID:     sess.Device,
		Domain:       "trae.cn",
	}
	if cb.Refresh != "" {
		if err := p.cfg.Upstream.RefreshToken(a); err != nil {
			log.Printf("panel: login exchange failed id=%s", id)
			return "", "", http.StatusBadGateway, errors.New("换票失败：上游没接受这个 refreshToken")
		}
	} else if a.AccessToken == "" {
		return "", "", http.StatusBadRequest, errors.New("回调里没有 refreshToken")
	}
	uid, nick, ent, infoErr := p.cfg.Upstream.GetUserInfo(a)
	if infoErr != nil || uid == "" {
		uid = cb.UID
	}
	if nick == "" {
		nick = cb.Nickname
	}
	if ent == "" {
		ent = cb.Ent
	}
	if !validUID(uid) {
		log.Printf("panel: login rejected uid")
		return "", "", http.StatusBadRequest, errors.New("上游没返回可用的 UserID")
	}
	path, err := authPath(p.cfg.AuthDir, uid)
	if err != nil {
		return "", "", http.StatusBadRequest, errors.New("bad uid")
	}
	if err := os.MkdirAll(p.cfg.AuthDir, 0o755); err != nil {
		return "", "", http.StatusInternalServerError, errors.New("auth dir failed")
	}
	a.UID = uid
	a.Nickname = nick
	a.EnterpriseID = ent
	a.Domain = "trae.cn"
	a.ApiHost = upstream.OAuthHost
	a.FilePath = path
	if err := a.SaveAtomic(); err != nil {
		log.Printf("panel: login save failed uid=%s", uid)
		return "", "", http.StatusInternalServerError, errors.New("写入 auths 失败")
	}
	p.cfg.Pool.Add(a)
	p.loginMu.Lock()
	delete(p.logins, id)
	p.loginMu.Unlock()
	log.Printf("panel: account added uid=%s", uid)
	return uid, nick, http.StatusOK, nil
}

func parseCallback(raw string) (callbackCreds, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return callbackCreds{}, fmt.Errorf("empty")
	}
	u, err := url.Parse(raw)
	if err != nil || u.RawQuery == "" {
		return callbackCreds{}, fmt.Errorf("bad callback")
	}
	q := u.Query()
	var cb callbackCreds
	cb.Refresh = q.Get("refreshToken")
	if ui := parseJSONParam(q.Get("userInfo")); ui != nil {
		cb.UID = asString(ui["UserID"])
		cb.Nickname = asString(ui["ScreenName"])
		cb.Ent = asString(ui["TenantID"])
	}
	if jwt := parseJSONParam(q.Get("userJwt")); jwt != nil {
		cb.Access = asString(jwt["Token"])
		if cb.Refresh == "" {
			cb.Refresh = asString(jwt["RefreshToken"])
		}
		cb.Expires = asInt64(jwt["TokenExpireAt"])
		if cb.Expires > 1e12 {
			cb.Expires /= 1000
		}
	}
	if cb.Refresh == "" && cb.Access == "" {
		return callbackCreds{}, fmt.Errorf("missing refresh")
	}
	return cb, nil
}

func parseJSONParam(raw string) map[string]any {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	if m := decodeObj(raw); m != nil {
		return m
	}
	if u, err := url.QueryUnescape(raw); err == nil {
		return decodeObj(u)
	}
	return nil
}

func decodeObj(raw string) map[string]any {
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return nil
	}
	return m
}

func asString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case json.Number:
		return t.String()
	default:
		return ""
	}
}

func asInt64(v any) int64 {
	switch t := v.(type) {
	case json.Number:
		n, _ := t.Int64()
		return n
	default:
		return 0
	}
}
