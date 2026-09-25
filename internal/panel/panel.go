// Package panel 同进程管理页：账号池、模型、配置、日志。
package panel

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"traework2api/internal/pool"
	"traework2api/internal/scheduler"
	"traework2api/internal/upstream"
	"traework2api/internal/usage"
)

// Config 面板依赖。AuthDir 用进程启动时的目录，改配置里的路径要重启才生效。
type Config struct {
	Pool       *pool.Pool
	Upstream   *upstream.Client
	Scheduler  *scheduler.Scheduler
	AuthDir    string
	APIKey     string
	Version    string
	Logs       *Ring
	Models     func() []map[string]any
	Usage      *usage.Recorder // 可选，用量台账
	ConfigPath string
	LoadConfig func() (any, error)
	SaveConfig func(raw []byte) ([]string, error)

	// Listen 进程的监听地址（":7864"）。登录回跳没配 login.callback_url 时按它拼本机地址。
	Listen string
	// LoginCallback 可选：面板对外地址（配置里的 login.callback_url），登录回跳前缀。
	// 用 getter 而不是值——面板保存配置后要立刻生效，不重启。
	LoginCallback func() string
}

// Panel 挂在 /panel/ 下，pattern 使用完整路径。
type Panel struct {
	cfg     Config
	mux     *http.ServeMux
	started time.Time
	logs    *Ring

	// apiKey 随配置热改（面板保存配置后 ApplyAPIKey）。独立存一份而不是读 cfg.APIKey：
	// cfg 是 New 时复制进来的快照，配置改了它不会变。
	keyMu  sync.RWMutex
	apiKey string

	loginMu sync.Mutex
	logins  map[string]loginSession
}

// New 构建面板。Logs 为空时自建一个环，但 main 应传入和标准日志共用的那个。
func New(cfg Config) *Panel {
	if cfg.Logs == nil {
		cfg.Logs = NewRing(400)
	}
	p := &Panel{
		cfg:     cfg,
		apiKey:  cfg.APIKey,
		mux:     http.NewServeMux(),
		started: time.Now(),
		logs:    cfg.Logs,
		logins:  map[string]loginSession{},
	}
	p.mux.HandleFunc("GET /panel/{$}", p.index)
	p.mux.HandleFunc("GET /panel/app.js", p.appScript)
	p.mux.HandleFunc("GET /panel/api/overview", p.withAuth(p.overview))
	p.mux.HandleFunc("GET /panel/api/logs", p.withAuth(p.logsHandler))
	p.mux.HandleFunc("GET /panel/api/models", p.withAuth(p.models))
	p.mux.HandleFunc("GET /panel/api/usage", p.withAuth(p.usage))
	p.mux.HandleFunc("POST /panel/api/usage/save", p.withAuth(p.usageSave))
	p.mux.HandleFunc("GET /panel/api/config", p.withAuth(p.getConfig))
	p.mux.HandleFunc("POST /panel/api/config", p.withAuth(p.saveConfig))
	p.mux.HandleFunc("POST /panel/api/login/start", p.withAuth(p.loginStart))
	p.mux.HandleFunc("POST /panel/api/login/finish", p.withAuth(p.loginFinish))
	// 登录回跳：浏览器 302 过来，带不了 Authorization 头，所以这条**故意**不套 withAuth。
	// 安全边界是一次性 id（只有刚点过登录的会话认得）+ 15 分钟 TTL + 成功即删。
	p.mux.HandleFunc("GET /panel/oauth/callback/{id}", p.oauthCallback)
	p.mux.HandleFunc("POST /panel/api/checkin", p.withAuth(p.checkinAll))
	p.mux.HandleFunc("POST /panel/api/balance", p.withAuth(p.balanceAll))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/checkin", p.withAuth(p.accountCheckin))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/balance", p.withAuth(p.accountBalance))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/disable", p.withAuth(p.accountDisable))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/clear-cooldown", p.withAuth(p.accountClear))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/remove", p.withAuth(p.accountRemove))
	return p
}

func (p *Panel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)
	p.mux.ServeHTTP(w, r)
}

func (p *Panel) authorized(r *http.Request) bool {
	want := p.key()
	if want == "" {
		return isLoopback(r)
	}
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(h) < len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(h[len(prefix):]), []byte(want)) == 1
}

// key 当前生效的面板密钥（空 = 不鉴权）。
func (p *Panel) key() string {
	p.keyMu.RLock()
	defer p.keyMu.RUnlock()
	return p.apiKey
}

// ApplyAPIKey 换面板密钥，立即生效（面板保存配置改了 api_key 时调用）。
func (p *Panel) ApplyAPIKey(k string) {
	p.keyMu.Lock()
	p.apiKey = k
	p.keyMu.Unlock()
}

func isLoopback(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (p *Panel) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !p.authorized(r) {
			writeErr(w, http.StatusUnauthorized, "missing or invalid API key")
			return
		}
		next(w, r)
	}
}

// usagePanelHours 账号池「用量」列的统计窗口，与用量视图默认窗口一致。
const usagePanelHours = 72

