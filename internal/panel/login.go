package panel

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
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
// 判据是**整条 URL 精确匹配**，2026-10-04 用无头浏览器渲染授权页实测（同一条链接只换
// auth_callback_url，看页面是登录页还是「登录失败 网络错误」）：
//
//	http://127.0.0.1:18080/authorize                                    ✓
//	https://127.0.0.1:18080/authorize                                   ✗（只换了 scheme）
//	http://127.0.0.1:18080/panel/oauth/callback/<id>/authorize          ✗（host+端口对，路径不对）
//	https://trae.misakano.kdns.fr/authorize                             ✗（路径对，host 不对）
//	https://trae.misakano.kdns.fr/panel/oauth/callback/<id>/authorize   ✗
//
// 另跑一次对照证明「改 URL 就拒」不成立（只动无关的 login_trace_id 仍进登录页），所以上面
// 几条的差异确实来自回调地址本身。
//
// 结论：公网回跳在架构上做不到——回调必须是**浏览器那台机器**的 127.0.0.1:18080/authorize，
// 面板在服务器上，永远不可能是那个 host。想免粘贴只有两条：① 浏览器所在机器跑个本机中继
// （把 127.0.0.1:18080/authorize 收到的查询串转给面板）；② **refreshToken 直登**
// （见 loginRefresh，已实现，最稳）。粘贴回跳那条路保持可用。
//
// 注：c3h3-ci/ai-proxy 的「授权页可向任意可达地址交付 token」来自它另一条登录入口
// （账号密码登录把凭证 POST 到它自己的回调域 mon.zijieapi.com），**不适用于**这里的
// IDE 授权页 auth_callback_url，别照搬。
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
func loginURL(machine, device, realm string) (string, error) {
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
	q.Set("auth_callback_url", traeCallback)
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
	// 面板可以带 {"realm":"intl"} 要一条国际版链接（缺省国内版）。
	realm := auth.RealmCN
	if raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<12)); len(raw) > 0 {
		var reqBody struct {
			Realm string `json:"realm"`
		}
		if json.Unmarshal(raw, &reqBody) == nil && reqBody.Realm == auth.RealmIntl {
			realm = auth.RealmIntl
		}
	}
	link, err := loginURL(machine, device, realm)
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
