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
	"traework2api/internal/reqlog"
	"traework2api/internal/scheduler"
	"traework2api/internal/upstream"
	"traework2api/internal/usage"
)

// Config 面板依赖。AuthDir 用进程启动时的目录，改配置里的路径要重启才生效。
type Config struct {
	Pool      *pool.Pool
	Upstream  *upstream.Client
	Scheduler *scheduler.Scheduler
	AuthDir   string
	APIKey    string
	Version   string
	Logs      *Ring
	Models    func() []map[string]any
	Usage     *usage.Recorder // 可选，用量台账
	// RequestLog 可选，请求级指标/归档（运行日志页的「请求记录」）。nil = 该接口 501。
	RequestLog *reqlog.Recorder
	ConfigPath string
	LoadConfig func() (any, error)
	SaveConfig func(raw []byte) ([]string, error)
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
	p.mux.HandleFunc("GET /panel/api/request_logs", p.withAuth(p.requestLogs))
	p.mux.HandleFunc("GET /panel/api/models", p.withAuth(p.models))
	p.mux.HandleFunc("GET /panel/api/packages", p.withAuth(p.packages))
	p.mux.HandleFunc("GET /panel/api/usage", p.withAuth(p.usage))
	p.mux.HandleFunc("POST /panel/api/usage/save", p.withAuth(p.usageSave))
	p.mux.HandleFunc("GET /panel/api/config", p.withAuth(p.getConfig))
	p.mux.HandleFunc("POST /panel/api/config", p.withAuth(p.saveConfig))
	p.mux.HandleFunc("POST /panel/api/login/start", p.withAuth(p.loginStart))
	p.mux.HandleFunc("POST /panel/api/login/finish", p.withAuth(p.loginFinish))
	// refreshToken 直登：不经过浏览器把号加进来（授权页只认浏览器本机的 127.0.0.1 回调，
	// 公网回跳实测被拒，见 login.go 顶部——所以这条路是唯一的免粘贴方案）。
	p.mux.HandleFunc("POST /panel/api/login/refresh", p.withAuth(p.loginRefresh))
	p.mux.HandleFunc("POST /panel/api/checkin", p.withAuth(p.checkinAll))
	p.mux.HandleFunc("POST /panel/api/balance", p.withAuth(p.balanceAll))
	p.mux.HandleFunc("POST /panel/api/keepalive", p.withAuth(p.keepaliveAll))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/checkin", p.withAuth(p.accountCheckin))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/balance", p.withAuth(p.accountBalance))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/disable", p.withAuth(p.accountDisable))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/enable", p.withAuth(p.accountEnable))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/clear-cooldown", p.withAuth(p.accountClear))
	// 换 ug 族设备指纹（手动解 9074 风控标记）。
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/rotate-device", p.withAuth(p.accountRotateDevice))
	p.mux.HandleFunc("POST /panel/api/accounts/rotate-device-all", p.withAuth(p.rotateDeviceAll))
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
	// ug 族设备指纹（签到/积分用的那套，按 uid + 种子派生）：面板展示 + 「换指纹」用。
	// 9074 是设备维度风控，换一个设备号就能脱开旧标记（star620/TraeTools 的做法）。
	DeviceID   string `json:"device_id,omitempty"`
	DeviceSeed int64  `json:"device_seed,omitempty"`
}

func (p *Panel) overview(w http.ResponseWriter, r *http.Request) { p.writeOverview(w, nil) }