// accountRow 账号状态 + 窗口内的用量汇总。嵌入后 JSON 平铺，前端仍按 uid/credits/... 读账号字段，
// 用量在 a.usage（无记录时为 null）。
type accountRow struct {
	pool.Status
	Usage *usage.Agg `json:"usage,omitempty"`
	// 累计口径（不受用量视图窗口影响），字段名与 wb 对齐：
	// 池级重试放大/坏号靠「成功 / 失败 + 在途（Status.InFlight）」一眼可见。
	SuccessCount int64     `json:"success_count,omitempty"`
	ErrTotal     int64     `json:"err_total,omitempty"`
	LastSuccess  time.Time `json:"last_success,omitempty"`
}

func (p *Panel) overview(w http.ResponseWriter, r *http.Request) {
	list := p.cfg.Pool.List()
	var healthy, cooling, disabled int
	for _, st := range list {
		switch {
		case st.Disabled:
			disabled++
		case st.Cooling:
			cooling++
		default:
			healthy++
		}
	}
	// 用量列与用量视图同源（同一份台账、同一窗口）：另存一份每账号累计计数器早晚会和台账
	// 对不上，届时运维得先分辨该信哪个。台账为空/无调用时 usage 缺席，前端显示「—」。
	rows := make([]accountRow, len(list))
	for i, st := range list {
		rows[i] = accountRow{Status: st}
	}
	for _, a := range p.cfg.Usage.Snapshot(usagePanelHours, p.nicks()).ByAccount {
		for i := range rows {
			if rows[i].UID != a.Key {
				continue
			}
			agg := a.Agg
			rows[i].Usage = &agg
			// 成功数 = 全历史请求 - 全历史失败：台账每次尝试都记一次，成败都由那一个
			// ok 标记决定，不另算一份计数器。
			if a.Life != nil {
				rows[i].SuccessCount = a.Life.Requests - a.Life.Errors
				rows[i].ErrTotal = a.Life.Errors
			}
			rows[i].LastSuccess = a.LastOK
			break
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"version":       p.cfg.Version,
		"uptime_sec":    int(time.Since(p.started).Seconds()),
		"auth_required": p.key() != "",
		"total":         len(list),
		"healthy":       healthy,
		"cooling":       cooling,
		"disabled":      disabled,
		"usage_hours":   usagePanelHours,
		"accounts":      rows,
	})
}

// nicks 池内 uid→昵称，仅供展示（不含任何凭证）。
func (p *Panel) nicks() map[string]string {
	nicks := map[string]string{}
	for _, st := range p.cfg.Pool.List() {
		if st.Nickname != "" {
			nicks[st.UID] = st.Nickname
		}
	}
	return nicks
}

func (p *Panel) logsHandler(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"entries": p.logs.Snapshot()})
}

func (p *Panel) models(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Models == nil {
		writeErr(w, http.StatusNotImplemented, "models unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": p.cfg.Models()})
}

// usage 返回逐请求用量聚合。hours 查询参数控制统计窗口（默认 72，上限 1440=60 天，
// 显式 hours=0 表示全部历史）：汇总卡片/按账号/按模型/时序**全部**按同一窗口统计。
func (p *Panel) usage(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Usage == nil {
		writeErr(w, http.StatusNotImplemented, "usage recorder not available")
		return
	}
	hours := 72
	if v := r.URL.Query().Get("hours"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			hours = n
		}
	}
	if hours > 1440 {
		hours = 1440
	}
	writeJSON(w, http.StatusOK, p.cfg.Usage.Snapshot(hours, p.nicks()))
}

// usageSave 立即把内存里的用量桶落盘（正常由后台 30s 防抖刷新负责）。
func (p *Panel) usageSave(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Usage == nil {
		writeErr(w, http.StatusNotImplemented, "usage recorder not available")
		return
	}
	p.cfg.Usage.Save()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (p *Panel) getConfig(w http.ResponseWriter, r *http.Request) {
	if p.cfg.LoadConfig == nil {
		writeErr(w, http.StatusNotImplemented, "config api not available")
		return
	}
	cfg, err := p.cfg.LoadConfig()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "load config failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "path": p.cfg.ConfigPath, "config": cfg})
}

func (p *Panel) saveConfig(w http.ResponseWriter, r *http.Request) {
	if p.cfg.SaveConfig == nil {
		writeErr(w, http.StatusNotImplemented, "config api not available")
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "read body failed")
		return
	}
	restart, err := p.cfg.SaveConfig(raw)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if restart == nil {
		restart = []string{}
	}
	log.Printf("panel: config saved restart_fields=%d", len(restart))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "restart_required": restart})
}

func (p *Panel) checkinAll(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Scheduler == nil {
		writeErr(w, http.StatusNotImplemented, "scheduler unavailable")
		return
	}
	if errs := p.cfg.Scheduler.RunCheckinNow(); len(errs) != 0 {
		writeErr(w, checkinErrorStatus(errs[0]), publicErr(errs[0]))
		return
	}
	p.overview(w, r)
}

