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
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"traework2api/internal/auth"
	"traework2api/internal/upstream"
)

const loginTTL = 15 * time.Minute

// traeCallback 缺省回调地址：TRAE 授权页认它自己 IDE 的这条。
//
// 实测过的四条（换别的地址页面直接「登录失败 / 网络错误，请刷新页面重试」）：
//   - https://<面板隧道域名>/panel/oauth/callback/<id>  ✗
//   - http://127.0.0.1:7864/panel/oauth/callback/test   ✗
//   - http://127.0.0.1:18080/panel/oauth/callback/test  ✗（端口对、路径不对也不行）
//   - http://127.0.0.1:18080/authorize                  ✓ 只有这条进得来登录页
//
// 注意这四条的对照里**只有成功那条的路径以 /authorize 结尾**（c3h3-ci/ai-proxy 的抓包结论
// 也是「授权页可向任意可达地址交付 token，与 host 无关」，它已推翻同类的「绑 127.0.0.1」判断）。
// 所以判据可能是**路径**而不是 host，尚未验证——loginStart 因此接受一个可选的 callback_base
// （http(s)://host[:port]），把回跳引到面板自己的 /panel/oauth/callback/<id>/authorize 上；
// 缺省不传，维持这条实测能用的地址，粘贴回跳那条路照旧。实验一次即可定论，别急着改缺省值。
const traeCallback = "http://127.0.0.1:18080/authorize"

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
	Host     string // 回调里的 host 参数：OAuth host，国际版靠它认（api-sg-central.trae.ai）
}

// intlHost 回调里的 host 参数是不是国际 OAuth host；是就返回 (domain, apiHost)，
// 不是（国内回调 / 老回调 / 测试里的假上游）返回空串，由调用方决定回落。
// ponytail: 判据与 auth.Realm() 同一套（trae.ai / byteintlapi 家族）。
func intlHost(host string) (string, string) {
	if strings.Contains(host, "trae.ai") || strings.Contains(host, "byteintlapi") {
		return "trae.ai", host
	}
	return "", ""
}

func randHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// loginURL 按地区生成登录链接：国际版同参数换域（www.trae.ai）+ 国际版客户端版本号。
// ponytail: 参数集与国内版逐项一致——实测国际版登录页认这套（client_id 两边相同）。
func loginURL(machine, device, realm, callback string) (string, error) {
	if callback == "" {
		callback = traeCallback
	}
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
	host, ver := "https://www.trae.cn", upstream.IdeVersion
	if realm == auth.RealmIntl {
		host, ver = "https://www.trae.ai", upstream.IdeVersionIntl
	}
	q.Set("x_app_version", ver)
	q.Set("x_app_type", "stable")
	return host + "/authorization?" + q.Encode(), nil
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
	// 面板可以带 {"realm":"intl"} 要一条国际版链接（缺省国内版）；
	// 也可以带 {"callback_base":"https://面板域名"} 要一条把回跳甩给面板自己的链接。
	realm := auth.RealmCN
	callback := traeCallback
	if raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<12)); len(raw) > 0 {
		var reqBody struct {
			Realm        string `json:"realm"`
			CallbackBase string `json:"callback_base"`
		}
		if json.Unmarshal(raw, &reqBody) == nil {
			if reqBody.Realm == auth.RealmIntl {
				realm = auth.RealmIntl
			}
			if b := strings.TrimSpace(reqBody.CallbackBase); b != "" {
				u, uerr := url.Parse(b)
				if uerr != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.RawQuery != "" || u.Path != "" {
					writeErr(w, http.StatusBadRequest, "callback_base 需形如 http(s)://host[:port]")
					return
				}
				callback = strings.TrimRight(b, "/") + "/panel/oauth/callback/" + id + "/authorize"
			}
		}
	}
	link, err := loginURL(machine, device, realm, callback)
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