// writeOverview 面板首屏 JSON。extra 让动作类端点（签到）把「本次发生了什么」搭车带回：
// 前端一次请求就拿到账号状态 + 结果，不必再猜本次拿了多少分。
func (p *Panel) writeOverview(w http.ResponseWriter, extra map[string]any) {
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
		if a := p.cfg.Pool.AuthByUID(st.UID); a != nil {
			rows[i].DeviceID = upstream.UGDeviceID(a)
			rows[i].DeviceSeed = a.DeviceSeedValue()
		}
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
	body := map[string]any{
		"version":       p.cfg.Version,
		"uptime_sec":    int(time.Since(p.started).Seconds()),
		"auth_required": p.key() != "",
		"total":         len(list),
		"healthy":       healthy,
		"cooling":       cooling,
		"disabled":      disabled,
		"usage_hours":   usagePanelHours,
		"accounts":      rows,
	}
	for k, v := range extra {
		body[k] = v
	}
	writeJSON(w, http.StatusOK, body)
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

// packages 返回全部账号的积分包构成，供「积分构成」视图对比。
// 逐账号**实时**查上游（同 balanceAll 的口径），失败的单列出来不拖累其余账号——
// 一个号凭证过期不该让整页空白。国际版免费号没有积分套餐（只有一条 credits_limit=0 的
// Free plan），返回空列表 + 0/0，由前端显示「无积分套餐」。
func (p *Panel) packages(w http.ResponseWriter, r *http.Request) {
	type item struct {
		UID      string                   `json:"uid"`
		Nickname string                   `json:"nickname,omitempty"`
		Realm    string                   `json:"realm,omitempty"`
		Remain   int64                    `json:"remain"`
		Size     int64                    `json:"size"`
		Packages []upstream.CreditPackage `json:"packages"`
		Error    string                   `json:"error,omitempty"`
	}
	list := p.cfg.Pool.List()
	out := make([]item, 0, len(list))
	for _, st := range list {
		it := item{UID: st.UID, Nickname: st.Nickname, Realm: st.Realm, Packages: []upstream.CreditPackage{}}
		a := p.cfg.Pool.AuthByUID(st.UID)
		if a == nil {
			it.Error = "凭证不可用"
			out = append(out, it)
			continue
		}
		packs, remain, size, err := p.cfg.Upstream.CreditPackages(a)
		if err != nil {
			it.Error = err.Error()
			out = append(out, it)
			continue
		}
		it.Packages = packs
		it.Remain = remain
		it.Size = size
		out = append(out, it)
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": out})
}

// requestLogs 请求记录取数：优先读 JSONL 归档（可跨重启）；归档未开时回落到进程内最近 100 条
// metrics.recent——否则「归档默认关」会让这一页永远空白，看起来像功能没做。
// 筛选（关键字/结果）在前端做（一页最多 1000 条，零延迟）。
func (p *Panel) requestLogs(w http.ResponseWriter, r *http.Request) {
	if p.cfg.RequestLog == nil {
		writeErr(w, http.StatusNotImplemented, "request logger not available")
		return
	}
	limit := 200
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > 1000 {
		limit = 1000
	}
	// 筛选在服务端做（不是前端筛已拉取的条目）：时间区间落在更早的时段时，
	// 「最近 N 条」里根本没有那些记录，只能让归档按区间取。判据与归档读盘共用
	// reqlog.Filter，开/关归档两条路径口径一致。
	q := r.URL.Query()
	flt := reqlog.Filter{
		Outcome:   q.Get("outcome"),
		Account:   q.Get("account"),
		Model:     q.Get("model"),
		ClientIP:  q.Get("client_ip"),
		UserAgent: q.Get("user_agent"),
		From:      parseTimeParam(q.Get("from")),
		To:        parseTimeParam(q.Get("to")),
	}
	snap := p.cfg.RequestLog.Snapshot()
	entries, err := p.cfg.RequestLog.ReadArchive(limit, flt)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	// archived 报的是「归档是不是这次取数的来源」（=归档开着），不是「这次取到了几条」：
	// 带筛选条件时「区间内没有记录」是一个真实结果，按条数判断会把它错误地回落成
	// 内存里的最近 N 条，把筛选条件之外的请求显示出来。
	archived := snap.Archive.Enabled
	if !archived {
		// 归档未开：回落到进程内最近事件，同一套筛选条件照过（倒序与归档一致）。
		entries = entries[:0]
		for _, e := range snap.Recent {
			if flt.Match(e) {
				entries = append(entries, e)
			}
		}
		if len(entries) > limit {
			entries = entries[:limit]
		}
	}
	if entries == nil {
		entries = []reqlog.Event{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"metrics":  snap,
		"entries":  entries,
		"limit":    limit,
		"archived": archived,
	})
}

// parseTimeParam 解析时间区间参数：unix 秒 / unix 毫秒 / RFC3339 / 本地
// "2006-01-02T15:04"（datetime-local 的原始值）。空串或不可解析都返回零值，
// 零值在 reqlog.Filter 里表示「不过滤这一端」。
func parseTimeParam(v string) time.Time {
	v = strings.TrimSpace(v)
	if v == "" {
		return time.Time{}
	}
	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		if n <= 0 {
			return time.Time{}
		}
		if n > 1e12 {
			return time.UnixMilli(n)
		}
		return time.Unix(n, 0)
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t
	}
	if t, err := time.ParseInLocation("2006-01-02T15:04", v, time.Local); err == nil {
		return t
	}
	return time.Time{}
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
	results, errs := p.cfg.Scheduler.RunCheckinNow()
	if len(errs) != 0 {
		writeErr(w, checkinErrorStatus(errs[0]), publicErr(errs[0]))
		return
	}
	p.writeOverview(w, map[string]any{"checkin": results})
}

// keepaliveAll 立即对全账号刷一遍 token（保活）。不花积分：只刷新凭证，不调对话。
func (p *Panel) keepaliveAll(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Scheduler == nil {
		writeErr(w, http.StatusNotImplemented, "scheduler unavailable")
		return
	}
	p.cfg.Scheduler.RunRefreshNow()
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
	res, err := p.cfg.Scheduler.CheckinUID(uid)
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
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "status": res.Status, "earned": res.Credits, "account": st,
	})
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

