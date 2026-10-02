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

// traeCallback TRAE 授权页只认它自己 IDE 的这条回调地址，写死，不做成配置项。
//
// 实测（换任何别的地址都被拒，页面直接「登录失败 / 网络错误，请刷新页面重试」）：
//   - https://<面板隧道域名>/panel/oauth/callback/<id>  ✗
//   - http://127.0.0.1:7864/panel/oauth/callback/test   ✗
//   - http://127.0.0.1:18080/panel/oauth/callback/test  ✗（端口对、路径不对也不行）
//   - http://127.0.0.1:18080/authorize                  ✓ 只有这条进得来登录页
//
// 所以面板拿不到回跳，只能把浏览器地址栏粘回面板换票；做成可配置项只会再走进一次这个死胡同。
// 想免粘贴，得在**浏览器那台机器**上跑个监听 18080 的本机中继（面板在服务器上时它够不着）。
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