// completeLogin 换票 → GetUserInfo → 落盘 → 进池 → 删会话。手动粘贴回调就走这一条路。
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
	// 国际账号的 OAuth host 来自回调的 host 参数——必须在换票之前落到 a.ApiHost 上，
	// RefreshToken（ExchangeToken）就是拿它当 base 的。国内回调留空，让客户端回落到
	// 自己的 OAuthHost（测试注入的假上游也是靠这个回落接上的）。
	a := &auth.Auth{
		RefreshToken: cb.Refresh,
		AccessToken:  cb.Access,
		ExpiresAt:    cb.Expires,
		MachineID:    sess.Machine,
		DeviceID:     sess.Device,
	}
	if domain, apiHost := intlHost(cb.Host); domain != "" {
		a.Domain, a.ApiHost = domain, apiHost
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
	a.Domain, a.ApiHost = "trae.cn", upstream.OAuthHost
	if domain, apiHost := intlHost(cb.Host); domain != "" {
		a.Domain, a.ApiHost = domain, apiHost
	}
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

// oauthCallback 公网回跳：授权页把浏览器跳回这里（回调地址 = callback_base + 本路径）。
//
// 无需面板密钥——浏览器跳转不带 Authorization 头；鉴权靠路径里那个一次性随机会话 id
// （randHex(16)，15 分钟过期，用后即删），猜不到就没法往池子里塞别人的号。
// 凭证解析与「粘贴回跳」共用 parseCallback + completeLogin，两条路行为完全一致。
func (p *Panel) oauthCallback(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	// 授权页可能 GET 回跳，也可能 POST 回来（表单里带参数）：一并合进 query 再解析。
	q := r.URL.Query()
	if r.Method == http.MethodPost {
		_ = r.ParseForm()
		for k, v := range r.PostForm {
			if q.Get(k) == "" && len(v) > 0 {
				q.Set(k, v[0])
			}
		}
	}
	cb, err := parseCallback(r.URL.Path + "?" + q.Encode())
	if err != nil {
		writeErr(w, http.StatusBadRequest, "回跳里没有 refreshToken")
		return
	}
	uid, nick, code, err := p.completeLogin(id, cb)
	if err != nil {
		writeErr(w, code, err.Error())
		return
	}
	log.Printf("panel: account added uid=%s via oauth callback", uid)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><title>登录完成</title>
<body style="font:15px system-ui;padding:2rem">账号 <b>%s</b>（%s）已加入账号池，可以关掉本页回到面板。</body>`,
		html.EscapeString(nick), html.EscapeString(uid))
}

// loginRefresh refreshToken 直登：不经过浏览器，把一条 refreshToken 直接换成凭证落盘。
//
// 与授权页回跳能不能落到本面板无关，是最稳的一条兜底（用户的 refreshToken 有效期以年计）。
// 换票、取用户信息、落盘、进池全部复用 completeLogin——只是凭证来源从「回跳参数」换成
// 「用户手输」，所以这里造一个一次性会话再调它。
func (p *Panel) loginRefresh(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<13))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "read body failed")
		return
	}
	var req struct {
		RefreshToken string `json:"refresh_token"`
		Realm        string `json:"realm"`
	}
	if json.Unmarshal(raw, &req) != nil {
		writeErr(w, http.StatusBadRequest, "bad body")
		return
	}
	rt := strings.TrimSpace(req.RefreshToken)
	if rt == "" || len(rt) > 4096 {
		writeErr(w, http.StatusBadRequest, "refresh_token 为空或过长")
		return
	}
	machine, err1 := randHex(16)
	device, err2 := randHex(16)
	id, err3 := randHex(16)
	if err1 != nil || err2 != nil || err3 != nil {
		writeErr(w, http.StatusInternalServerError, "random failed")
		return
	}
	p.loginMu.Lock()
	if p.logins == nil {
		p.logins = map[string]loginSession{}
	}
	p.logins[id] = loginSession{Machine: machine, Device: device, At: time.Now()}
	p.loginMu.Unlock()
	cb := callbackCreds{Refresh: rt}
	if req.Realm == auth.RealmIntl {
		// 国际版的换票 base 是它的 OAuth host（与回调 host 参数同一套判据）。
		cb.Host = upstream.UgHostIntl
	}
	uid, nick, code, err := p.completeLogin(id, cb)
	if err != nil {
		writeErr(w, code, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "uid": uid, "nickname": nick})
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
	cb.Host = q.Get("host")
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