func (p *Panel) balanceAll(w http.ResponseWriter, r *http.Request) {
	failed := 0
	for _, st := range p.cfg.Pool.List() {
		if err := p.refreshBalance(st.UID); err != nil {
			failed++
			log.Printf("panel: balance uid=%s failed", st.UID)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "failed": failed, "accounts": p.cfg.Pool.List()})
}

func (p *Panel) accountCheckin(w http.ResponseWriter, r *http.Request) {
	uid, ok := p.uidFrom(w, r)
	if !ok {
		return
	}
	if p.cfg.Scheduler == nil {
		writeErr(w, http.StatusNotImplemented, "scheduler unavailable")
		return
	}
	err := p.cfg.Scheduler.CheckinUID(uid)
	if errors.Is(err, scheduler.ErrDisabled) {
		writeErr(w, http.StatusConflict, "account disabled")
		return
	}
	if errors.Is(err, scheduler.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	if err != nil {
		writeErr(w, checkinErrorStatus(err), publicErr(err))
		return
	}
	st, _ := p.cfg.Pool.Status(uid)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "account": st})
}

func (p *Panel) accountBalance(w http.ResponseWriter, r *http.Request) {
	uid, ok := p.uidFrom(w, r)
	if !ok {
		return
	}
	if err := p.refreshBalance(uid); err != nil {
		code := http.StatusBadGateway
		if errors.Is(err, scheduler.ErrNotFound) {
			code = http.StatusNotFound
		}
		writeErr(w, code, publicErr(err))
		return
	}
	st, _ := p.cfg.Pool.Status(uid)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "account": st})
}

func (p *Panel) accountDisable(w http.ResponseWriter, r *http.Request) {
	uid, ok := p.uidFrom(w, r)
	if !ok {
		return
	}
	if _, exists := p.cfg.Pool.Status(uid); !exists {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	p.cfg.Pool.Disable(uid, "disabled from panel")
	log.Printf("panel: disabled uid=%s", uid)
	st, _ := p.cfg.Pool.Status(uid)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "account": st})
}

func (p *Panel) accountClear(w http.ResponseWriter, r *http.Request) {
	uid, ok := p.uidFrom(w, r)
	if !ok {
		return
	}
	st, exists := p.cfg.Pool.Status(uid)
	if !exists {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	if st.Disabled {
		writeErr(w, http.StatusConflict, "account disabled")
		return
	}
	if !p.cfg.Pool.ClearCooldown(uid) {
		writeErr(w, http.StatusConflict, "not cooling")
		return
	}
	log.Printf("panel: cooldown cleared uid=%s", uid)
	st, _ = p.cfg.Pool.Status(uid)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "account": st})
}

func (p *Panel) accountRemove(w http.ResponseWriter, r *http.Request) {
	uid, ok := p.uidFrom(w, r)
	if !ok {
		return
	}
	path, err := authPath(p.cfg.AuthDir, uid)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad uid")
		return
	}
	if _, found := p.cfg.Pool.Remove(uid); !found {
		if _, statErr := os.Stat(path); statErr != nil {
			writeErr(w, http.StatusNotFound, "account not found")
			return
		}
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		writeErr(w, http.StatusInternalServerError, "remove file failed")
		return
	}
	log.Printf("panel: removed uid=%s", uid)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (p *Panel) refreshBalance(uid string) error {
	a := p.cfg.Pool.AuthByUID(uid)
	if a == nil {
		return scheduler.ErrNotFound
	}
	remain, total, expire, err := p.cfg.Upstream.UserEntUsage(a)
	if err != nil {
		return err
	}
	p.cfg.Pool.SetCreditsExpire(uid, remain, total, expire)
	return nil
}

func (p *Panel) uidFrom(w http.ResponseWriter, r *http.Request) (string, bool) {
	uid := r.PathValue("uid")
	if !validUID(uid) {
		writeErr(w, http.StatusBadRequest, "bad uid")
		return "", false
	}
	return uid, true
}

var uidRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

func validUID(s string) bool { return uidRE.MatchString(s) }

// authPath 只允许 auths/trae-{uid}.json，拒绝路径穿越。
func authPath(dir, uid string) (string, error) {
	if dir == "" || !validUID(uid) {
		return "", fmt.Errorf("bad uid")
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	full := filepath.Join(absDir, "trae-"+uid+".json")
	rel, err := filepath.Rel(absDir, full)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("bad uid")
	}
	return full, nil
}

func checkinErrorStatus(err error) int {
	var be *upstream.BusinessError
	if errors.As(err, &be) {
		return http.StatusConflict
	}
	return http.StatusBadGateway
}

func publicErr(err error) string {
	if err == nil {
		return ""
	}
	var ue *upstream.Error
	if errors.As(err, &ue) {
		return fmt.Sprintf("upstream %s http %d", ue.Kind, ue.Status)
	}
	return err.Error()
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	raw, err := json.Marshal(v)
	if err != nil {
		http.Error(w, "encode failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"error": msg})
}