// accountEnable 重新启用被禁用的账号（启用后立刻参与选号）。
func (p *Panel) accountEnable(w http.ResponseWriter, r *http.Request) {
	uid, ok := p.uidFrom(w, r)
	if !ok {
		return
	}
	old, exists := p.cfg.Pool.Status(uid)
	if !exists {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	if !old.Disabled {
		writeErr(w, http.StatusConflict, "account not disabled")
		return
	}
	if !p.cfg.Pool.Enable(uid) {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	log.Printf("panel: enabled uid=%s", uid)
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

// rotateDevice 给账号换一套 ug 族设备指纹（种子 +1）并落盘。
//
// 用途：9074「当前参与用户太多」是设备维度风控，换设备号能脱开旧标记。
// **只在用户点按钮时发生**：自动重试业务错误是钉过的红线（见 TestClaimNotRetriedOnBusinessError），
// 这里是显式的人工动作，语义上不是重试。
func (p *Panel) rotateDevice(uid string) error {
	a := p.cfg.Pool.AuthByUID(uid)
	if a == nil {
		return scheduler.ErrNotFound
	}
	a.SetDeviceSeed(a.DeviceSeedValue() + 1)
	if err := a.SaveAtomic(); err != nil {
		return err
	}
	log.Printf("panel: rotate device uid=%s seed=%d", uid, a.DeviceSeedValue())
	return nil
}

func (p *Panel) accountRotateDevice(w http.ResponseWriter, r *http.Request) {
	uid, ok := p.uidFrom(w, r)
	if !ok {
		return
	}
	if err := p.rotateDevice(uid); err != nil {
		code := http.StatusBadGateway
		if errors.Is(err, scheduler.ErrNotFound) {
			code = http.StatusNotFound
		}
		writeErr(w, code, publicErr(err))
		return
	}
	st, _ := p.cfg.Pool.Status(uid)
	dev := ""
	if a := p.cfg.Pool.AuthByUID(uid); a != nil {
		dev = upstream.UGDeviceID(a)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "account": st, "device_id": dev})
}

// rotateDeviceAll 一键给全部账号换指纹（风控按设备维度批量标记时用）。
func (p *Panel) rotateDeviceAll(w http.ResponseWriter, r *http.Request) {
	failed := 0
	for _, st := range p.cfg.Pool.List() {
		if err := p.rotateDevice(st.UID); err != nil {
			failed++
			log.Printf("panel: rotate device uid=%s failed: %v", st.UID, err)
		}
	}
	p.writeOverview(w, map[string]any{"rotated_failed": failed})
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
