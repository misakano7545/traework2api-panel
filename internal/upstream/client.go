// client.go SOLO 上游客户端：llm_utils_chat / get_detail_param / ExchangeToken /
// checkin_credits / ide_user_ent_usage + 错误分类。
package upstream

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"traework2api/internal/auth"
)

// ErrKind 错误分类，pool 据此决定冷却时长（SPEC §4.3）。
type ErrKind int

const (
	ErrNone        ErrKind = iota // 成功
	ErrPlanLimit                  // 1005 + plan → 权益不足（硬冷却 12h）
	ErrSoftRate                   // 429 → 短冷却 60s
	ErrSessionDead                // 401 + Cloud-IDE-JWT 失效 → 禁用
	ErrNotFound                   // 404 → 短冷却 60s 不累计 errCount
	ErrServer                     // 5xx
	ErrClient                     // 其他 4xx
)

// ErrNoIntlUG 国际版没有**签到**接口（实测两个国际域上 EpCheckin* 全 404）。
// 积分/套餐有接口，走 v1 路径（见 EpEntUsageIntl），不要拿这个错误去挡它。
// 返回值语义：签到布尔 false、额度 0，调用方据此跳过即可，不要当失败重试。
var ErrNoIntlUG = errors.New("国际版没有签到/积分接口")

// ErrCheckinAlready 上游 claim 回 9095（今日已签到）：这是一次幂等竞态，不是失败。
// 与 status 的 checked_in 同义，调用方当「已签」处理——当成失败会让面板显示
// 「签到失败：今日已签到」这种自相矛盾的结果（照 wild-work 的 DailyCheckin）。
var ErrCheckinAlready = errors.New("今日已签到")

// checkinAlreadyCode 签到 claim 的「今日已签到」业务码。
// ⚠️ 它**不能**当作「本账号已签」：上游的信息是**设备维度**的——同一台设备可能已经替
// 另一个账号签过（Trae2api-cn src/trae_client.py 原注释：only the account status
// endpoint can establish whether this account is checked in）。调用方据此换设备指纹，
// 再由 status 复核定论。
const checkinAlreadyCode = 9095

// checkinRateLimitCode 签到 claim 的「当前参与用户太多」：与 9095 一样是设备维度信号，
// 换设备指纹即可脱开；**不要**秒级内重试——上游原注释说第二次 claim 会延长限流窗口。
const checkinRateLimitCode = 9074

// IsCheckinRateLimited 报告错误是不是签到限流（9074）。
func IsCheckinRateLimited(err error) bool {
	var be *BusinessError
	return errors.As(err, &be) && be.Code == checkinRateLimitCode
}

// checkinMsg 取 message/msg 两个键里先出现的那个（上游两个键都出现过）。
func checkinMsg(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}

func (k ErrKind) String() string {
	switch k {
	case ErrPlanLimit:
		return "plan_limit"
	case ErrSoftRate:
		return "soft_rate"
	case ErrSessionDead:
		return "session_dead"
	case ErrNotFound:
		return "not_found"
	case ErrServer:
		return "server"
	case ErrClient:
		return "client"
	default:
		return "none"
	}
}

// Error 带分类的上游错误。
type Error struct {
	Kind   ErrKind
	Status int
	Msg    string
}

func (e *Error) Error() string {
	return fmt.Sprintf("upstream %s (http %d): %s", e.Kind, e.Status, e.Msg)
}

// BusinessError 是 HTTP 成功但上游业务拒绝。
type BusinessError struct {
	Code    int
	Message string
}

func (e *BusinessError) Error() string {
	return fmt.Sprintf("%s (code %d)", e.Message, e.Code)
}

// Classify 按 HTTP 状态码 + body 判定错误类别（SPEC §4.3）。
func Classify(status int, body string) ErrKind {
	lower := strings.ToLower(body)
	// session 失效：401 一律算 session 死。曾经拿 body 标记（login/session/unauthorized…）
	// 再判一次，但两个分支返回同一个 ErrKind，那段是死代码；是否带标记都不改变结果（TestClassify 两条 401）。
	if status == http.StatusUnauthorized {
		return ErrSessionDead
	}
	// 传输层的信号优先，别被 body 里的业务码抢走：401/429/404/5xx 说的就是「这次没成」
	// 的形态（500 里带 quota 文案仍该按服务端故障处理）。1005/plan 那类业务码也排在这后面。
	if status == http.StatusTooManyRequests {
		return ErrSoftRate
	}
	if status == http.StatusNotFound {
		return ErrNotFound
	}
	if status >= 500 {
		return ErrServer
	}
	// 1005 plan 权益不足（业务码，排在传输层之后：429/5xx 的 body 里带 1005 文案时按传输层算，
	// 否则一次限流会被当成权益不足罚成 12h 硬冷却）。判据取结构化码，不扫文案里的数字：
	// 111005 那种「数字里含 1005」的未知码会被误判成权益不足。
	if bodyBusinessCode(body) == 1005 || strings.Contains(body, `"code":1005`) {
		return ErrPlanLimit
	}
	// 业务码胜过「4xx 一律 Client」：同一个码走 HTTP 状态与走 SSE 流内事件是同一套上游
	// 语义的两种到达方式，落进不同的池状态就会出怪事 —— 4008 走流内是软冷却、走 HTTP 403
	// 却是降权，就是这么来的。分类表复用流内那张（soloSpecificKind），认不出的不算。
	// 不拿「有没有结构化业务码」当门槛：流内是无条件查表的，这边要求 code != 0 会让同一段
	// 文案在两条路上分到不同的池动作（取不到码就传 0，让两张表的文字判据对称）。
	if k, ok := soloSpecificKind(int64(bodyBusinessCode(body)), body); ok {
		return k
	}
	// 调用方侧的「输入过大」：罚号没有意义（同 4026 那条的教训 —— 一个客户端发超长
	// prompt，重试一圈就把轮转过的每个号都放倒）。上游 413 或几种文案都算。
	if status == http.StatusRequestEntityTooLarge || inputTooLarge(lower) {
		return ErrNone
	}
	if status >= 400 {
		return ErrClient
	}
	return ErrNone
}

// bodyBusinessCode 取上游错误信封里的业务码（`code` 与 `Code` 两种写法，值可能是数字
// 也可能是数字字符串）。0 = 没有或认不出（认不出就当没有，交给 HTTP 状态判）。
func bodyBusinessCode(body string) int {
	var m map[string]any
	if json.Unmarshal([]byte(body), &m) != nil {
		return 0
	}
	for _, k := range []string{"code", "Code"} {
		switch v := m[k].(type) {
		case float64:
			return int(v)
		case string:
			if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
				return n
			}
		}
	}
	return 0
}

// inputTooLargeMarkers 调用方侧「输入过大」的文案（上游几种写法都认）。
// 故意不含宽泛的 "exceeded"/"rate"：那两块也出现在 4008 的额度文案里（"your requests
// have exceeded the quota"），归错就变成给一个限流号免罚。
var inputTooLargeMarkers = []string{
	"context length", "maximum context length", "prompt is too long",
	"input is too large", "input too large", "too many tokens",
}

func inputTooLarge(lower string) bool {
	for _, m := range inputTooLargeMarkers {
		if strings.Contains(lower, m) {
			return true
		}
	}
	return false
}

// Client SOLO 上游 HTTP 客户端。Host 字段可覆盖便于测试。
type Client struct {
	// HTTP 用于短 JSON 请求（ExchangeToken/模型/签到/积分），有总超时兜底。
	HTTP *http.Client
	// StreamHTTP 用于 SSE 流式对话：不设总超时，避免长流被截断；
	// 通过 Transport.ResponseHeaderTimeout 兜底「上游一直不返回首字节」的悬挂。
	// 与 HTTP 共享同一 Transport（连接池复用）。nil 时 ChatStream 回退 HTTP。
	StreamHTTP *http.Client

	// idleTimeout 聊天 SSE 流内空闲上限（0 = 不看门狗）。由 SetTimeouts 更新。
	idleTimeout time.Duration

	// timeoutMu 保护上面三个超时（HTTP.Timeout / Transport.ResponseHeaderTimeout /
	// idleTimeout）。面板热改配置会调 SetTimeouts，并发请求正在 Do —— 直接改
	// http.Client 结构体字段就是数据竞争。请求路径持**读锁跨过 Do**：Do 返回即释放，
	// body 读取不碰这些字段（ResponseHeaderTimeout 只在等响应头时被读）。
	// ponytail: 写锁要等一个在途 Do 落地；保存配置是人工低频动作，够用。
	timeoutMu sync.RWMutex

	AgentHost string // https://trae-api-cn.mchost.guru
	// AgentHostIntl 国际版（realm=intl）的 agent host；空则用内置默认。
	AgentHostIntl string // https://a0ai-api-sg.byteintlapi.com
	UgHost        string // https://api.trae.cn
	OAuthHost     string // https://api.trae.com.cn
	ClientID      string // en1oxy7wnw8j9n

	// ── agent 基址的动态发现（见 domains.go）────────────────────────────
	// hostMu 保护下面四个：DiscoverHosts 在后台写，请求路径并发读。
	// 与 timeoutMu 分开：那组是热改超时用的，这组是域名表刷新用的，锁的粒度与
	// 持锁时长都不一样（发现持锁仅覆盖赋值，不跨网络请求）。
	hostMu       sync.RWMutex
	dynAgentCN   string    // 发现出来的国服 agent 基址（空 = 未发现，回落常量）
	dynAgentIntl string    // 同上，国际版
	dynAgentAt   time.Time // 最近一次成功写入的时刻（面板展示）

	// discoveryCli 发现专用 HTTP 客户端（短超时）。nil 时按内置短超时新建，
	// 不受热改的 120s 运行期超时影响，也不占用 HTTP 那套长超时语义。
	discoveryCli *http.Client

	// BalanceCli 余额/积分包查询专用客户端：独立 Transport + 短超时，
	// 不被 SSE 长流的连接池占满饿死（packages / balance refresh 在高并发
	// chat 期间曾被堵到 30s+ 超时，4 个号全失败）。
	// 公开字段：测试要往这塞自定义 Transport（同 HTTP/StreamHTTP 的惯例）。
	BalanceCli *http.Client
}

// New 生产默认值。配置连接池减少 TLS 握手。
func New() *Client {
	tr := &http.Transport{
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   20,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: 120 * time.Second, // 首字节兜底（长推理预留），不限制整流时长
	}
	balanceTr := &http.Transport{
		MaxIdleConns:          16,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       60 * time.Second,
		ResponseHeaderTimeout: 15 * time.Second,
	}
	return &Client{
		HTTP:          &http.Client{Timeout: 120 * time.Second, Transport: tr},
		StreamHTTP:    &http.Client{Transport: tr}, // 无总超时
		BalanceCli:    &http.Client{Timeout: 15 * time.Second, Transport: balanceTr},
		AgentHost:     AgentHost,
		AgentHostIntl: AgentHostIntl,
		UgHost:        UgHost,
		OAuthHost:     OAuthHost,
		ClientID:      ClientID,
	}
}

// agentBase 显式配置的 agent 基址（与自动发现字段共用 hostMu：applyUpstream 会热改它）。
func (c *Client) agentBase() string {
	c.hostMu.RLock()
	defer c.hostMu.RUnlock()
	return c.AgentHost
}

// SetAgentHosts 热改显式 agent 基址。写侧必须走这里：请求路径在并发读这两个字段
// （applyUpstream 直接赋值会与读侧构成数据竞争，-race 实测报 DATA RACE）。
func (c *Client) SetAgentHosts(cn, intl string) {
	c.hostMu.Lock()
	c.AgentHost, c.AgentHostIntl = cn, intl
	c.hostMu.Unlock()
}

func (c *Client) ugBase() string    { return c.UgHost }
func (c *Client) oauthBase() string { return c.OAuthHost }

// agentBaseFor 按账号地区选 agent host（对话 + 模型表都走这里）。
// 优先用 domains.go 动态发现出来的具体后端，其次显式配置，最后内置常量。
// Auth 为 nil（如配置校验路径）按国内版。
func (c *Client) agentBaseFor(a *auth.Auth) string {
	realm := auth.RealmCN
	if a != nil {
		realm = a.Realm()
	}
	return c.effectiveAgentBase(realm)
}

// identFor 账号地区对应的身份版本组（User-Agent / X-Ide-Version / *-Version-Code）。
//
// 实现已收敛到 identity.go：这里只做「地区 → 版本对」的转发，别再复制一份判断逻辑
// （历史上出现过 headers 与登录各写一套、改了一处漏一处）。
func identFor(a *auth.Auth) (version, versionCode string) {
	realm := realmOf(a)
	return currentIdeVersionFor(realm), verCodeFor(realm)
}

// maxJSONBody 上游 JSON 响应上限。8MB：模型目录表（batch_get_detail_param）随模型数
// 单调增长；早先 1MB 会把「响应变大」伪装成 json.Unmarshal 的 "unexpected end of
// JSON input"，日志里分不清是截断还是上游坏了（同 workbuddy-3in1 的补丁 0005）。
const maxJSONBody = 8 << 20

// doJSON 发请求并解 JSON；HTTP 非 2xx 时返回带 body 片段的 *Error。
func (c *Client) doJSON(req *http.Request) (json.RawMessage, error) {
	c.timeoutMu.RLock()
	resp, err := c.HTTP.Do(req)
	c.timeoutMu.RUnlock()
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	// 多读 1 字节判定超限：截断的 body 交给 json.Unmarshal 只会得到语法错误，
	// 且读取过程中断（连接被掐）也必须报出来，不能当成「上游返回了半截 JSON」。
	raw, rerr := io.ReadAll(io.LimitReader(resp.Body, maxJSONBody+1))
	if rerr != nil {
		return nil, fmt.Errorf("读取上游响应中断（%d 字节已收）: %w", len(raw), rerr)
	}
	if len(raw) > maxJSONBody {
		return nil, fmt.Errorf("上游响应超过 %d 字节上限，放弃解析", maxJSONBody)
	}
	if resp.StatusCode >= 400 {
		kind := Classify(resp.StatusCode, string(raw))
		return nil, &Error{Kind: kind, Status: resp.StatusCode, Msg: truncate(string(raw), 200)}
	}
	return raw, nil
}

// RefreshToken 通过 ExchangeToken 强制刷新 access token（refreshToken 轮换）。
// 成功时更新 a 的字段；调用方负责 SaveAtomic。全程持 a 写锁。
func (c *Client) RefreshToken(a *auth.Auth) error {
	a.Lock()
	defer a.Unlock()
	return c.refreshLocked(a)
}

// RefreshTokenIfNeeded 仅当 token 在 skew 内即将过期（或已过期）时才刷新，
// 返回是否真正刷新。持锁内重查，避免并发请求对同一账号重复 ExchangeToken 轮换。
// 调用方仅在 returned 为 true 时需要 SaveAtomic。
func (c *Client) RefreshTokenIfNeeded(a *auth.Auth, skew time.Duration) (bool, error) {
	a.Lock()
	defer a.Unlock()
	if !a.NeedsRefreshLocked(skew) {
		return false, nil
	}
	if err := c.refreshLocked(a); err != nil {
		return false, err
	}
	return true, nil
}

// exchangeRejectHints 换票被拒的业务码 → 一句能直接给人看的处置。
//
// 真因藏在 4xx 响应体的 ResponseMetadata.Error.Code（有用原话在 Data.__Message.error，
// Message 只是参数模板，形如 `无效参数：{__Message.field}.`）；只抄一句「HTTP 400」等于把
// 「凭据该重录」说成「网络抖动」，处置完全不同。码值与解释来自 chevy222/app-cf-checkin
// 与 L0NE-6/Trae-AutoCheckin 的真机实测对照。
var exchangeRejectHints = map[string]string{
	"10101": "refreshToken 与换票的客户端不匹配（旧版取票或 SOLO 线签发的票；同一账号被别处轮换过也会），重新登录取一次凭据即可",
	"20101": "refreshToken 已失效或被轮换掉（同一账号别在多处同时刷新），重新登录取一次凭据即可",
	"20403": "refreshToken 与设备不匹配（票不是这份设备资料签发的），重新登录取一次凭据即可",
	"20405": "上游要求设备证明（新版换票校验），当前请求没有设备签名，重新登录也解决不了",
}

// exchangeRejectHint 从 4xx 响应体里认出上面那几个码，认不出返回空。
// 只认带引号的形态（`"Code":"10101"`），避免别的数字串里恰好含这几位。
func exchangeRejectHint(body string) string {
	for code, hint := range exchangeRejectHints {
		if strings.Contains(body, `"`+code+`"`) {
			return hint
		}
	}
	return ""
}

// refreshLocked 是 RefreshToken 的持锁内部实现；调用方必须已持有 a 写锁。
// 任何失败路径都不改写 a 字段，保证旧 refreshToken 可重试。
func (c *Client) refreshLocked(a *auth.Auth) error {
	if strings.TrimSpace(a.RefreshToken) == "" {
		return fmt.Errorf("no refreshToken")
	}
	host := a.ApiHost
	if host == "" {
		host = c.oauthBase()
	}
	body := map[string]any{
		"ClientID":     c.ClientID,
		"RefreshToken": a.RefreshToken, // 已持 a 写锁，直接读
		"ClientSecret": "-",
		"UserID":       "",
	}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, host+EpExchange, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	OAuthHeaders(req)
	data, err := c.doJSON(req)
	if err != nil {
		// 换票被拒：把 4xx 真因翻成人话。Classify 只给类别（400 → client），既不告诉运维
		// 「这号该重登」，也不该拿它去降权——refreshToken 用不了是凭证级问题，归 SessionDead。
		var ue *Error
		if errors.As(err, &ue) {
			if h := exchangeRejectHint(ue.Msg); h != "" {
				return &Error{Kind: ErrSessionDead, Status: ue.Status, Msg: h}
			}
		}
		return err
	}
	var resp struct {
		Result struct {
			Token         string `json:"Token"`
			TokenExpireAt any    `json:"TokenExpireAt"` // 见 msField：上游两种形状都在用
			// 下面两个按数字解析：只见过数字形态，不做额外兼容。
			TokenExpireDuration int64  `json:"TokenExpireDuration"`
			RefreshToken        string `json:"RefreshToken"`
			RefreshExpireAt     int64  `json:"RefreshExpireAt"`
		} `json:"Result"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return fmt.Errorf("exchange parse: %w", err)
	}
	if resp.Result.Token == "" {
		return fmt.Errorf("refresh_failed: no token in response — re-login required")
	}
	a.AccessToken = resp.Result.Token
	if resp.Result.RefreshToken != "" {
		a.RefreshToken = resp.Result.RefreshToken
	}
	// 过期时间：优先 TokenExpireAt（上游返回毫秒，需归一化为 Unix 秒）
	if ms := msField(resp.Result.TokenExpireAt); ms > 0 {
		a.ExpiresAt = normalizeExpiresAt(ms)
	} else if resp.Result.TokenExpireDuration > 0 {
		a.ExpiresAt = time.Now().Add(time.Duration(resp.Result.TokenExpireDuration) * time.Second).Unix()
	}
	return nil
}

// msField 取毫秒时间戳字段：数字与字符串都在用（agent2api 实测中国版回的是字符串）。
// 按 int64 解析会把整条换票响应判成解析失败 → 票不落盘 → 连续 401 后被判定失效，
// 一个形状变化就能让全池账号掉线，所以这一处宽容。
func msField(v any) int64 {
	switch x := v.(type) {
	case float64:
		return int64(x)
	case string:
		n, _ := strconv.ParseInt(strings.TrimSpace(x), 10, 64)
		return n
	}
	return 0
}

// normalizeExpiresAt 把 ExchangeToken 的 TokenExpireAt 归一化为 Unix 秒。
// 上游返回毫秒（如 1786847930141），auth 文件用秒（1786847930）。
// 毫秒时间戳 ~1.7e12，秒时间戳 ~1.7e9，用 1e12 区分。
func normalizeExpiresAt(v int64) int64 {
	if v > 1e12 {
		return v / 1000
	}
	return v
}

// ChatStream 发 llm_utils_chat 请求并返回原始 SSE body 流（调用方负责 Close）。
// 非 2xx 时 rc 为 nil、body 为上游响应体（供调用方 Classify）、err 为 nil；
// 只有传输层失败才返回 err。
func (c *Client) ChatStream(a *auth.Auth, body []byte) (rc io.ReadCloser, status int, respBody []byte, err error) {
	req, err := http.NewRequest(http.MethodPost, c.agentBaseFor(a)+EpChat, bytes.NewReader(PrepareBody(body)))
	if err != nil {
		return nil, 0, nil, err
	}
	SOLOHeaders(req, a, true)
	// 用专用流客户端（无总超时），避免长 SSE 流被 HTTP.Timeout 截断。
	// 读锁跨过 Do：SetTimeouts（面板热改）写的就是这两个 client 里的超时字段。
	c.timeoutMu.RLock()
	hc := c.HTTP
	if c.StreamHTTP != nil {
		hc = c.StreamHTTP
	}
	idle := c.idleTimeout
	resp, err := hc.Do(req)
	c.timeoutMu.RUnlock()
	if err != nil {
		log.Printf("chat_stream uid=%s: transport error: %v", a.UID, err)
		return nil, 0, nil, err
	}
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		kind := Classify(resp.StatusCode, string(raw))
		log.Printf("chat_stream uid=%s: upstream %d %s body=%s",
			a.UID, resp.StatusCode, kind, truncate(string(raw), 200))
		return nil, resp.StatusCode, raw, nil
	}
	if idle > 0 {
		return &idleTimeoutReader{rc: resp.Body, idle: idle}, resp.StatusCode, nil, nil
	}
	return resp.Body, resp.StatusCode, nil, nil
}

// ModelInfo 动态模型信息。
type ModelInfo struct {
	ID            string
	Name          string
	ContextWindow int64 // = maxInputTokens
	MaxTokens     int64 // = maxOutputTokens

	// 思考相关三个字段按上游原样搬，不解释、不换算（面板「能力 / 思考」列如实显示）：
	//   Capability = display_config.model_capability（实测 15 个官方模型全是 reasoning_model）
	//   Thinking   = model_detail_list[0].model_extra_config 里 Thinking.Type（enabled / 空）
	//   Effort     = reasoning_effort_config 原文（如 {"support_thinking":false}；上游没给则空）
	Capability string
	Thinking   string
	Effort     string

	// Function 该模型属于哪条通道（solo_work_lite / solo_coder）：出站 function 跟着模型走。
	//
	// ⚠️ 2026-10-04 配对实测：function **不构成可调用性约束**——4 个模型（glm-5.2 /
	// glm-5.3-flash / kimi-k2.8-preview / DeepSeek-V4-Flash）× 4 根轴
	// （solo_work_lite / solo_agent / chat_v3 / solo_coder）16/16 全 200 且都有正文 + usage。
	// 此前「coder 面的模型会被 Work 通道判 4001」与 240xu 待验证的「solo_agent 换轴」都
	// 复现不出来，所以按模型分通道只是**组织方式**，不是必需；别再为它改路由。
	Function string

	// 以下来自客户端模型选择器视图（batch_get_detail_param，见 fetchBatchCatalog）：
	//   ContextMax 客户端**声明**口径（ContextWindow 是 __dev 实际请求口径）；批量视图没有则 0
	//   Rate       基准积分倍率（上游 consumption_rate.data.rate）
	//   Image      多模态（display_config.multimodal）
	//   Legacy/Why  客户端已隐藏（上代/内部）——**仍然可调**，只是不在客户端选择器里
	ContextMax    int64
	Rate          *float64
	SupportsImage *bool
	Legacy        bool
	LegacyWhy     string

	// PaidOnly 上游 identity_list 里没有 Free(0) = 订阅专享：Free 号调它必 1005。
	// 国内版全表都有 0，只有国际版会置位；取不到 identity_list 时为 false（按可用处理）。
	PaidOnly bool

	// PickOnly 该模型只在**客户端批量视图**里可见，账号的聊天配置表里没有它。
	// 出站 body 必须去掉 config_name：实测传 config_name=<自己> 上游回 4001
	// 「param is invalid」，不传则正常出正文（2026-10-04，glm-5.3-flash / qwen3.8-flash /
	// kimi-k2.8-preview / glm-5.3-flashx 四个全是这样；chat 配置表里有的模型传不传都行，
	// 所以只对这批复单放行，不动现有流量的报文形状）。
	PickOnly bool

	// DevModelName / MaxModelName 上游表里这个模型的两种**变体名**
	// （model_detail_list[].model_name 原样，形如 "glm-5.2__dev" / "glm-5.2__max"）。
	//
	// 出站 model 填的是变体名，config_name 仍填裸的 ID：2026-10-15 实测
	// body{config_name:"glm-5.2", model:"glm-5.2__dev"} 与 {..."__max"} 在
	// solo_work_lite / chat_v3 / solo_agent 三个面上都 200，正文与 usage 齐全。
	//
	// ⚠️ 变体名**不能由 ID 拼**——全表 417 条带后缀的变体名里 99 条不满足
	// ID+后缀（search_agent_qwen_fast→search_agent_model__max、
	// glm-5.2_advisor_doubao→glm-5.2__dev、custom_model_kimi→custom_model_placeholder__dev）。
	// 所以这里只存**读到的原值**，取不到就是空串，绝不猜。见 variantNames。
	DevModelName string
	MaxModelName string

	// Realm 该条目来自哪个地区（cn / intl）。由调用方（server.fetchDynamicModels）填，
	// 不在这里推断：同一个模型名在**两个地区都有**时（deepseek-v4.1-flash /
	// minimax-m2.7 / kimi-k2.5 …），裸名已经不足以标识一条条目。
	Realm string
}

// MaxAvailable 这个模型有没有「更大的 Max 窗口」。
//
// 两个条件都要：① 上游表里真有 __max 变体名；② 客户端声明的 max 口径大于 dev 口径。
// 只有 ① 的（solo_work_lite 面上 12 个条目有 __max 名但 ctx_max 与 dev 相同）
// 开了也不增长，开了等于没开，如实报 false。
func (mi ModelInfo) MaxAvailable() bool {
	return mi.MaxModelName != "" && mi.ContextMax > mi.ContextWindow
}

// paramConfig 是 get_detail_param 响应里的一条模型配置。
type paramConfig struct {
	ConfigName        string `json:"config_name"`
	IsInvisibleToUser bool   `json:"is_invisible_to_user"`
	Usage             string `json:"usage"`
	DisplayConfig     struct {
		DisplayName     string `json:"display_name"`
		ModelCapability string `json:"model_capability"`
		Multimodal      *bool  `json:"multimodal"`
	} `json:"display_config"`
	ContextWindowTokens struct {
		Dev int64 `json:"dev"`
		// Max 客户端**声明**口径（dev 是 __dev 实际请求口径）；批量视图才有值。
		Max int64 `json:"max"`
	} `json:"context_window_tokens"`
	// ConfigSwitch 客户端是否开启该条目；批量视图才有（false = 客户端已关闭）。
	ConfigSwitch *bool `json:"config_switch"`
	// CustomModels 非空 = 服务端自定义预设的回显（客户端主选择器不列它）。
	CustomModels []any `json:"custom_models"`
	// DisplayContactConfig 是 JSON 字符串（要二次解码），里面有每模型积分倍率。
	DisplayContactConfig string `json:"display_contact_config"`
	ModelDetailList      []struct {
		MaxTokens int64 `json:"max_tokens"`
		// ModelName 形如 "glm-5.2__dev"：带 __dev 后缀的条目 = 可调用实证（批量视图）。
		ModelName string `json:"model_name"`
		// ModelExtraConfig 是 JSON 字符串（要二次解码），里面的 Thinking.Type 才是思考开关。
		ModelExtraConfig string `json:"model_extra_config"`
	} `json:"model_detail_list"`
	// ReasoningEffortConfig 上游原样给的思考档位配置，面板不做解释。
	ReasoningEffortConfig json.RawMessage `json:"reasoning_effort_config"`
}

// thinkingType 从 model_extra_config 这段 JSON 字符串里取 Thinking.Type（如 "enabled"）。
// 上游把这层嵌成字符串，解析失败就当作"上游没给"，不猜。
func thinkingType(extraConfig string) string {
	if extraConfig == "" {
		return ""
	}
	var m struct {
		Thinking struct {
			Type string `json:"Type"`
		} `json:"Thinking"`
	}
	if json.Unmarshal([]byte(extraConfig), &m) != nil {
		return ""
	}
	return m.Thinking.Type
}

// usageChat 用户可对话模型。上游还有 custom_model（自定义槽位）与 summary（内部摘要），
// 以及 is_invisible_to_user 的内部子代理；这些都不是用户能选的官方模型。
const usageChat = "chat_completion"

// pickOfficialModels 从全量配置表里挑出用户可选的官方模型。
// 实测上游返回 39 条（版本码 20260716）/ 42 条（20260811）内部配置，
// 排除 is_invisible_to_user（子代理等）+ custom_model_* 槽位 + summary 之后，
// 就是客户端模型选择器里能选的那些。
// ponytail: 国际版把 5 个官方模型全标成 is_invisible_to_user=true 却照样能调
// （实测 gpt-5.2 / kimi-k3 / gpt-5.4 都出正文）。国际版因此不按可见性一刀切，改按
// 「有没有显示名」判：官方模型都有 display_name（GPT-5.2/Kimi-K3…），内部子代理
// （browser_use_subagent）没有，正好分得开。国内版口径不变。
func pickOfficialModels(list []paramConfig, a *auth.Auth, function string) []ModelInfo {
	intl := a != nil && a.Realm() == auth.RealmIntl
	out := make([]ModelInfo, 0, len(list))
	for _, cfg := range list {
		if cfg.ConfigName == "" || cfg.Usage != usageChat {
			continue
		}
		if cfg.IsInvisibleToUser {
			if !intl || cfg.DisplayConfig.DisplayName == "" {
				continue
			}
		}
		// coder 通道混着内部子代理（search_agent* 一族 12 个），用户侧没有它们；
		// Work 通道靠 is_invisible_to_user 判就够（实测只有 browser_use_subagent 等 2 个）。
		if function == FunctionCoder && strings.HasPrefix(cfg.ConfigName, "search_agent") {
			continue
		}
		devName, maxName := variantNames(cfg)
		mi := ModelInfo{
			ID:            cfg.ConfigName,
			Name:          cfg.DisplayConfig.DisplayName,
			ContextWindow: cfg.ContextWindowTokens.Dev,
			// 单视图自己就声明了 max：批量端点坏掉时会退到这条路径（FetchModels 里批量
			// 失败只回退不报错），不带过来就等于「上游声明了 max、我们这边 MaxAvailable 恒 false」。
			ContextMax: cfg.ContextWindowTokens.Max,
			Function:   function,
			// 单 function 视图也给变体名：只在老视图里出现的模型也要能开 Max。
			// 不过这条路取的变体名可能不准（老视图的 model_name 与 ID 对不上时取空），
			// 取空就是「没有 Max」——宁可少一个开关，不给一个发出去没用的名字。
			DevModelName: devName,
			MaxModelName: maxName,
		}
		if len(cfg.ModelDetailList) > 0 {
			mi.MaxTokens = cfg.ModelDetailList[0].MaxTokens
			mi.Thinking = thinkingType(cfg.ModelDetailList[0].ModelExtraConfig)
		}
		mi.Capability = cfg.DisplayConfig.ModelCapability
		if free, known := contactFree(cfg.DisplayContactConfig); known {
			mi.PaidOnly = !free
		}
		if len(cfg.ReasoningEffortConfig) > 0 && string(cfg.ReasoningEffortConfig) != "null" {
			mi.Effort = string(cfg.ReasoningEffortConfig)
		}
		out = append(out, mi)
	}
	return out
}

// ── 客户端模型选择器视图（batch_get_detail_param）────────────────────────────
//
// 同一 host 上单 function 的 EpModels 与批量的 EpModelsBatch 是**两个视图**：可见性标志、
// 上下文口径、模型集合都不同（2026-10-04 实测，本仓 CN 账号）。批量视图是客户端选择器用的，
// 也是权威的那一份：
//   - 单 function 视图缺 glm-5.3-flash / glm-5.3-flashx / kimi-k2.8-preview / qwen3.8-flash；
//   - 单 function 视图把 12 个客户端已隐藏的上代模型当可见（glm-5/5.1、kimi-k2.5/2.6/2.7-code、
//     minimax-m2.7、qwen-3.5/3.6-plus、Doubao-Seed-2.0-Code、DeepSeek-V4-Flash/Pro 非正式版）。
// 参照实现：smart-open/TraeWorkAssistant（Rust/MIT）的 models_sync.rs，其请求形态与过滤规则
// 逐条对照过；本仓按自己的结构重写。

// batchRequestFunctions 客户端选择器的真实请求形态（7 个视图，顺序照抓包固化值）。
var batchRequestFunctions = []string{
	"builder", "builder_v3", "chat_v3", "code_reviewer", "code_review_summary", "refactor", "solo_agent",
}

// batchPriority 同一模型在多组出现时的权威次序：solo_agent / chat_v3 是主视图，
// builder* 是降级副本，code_reviewer / refactor 是评审替身（展示名都不一样）。
var batchPriority = []string{"solo_agent", "chat_v3", "builder", "builder_v3"}

// catalogVisibleMaxMin 客户端的代际阈值：声明上下文 max 低于它的模型不出现在选择器里
// （上代：dev 116k/200k 且 max 缺失）。
const catalogVisibleMaxMin = 250_000

// catalogInternalExact / catalogInternalPrefix 官方内部配置（子代理、摘要、槽位）。
var catalogInternalExact = []string{"Doubao_1_6", "doubao_1_6", "aquila", "sagitta"}

var catalogInternalPrefix = []string{
	"custom_model", "search_agent", "fast_apply", "input_optimization",
	"explore", "file_search", "browser_use", "agnes", "summary", "commit",
}

func isInternalConfig(name string) bool {
	for _, e := range catalogInternalExact {
		if name == e {
			return true
		}
	}
	for _, p := range catalogInternalPrefix {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// catalogMeta 批量视图给一个模型的权威元数据。
type catalogMeta struct {
	ID          string // 官方 config_name 原样（大小写以它为准）
	Label       string // 权威视图的展示名
	Visible     bool   // 客户端选择器会展示它
	Why         string // Visible=false 的判据（面板 tooltip 用）
	Dev, Max    int64
	Rate        float64
	HasRate     bool
	Image       bool
	HasImage    bool
	HasDevEntry bool // model_detail_list 里有 __dev 条目 = 可调用实证
	// DevName / MaxName 上游表里两种变体名（model_detail_list[].model_name 原样）。
	// 见 ModelInfo.DevModelName 的实测说明：**不能由 ID 拼**。
	DevName, MaxName string

	// PaidOnly 上游 identity_list 里没有 Free（0）= 订阅专享，Free 号调它必 1005。
	// 国内版全表都有 0，所以只有国际版会置位。上游没给 identity_list 时为 false：
	// 「不知道」按可用处理，别把可用模型误标成付费。
	PaidOnly bool
}

// variantNames 从 model_detail_list 里取两种变体名（表里没有则为空）。
//
// ⚠️ 只能这么取。2026-10-15 全表实测：417 条带后缀的变体名里有 **99 条 ≠ config_name+后缀**：
//
//	config_name=search_agent_qwen_fast → model_name=search_agent_model__max（多个 config 共用一个变体名）
//	config_name=glm-5.2_advisor_doubao  → model_name=glm-5.2__dev（advisor 槽位复用基础模型的变体名）
//	config_name=custom_model_kimi       → model_name=custom_model_placeholder__dev
//
// 按 config_name 拼出来的名字上游认不出，会静默拿到 dev 行为。
func variantNames(ci paramConfig) (dev, mx string) {
	for _, d := range ci.ModelDetailList {
		name := strings.TrimSpace(d.ModelName)
		switch {
		case dev == "" && strings.HasSuffix(name, "__dev"):
			dev = name
		case mx == "" && strings.HasSuffix(name, "__max"):
			mx = name
		}
	}
	return dev, mx
}

// contactRate 从 display_contact_config 这段 JSON 字符串里取基准积分倍率
// （consumption_rate.data.rate；会员/闲时折扣在 discount 块，客户端展示的是基准值）。
func contactRate(raw string) (float64, bool) {
	if strings.TrimSpace(raw) == "" {
		return 0, false
	}
	var m struct {
		ConsumptionRate struct {
			Data struct {
				Rate float64 `json:"rate"`
			} `json:"data"`
		} `json:"consumption_rate"`
	}
	if json.Unmarshal([]byte(raw), &m) != nil {
		return 0, false
	}
	return m.ConsumptionRate.Data.Rate, m.ConsumptionRate.Data.Rate > 0
}

// contactFree 从 display_contact_config 里读 access.data.identity_list，
// 判断这个模型对 Free 身份（0）开不开放。
//
// 2026-10-21 实测：国内版 47 条全是 [0 5 1 2 3 100]，人人可用，没有订阅墙；
// 国际版 gpt-6.1-sol / gpt-6-sol / gpt-6-luna 是 [4 1 2 3]（缺 0）= 订阅专享，
// 其余是 [0 5 4 1 2 3]。同一张 Free 号实测：后者 4/4 全 200，前者 1005「plan 权益不足」。
// 上游没给 identity_list 时返回 known=false，不猜。
func contactFree(raw string) (free, known bool) {
	if strings.TrimSpace(raw) == "" {
		return false, false
	}
	var m struct {
		Access struct {
			Data struct {
				IdentityList []int `json:"identity_list"`
			} `json:"data"`
		} `json:"access"`
	}
	if json.Unmarshal([]byte(raw), &m) != nil || len(m.Access.Data.IdentityList) == 0 {
		return false, false
	}
	for _, id := range m.Access.Data.IdentityList {
		if id == 0 {
			return true, true
		}
	}
	return false, true
}

// meta 按客户端选择器的四条尺子判定可见性，并取出倍率/双口径/图片支持。
// 尺子（缺一条就会多出一批上代或内部条目）：
//  1. 内部配置黑名单 / 自定义槽位回显；
//  2. is_invisible_to_user 或 config_switch=false；
//  3. 声明上下文 max ≥ 250k（代际）；
//  4. 有展示名。
func (ci paramConfig) meta() catalogMeta {
	m := catalogMeta{ID: ci.ConfigName, Visible: true}
	switch {
	case isInternalConfig(ci.ConfigName):
		m.Visible, m.Why = false, "内部配置"
	case strings.HasPrefix(ci.ConfigName, "custom_") || ci.Usage == "custom_model" || len(ci.CustomModels) > 0:
		m.Visible, m.Why = false, "自定义模型回显"
	case ci.IsInvisibleToUser:
		m.Visible, m.Why = false, "客户端标记不可见"
	case ci.ConfigSwitch != nil && !*ci.ConfigSwitch:
		m.Visible, m.Why = false, "客户端已关闭"
	}
	m.Dev, m.Max = ci.ContextWindowTokens.Dev, ci.ContextWindowTokens.Max
	if m.Visible && (m.Max <= 0 || m.Max < catalogVisibleMaxMin) {
		m.Visible, m.Why = false, "上代（客户端代际阈值 max<250k）"
	}
	m.Label = strings.TrimSpace(ci.DisplayConfig.DisplayName)
	if m.Label == "" {
		if m.Visible {
			m.Visible, m.Why = false, "无展示名"
		}
	}
	if r, ok := contactRate(ci.DisplayContactConfig); ok {
		m.Rate, m.HasRate = r, true
	}
	if free, known := contactFree(ci.DisplayContactConfig); known {
		m.PaidOnly = !free
	}
	if ci.DisplayConfig.Multimodal != nil {
		m.Image, m.HasImage = *ci.DisplayConfig.Multimodal, true
	}
	m.DevName, m.MaxName = variantNames(ci)
	m.HasDevEntry = m.DevName != ""
	return m
}

// fetchBatchCatalog 拉客户端选择器视图，返回 config_name(小写) → 权威元数据。
func (c *Client) fetchBatchCatalog(a *auth.Auth) (map[string]catalogMeta, error) {
	body, _ := json.Marshal(map[string]any{
		"functions":                 batchRequestFunctions,
		"agent_type":                "solo_agent",
		"current_config_info":       map[string]any{"config_name": "", "is_custom_model": false},
		"mode_type":                 0,
		"access_type":               0,
		"ab_force_vids":             "",
		"ab_autotest_advanced_mode": 0,
		"show_custom_model":         true,
	})
	req, err := http.NewRequest(http.MethodPost, c.agentBaseFor(a)+EpModelsBatch, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	SOLOHeaders(req, a, false)
	data, err := c.doJSON(req)
	if err != nil {
		return nil, err
	}
	var resp struct {
		FunctionConfigs []struct {
			Function       string        `json:"function"`
			ConfigInfoList []paramConfig `json:"config_info_list"`
		} `json:"function_configs"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("batch models parse: %w", err)
	}
	if len(resp.FunctionConfigs) == 0 {
		return nil, errors.New("batch models: function_configs empty")
	}
	rank := func(fn string) int {
		for i, f := range batchPriority {
			if f == fn {
				return i
			}
		}
		return len(batchPriority)
	}
	sort.SliceStable(resp.FunctionConfigs, func(i, j int) bool {
		return rank(resp.FunctionConfigs[i].Function) < rank(resp.FunctionConfigs[j].Function)
	})
	out := map[string]catalogMeta{}
	for _, g := range resp.FunctionConfigs {
		for _, ci := range g.ConfigInfoList {
			id := strings.TrimSpace(ci.ConfigName)
			if id == "" {
				continue
			}
			m := ci.meta()
			prev, seen := out[strings.ToLower(id)]
			// 先见者优先（已按权威视图排好序）；只有「后来者带 __dev 实证、先前的没有」才覆盖。
			if seen && (prev.HasDevEntry || !m.HasDevEntry) {
				continue
			}
			out[strings.ToLower(id)] = m
		}
	}
	return out, nil
}

// mergeCatalog 用批量视图校正/补全单 function 视图的结果。
// 不删模型：客户端已隐藏的上代条目仍然可调（mapModel 会把不在表里的模型判 400），
// 只标记成 Legacy 让面板如实显示，是否下线由人决定。
func mergeCatalog(list []ModelInfo, cat map[string]catalogMeta, a *auth.Auth) []ModelInfo {
	seen := make(map[string]bool, len(list))
	for i := range list {
		key := strings.ToLower(list[i].ID)
		seen[key] = true
		m, ok := cat[key]
		if !ok {
			// 批量视图里没有它 = 客户端选择器不展示：老视图独有（solo_coder_search_agent 这类
			// 内部子代理、gemini-3-pro-solo、Doubao-Seed-2.0-Code 等都落这里）。
			// 只标记不删——删了 mapModel 会把它们判 400。实测这类条目的展示名还不可信
			// （Dola-Seed-2.0-Code 顶着 "Seed-2.1-Turbo"、gemini-3-pro-solo 顶着 "GPT-5-medium"），
			// 标记出来比猜一个名字诚实。
			list[i].Legacy, list[i].LegacyWhy = true, "老视图独有（客户端选择器无此条目）"
			continue
		}
		if m.Dev > 0 {
			list[i].ContextWindow = m.Dev
		}
		// Max 口径与变体名同样「批量视图有值才覆盖」：上游某些面（solo_work_lite / solo_coder）
		// 的条目压根不带 context_window_tokens.max，也没有 __max 名。无条件写入会把单 function
		// 视图刚取到的 1M 与 __max 名一起清成 0/空，MaxAvailable() 从 true 翻成 false。
		// 失败表象是「某个模型的 Max 开关凭空消失」，而且是按模型逐个丢，不是整表丢，
		// 极难归因——宁可留着单视图的值（那也是上游给的），也不拿一份没这份数据的视图去清它。
		if m.Max > 0 {
			list[i].ContextMax = m.Max
		}
		// 变体名以批量视图（客户端选择器）为准：它才是权威那份；但它没给名字时不清空。
		if m.DevName != "" {
			list[i].DevModelName = m.DevName
		}
		if m.MaxName != "" {
			list[i].MaxModelName = m.MaxName
		}
		if m.HasRate {
			r := m.Rate
			list[i].Rate = &r
		}
		// 只置位不清零：两个视图的 identity_list 同源，批量视图没给时不该把
		// 单视图已经判定的「订阅专享」抹掉。
		if m.PaidOnly {
			list[i].PaidOnly = true
		}
		if m.HasImage {
			v := m.Image
			list[i].SupportsImage = &v
		}
		if !m.Visible {
			list[i].Legacy, list[i].LegacyWhy = true, m.Why
		}
		if m.Label != "" {
			list[i].Name = m.Label
		}
	}
	// 批量视图可见、单 function 视图没给的新模型（客户端新上架）→ 补进来。
	// Function 用默认通道：上游对这几根 function 轴是宽松的（实测 glm-5.2/kimi-k2.8-preview
	// 在 solo_work_lite / solo_agent / chat_v3 / solo_coder 下都 200），不必为它改聊天路径。
	added := make([]ModelInfo, 0)
	for key, m := range cat {
		if seen[key] || !m.Visible {
			continue
		}
		mi := ModelInfo{ID: m.ID, Name: m.Label, ContextWindow: m.Dev, ContextMax: m.Max, Function: Function, PickOnly: true,
			DevModelName: m.DevName, MaxModelName: m.MaxName, PaidOnly: m.PaidOnly}
		if m.HasRate {
			r := m.Rate
			mi.Rate = &r
		}
		if m.HasImage {
			v := m.Image
			mi.SupportsImage = &v
		}
		added = append(added, mi)
	}
	sort.Slice(added, func(i, j int) bool { return added[i].ID < added[j].ID })
	return append(list, added...)
}

// FetchModels 拉账号当前可用的官方模型表（get_detail_param）。
// 返回的是过滤后的官方模型，不是上游的内部全量表。
func (c *Client) FetchModels(a *auth.Auth) ([]ModelInfo, error) {
	body := map[string]any{
		"function":            Function,
		"config_names":        nil,
		"need_prompt":         false,
		"current_config_info": nil,
		"poly_prompt":         true,
		"mode_type":           nil,
		"agent_type":          nil,
	}
	// 两条通道各拉一份并合并：Work（solo_work_lite）与 coder（solo_coder，网页端那批）。
	// 同一台主机、同一个端点，只有 body 里的 function 不同。
	var out []ModelInfo
	seen := map[string]bool{}
	var lastErr error
	for _, fn := range []string{Function, FunctionCoder} {
		body["function"] = fn
		raw, _ := json.Marshal(body)
		req, err := http.NewRequest(http.MethodPost, c.agentBaseFor(a)+EpModels, bytes.NewReader(raw))
		if err != nil {
			lastErr = err
			continue
		}
		SOLOHeaders(req, a, false)
		data, err := c.doJSON(req)
		if err != nil {
			lastErr = err
			continue
		}
		var resp struct {
			ConfigInfoList []paramConfig `json:"config_info_list"`
		}
		if err := json.Unmarshal(data, &resp); err != nil {
			lastErr = fmt.Errorf("models parse: %w", err)
			continue
		}
		for _, mi := range pickOfficialModels(resp.ConfigInfoList, a, fn) {
			if seen[mi.ID] {
				continue
			}
			seen[mi.ID] = true
			out = append(out, mi)
		}
	}
	if len(out) == 0 {
		if lastErr == nil {
			lastErr = fmt.Errorf("models api returned no official model")
		}
		return nil, lastErr
	}
	// 用客户端选择器视图校正：补上单 function 视图看不到的新模型、把客户端已隐藏的上代/内部
	// 条目标记出来、带上权威倍率与上下文双口径。拉不到就退回单视图结果（不让新端点成为单点）。
	if cat, err := c.fetchBatchCatalog(a); err == nil {
		out = mergeCatalog(out, cat, a)
	} else {
		// 静默回退过一次踩坑：批量端点 500 时 Max 与权威倍率一起消失，日志里一个字都没有，
		// 面板上只看得到「某模型没有 Max」。回退照旧，但必须留痕。
		log.Printf("[models] 批量视图拉取失败，退回单 function 视图（权威倍率/可见性/部分上下文口径拿不到）: %v", err)
	}
	return out, nil
}

// CheckinStatus 查询签到状态；credits 返回**本次实收**积分，只取上游 credits 一个字段。
//
// 实测不加 extra_credits（2026-10-16）：status 回 credits=100 且 extra_credits=100，而当天入账的
// 额度包 checkin_YYYYMMDD_<uid> 只有 100；连续 15 天 Σ包 与面板 credits 逐位吻合、多不出那 100。
// 也就是说 extra_credits 是展示值（连签加成），**没有进余额**——加上去就把「本次所得」虚报一倍。
func (c *Client) CheckinStatus(a *auth.Auth) (checkedIn bool, credits int64, enable bool, err error) {
	if a != nil && a.Realm() == auth.RealmIntl {
		return false, 0, false, ErrNoIntlUG
	}
	req, err := http.NewRequest(http.MethodPost, c.ugBase()+EpCheckinStatus, bytes.NewReader([]byte(ugBody)))
	if err != nil {
		return false, 0, false, err
	}
	UgHeaders(req, a)
	data, err := c.doJSON(req)
	if err != nil {
		return false, 0, false, err
	}
	var resp struct {
		CheckedIn bool  `json:"checked_in"`
		Credits   int64 `json:"credits"`
		// extra_credits 故意**不接**：它是连签加成的展示值，实测没有入账（见函数注释）。
		Enable  bool   `json:"enable"`
		Code    int    `json:"code"`
		Message string `json:"message"`
		Msg     string `json:"msg"`
		Success *bool  `json:"success"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return false, 0, false, fmt.Errorf("checkin status parse: %w", err)
	}
	// 上游的错误信封（典型是限流 9074）**不带** checked_in/enable：不校验就一律读成
	// enable=false，调度器会把限流当成「上游把这个号的签到关了」跳过（skipped，不冷却、
	// 不报警），日志还写着 upstream disabled——限流被伪装成正常状态。校验后才能如实报出来。
	if resp.Code != 0 {
		return false, 0, false, &BusinessError{Code: resp.Code, Message: "签到状态：" + checkinMsg(resp.Message, resp.Msg)}
	}
	if resp.Success != nil && !*resp.Success {
		return false, 0, false, &BusinessError{Code: resp.Code, Message: "签到状态：" + checkinMsg(resp.Message, resp.Msg)}
	}
	return resp.CheckedIn, resp.Credits, resp.Enable, nil
}

// CheckinClaim 执行签到。
func (c *Client) CheckinClaim(a *auth.Auth) error {
	if a != nil && a.Realm() == auth.RealmIntl {
		return ErrNoIntlUG
	}
	req, err := http.NewRequest(http.MethodPost, c.ugBase()+EpCheckinClaim, bytes.NewReader([]byte(ugBody)))
	if err != nil {
		return err
	}
	UgHeaders(req, a)
	data, err := c.doJSON(req)
	if err != nil {
		return err
	}
	var resp struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Msg     string `json:"msg"`
		Success *bool  `json:"success"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return fmt.Errorf("checkin claim parse: %w", err)
	}
	if resp.Code == 0 {
		if resp.Success != nil && !*resp.Success {
			return &BusinessError{Code: resp.Code, Message: "签到失败：" + checkinMsg(resp.Message, resp.Msg)}
		}
		return nil
	}
	// 9095「今日已签到」是幂等回执：与 status 的竞态（status 说没签、claim 说已签）会走到
	// 这里，报成失败会让面板把「已经签上了」显示成红字失败。
	if resp.Code == checkinAlreadyCode {
		return ErrCheckinAlready
	}
	return &BusinessError{Code: resp.Code, Message: "签到失败：" + checkinMsg(resp.Message, resp.Msg)}
}

// ugBody 签到类接口的请求体。带 req_source:1 —— chevy222/app-cf-checkin 按真机抓包记的
// 客户端口径是 status/claim/usage 三个接口都带这个字段（我们的 {} 目前也 200，属对齐而非修 bug）。
const ugBody = `{"req_source":1}`

// grantPending 上游「现在领不到」的业务码：不是失败，下一轮自然会再试。
// 9069/9090 活动暂不可用（两条链路各一个）/ 9070 暂无领取权限。
func grantPending(code int) bool { return code == 9069 || code == 9090 || code == 9070 }

// workExtra 领取结论只在这一个对象里（activity/action 的响应）。
// 实测 2026-10-19：granted=true + grantable=false = 这个号已经领过了；
// prize_text 是上游给的中文结果（券名 / 「明天再来」），不是我们编的。
type workExtra struct {
	Grantable bool   `json:"grantable"`
	Granted   bool   `json:"granted"`
	PrizeText string `json:"prize_text"`
}

// ugPost 打一条 ug 接口并解出业务码与 work_extra。REST 层永远回 200，结论只能看 code。
func (c *Client) ugPost(a *auth.Auth, path, body string) (code int, msg string, we workExtra, err error) {
	req, err := http.NewRequest(http.MethodPost, c.ugBase()+path, strings.NewReader(body))
	if err != nil {
		return 0, "", we, err
	}
	UgHeaders(req, a)
	data, err := c.doJSON(req)
	if err != nil {
		return 0, "", we, err
	}
	var r struct {
		Code      int       `json:"code"`
		Message   string    `json:"message"`
		Msg       string    `json:"msg"`
		WorkExtra workExtra `json:"work_extra"`
	}
	if err := json.Unmarshal(data, &r); err != nil {
		return 0, "", we, fmt.Errorf("%s parse: %w", path, err)
	}
	return r.Code, checkinMsg(r.Message, r.Msg), r.WorkExtra, nil
}

// ClaimActivities 领取当前可领的活动/权益（借鉴 workbuddy-3in1 的 Trae 链路）。
//
// 三条链路（2026-10-19 对 5 个真号实测，两个 host 交叉验证过，只有 api.trae.cn 上有）：
//   - activity_type==103 的裂变奖励必须带 activity_id 单独领（传 activity_type 一律 4001）；
//   - 其余活动走 activity/action：它是唯一会回 work_extra（granted/grantable/prize_text）的
//     接口，实测 checkin_1024 这类券活动能领、也能读出「已领」；
//   - grant_work_benefit 是兜底的总入口（服务端按账号可领清单自行发放），覆盖 action
//     不认的那几类（新人/发消息/学生认证/桌面下载）。
//
// 返回的 notes 只写「哪条、什么结论」，不写积分数字——响应体里没有可信的到账额度。
// 领不到的（grantPending）不进 notes，只留日志，避免每天刷同样的噪音。
func (c *Client) ClaimActivities(a *auth.Auth) ([]string, error) {
	if a != nil && a.Realm() == auth.RealmIntl {
		return nil, ErrNoIntlUG // 国际域没有 ug 接口（同签到）
	}
	var notes []string
	var errs []error
	acts, aerr := c.activityList(a)
	if aerr != nil {
		errs = append(errs, aerr)
	}
	now := time.Now().UnixMilli()
	for _, x := range acts {
		if !x.Enabled || (x.Start != 0 && now < x.Start) || (x.End != 0 && now > x.End) {
			continue
		}
		if x.Type == 103 {
			gcode, gmsg, _, gerr := c.ugPost(a, EpFissionGrant, `{"activity_id":"`+x.ID+`","req_source":1}`)
			switch {
			case gerr != nil:
				errs = append(errs, fmt.Errorf("%s: %w", x.ID, gerr))
			case gcode == 0:
				notes = append(notes, x.ID+" 已领")
			case grantPending(gcode):
				log.Printf("[grant] %s %s: code=%d %s（现在领不到）", a.UID, x.ID, gcode, gmsg)
			default:
				notes = append(notes, fmt.Sprintf("%s code=%d %s", x.ID, gcode, gmsg))
			}
			continue
		}
		acode, amsg, we, aerr := c.ugPost(a, EpActivityAction, `{"activity_id":"`+x.ID+`","req_source":1}`)
		switch {
		case aerr != nil:
			errs = append(errs, fmt.Errorf("%s: %w", x.ID, aerr))
		case acode == 0 && we.Granted:
			notes = append(notes, x.ID+" 已领"+prizeSuffix(we.PrizeText))
		case acode == 0:
			// 受理但没有 work_extra（student_auth_reward 实测如此）：没有可报的结论。
		case grantPending(acode):
			log.Printf("[grant] %s %s: code=%d %s（现在领不到）", a.UID, x.ID, acode, amsg)
		default:
			notes = append(notes, fmt.Sprintf("%s code=%d %s", x.ID, acode, amsg))
		}
	}
	// 兜底总入口：action 不认的那几类活动靠它。
	bcode, bmsg, _, berr := c.ugPost(a, EpGrantBenefit, `{"trae_client":"Lite","device_id":"`+UGDeviceID(a)+`"}`)
	switch {
	case berr != nil:
		errs = append(errs, fmt.Errorf("权益领取: %w", berr))
	case bcode == 0:
		notes = append(notes, "权益已领")
	case grantPending(bcode):
		log.Printf("[grant] %s 权益: code=%d %s（现在领不到）", a.UID, bcode, bmsg)
	default:
		notes = append(notes, fmt.Sprintf("权益 code=%d %s", bcode, bmsg))
	}
	if len(errs) > 0 {
		return notes, errors.Join(errs...)
	}
	return notes, nil
}

func prizeSuffix(p string) string {
	if p == "" {
		return ""
	}
	return "（" + p + "）"
}

// activityRow 活动清单里的一条（只有它能给出 activity_id + 类型 + 有效期）。
type activityRow struct {
	ID      string `json:"activity_id"`
	Type    int    `json:"activity_type"`
	Enabled bool   `json:"Enabled"`
	Start   int64  `json:"start_time_ms"`
	End     int64  `json:"end_time_ms"`
}

// activityList 拉活动清单。
func (c *Client) activityList(a *auth.Auth) ([]activityRow, error) {
	req, err := http.NewRequest(http.MethodPost, c.ugBase()+EpActivityInfo, strings.NewReader(ugBody))
	if err != nil {
		return nil, err
	}
	UgHeaders(req, a)
	data, err := c.doJSON(req)
	if err != nil {
		return nil, fmt.Errorf("活动清单: %w", err)
	}
	var r struct {
		Activities []activityRow `json:"commercial_activities"`
	}
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("活动清单 parse: %w", err)
	}
	return r.Activities, nil
}

// entUsageBody 积分接口请求体。用**客户端自己的形状**，不是空体：
// 客户端自带 SDK（`GetIdeUserEntUsageV2`）发的是 {require_usage, req_source, full_data, Request}，
// 调用点默认 {require_usage:true, req_source:1, full_data:true}（NextAgentX/trae-workbuddy-switch
// 从客户端缓存里翻出的原文 + cf-checkin 的真机抓包）。实测本机账号上 `{}` 与这个体返回的 pack
// 列表完全一致，所以这是去掉一个潜在失败模式（服务端若哪天把 full_data 默认成 false，`{}` 会拿不到
// 完整 pack 列表）而非修 bug —— 值不值得改就看这一点。
const entUsageBody = `{"require_usage":true,"req_source":1,"full_data":true}`

// entUsagePath 积分接口路径：国际版只提供 v1（v2 在 api-sg-central 上恒 404）。
func entUsagePath(a *auth.Auth) string {
	if a != nil && a.Realm() == auth.RealmIntl {
		return EpEntUsageIntl
	}
	return EpEntUsage
}

// ugBaseFor 计费/UG 网关：国际版用账号自己的 ApiHost（登录回调 host），
// 没有则回落 UgHostIntl。国内版恒为 UgHost。
func (c *Client) ugBaseFor(a *auth.Auth) string {
	if a != nil && a.Realm() == auth.RealmIntl {
		if a.ApiHost != "" {
			return a.ApiHost
		}
		return UgHostIntl
	}
	return c.ugBase()
}

// EpPayStatus 付费身份查询（国内 v1 路径；国际版同路径落在 UgHostIntl）。
const EpPayStatus = "/trae/api/v1/pay/ide_user_pay_status"

// PayIdentity 账号的付费身份：user_pay_identity 0 = Free，非 0 = 订阅档。
//
// 这是「订阅专享模型」拦不拦的唯一依据。额度接口（ide_user_ent_usage，每 5 分钟已经在打）
// 里**没有**这个字段（2026-10-21 实测两地区都是 is_credits_billing / is_dollar_usage_billing /
// is_pay_freshman / trial_status，无身份），所以只能单独问一次。
// 上游没给该字段时 known=false，调用方按「不知道」放行，不猜。
func (c *Client) PayIdentity(a *auth.Auth) (identity int, known bool, err error) {
	req, err := http.NewRequest(http.MethodPost, c.ugBaseFor(a)+EpPayStatus, bytes.NewReader([]byte("{}")))
	if err != nil {
		return 0, false, err
	}
	UgHeaders(req, a)
	data, err := c.doJSON(req)
	if err != nil {
		return 0, false, err
	}
	var m struct {
		Identity *int `json:"user_pay_identity"`
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return 0, false, err
	}
	if m.Identity == nil {
		return 0, false, nil
	}
	return *m.Identity, true, nil
}

// entUsagePack 一个套餐包的原始字段（国内 v2 与国际 v1 同形）。
//
// ⚠️ 两个地区**不是同一个口径**，字段名也不同（2026-10-15 实测，两国版本同时抓）：
//
//	国内 is_dollar_usage_billing=false / is_credits_billing=true
//	    额度 quota.credits_limit、已用 usage.credits_amount（basic_usage_limit 键**根本不存在**）
//	国际 is_dollar_usage_billing=true
//	    额度 quota.basic_usage_limit、已用 usage.basic_usage_amount（credits_limit 恒 0）
//
// 所以「国际版免费号显示 0 积分」不是接口坏了、也不是上游真没额度：是拿积分字段去读了一个
// 美元计费的账号。实测国际 Free plan：basic_usage_limit=1（$1）且 basic_usage_amount=0.00059。
// 判别只能用顶层 is_dollar_usage_billing，**不能**靠「basic_usage_limit 有没有值」——
// 国内版该键是缺失的，缺失与 0 在 JSON 里都是零值，分不开。
type entUsagePack struct {
	DisplayDesc         string `json:"display_desc"`
	GroupName           string `json:"group_name"`
	GroupType           int    `json:"group_type"`
	ExpireTime          int64  `json:"expire_time"`
	YearlyExpireTime    int64  `json:"yearly_expire_time"`
	SourceID            string `json:"source_id"`
	EntitlementBaseInfo struct {
		StartTime     int64  `json:"start_time"`
		EndTime       int64  `json:"end_time"`
		EntitlementID string `json:"entitlement_id"`
		Quota         struct {
			// CreditsLimit 用 float64：上游可能给 2000.0 这种形态，int64 会让**整包**反序列化
			// 失败并连带丢掉余额（实测 json.Unmarshal 报 cannot unmarshal number 2000.0）。
			CreditsLimit float64 `json:"credits_limit"`
			// BasicUsageLimit 国际版的美元额度（Free plan=1=「$1 免费额度」，Pro 月付=20）。
			BasicUsageLimit float64 `json:"basic_usage_limit"`
			// BonusUsageLimit 赠送额度，两地区都可能出现（实测国际 Free plan=0）。
			BonusUsageLimit float64 `json:"bonus_usage_limit"`
		} `json:"quota"`
	} `json:"entitlement_base_info"`
	Usage struct {
		CreditsAmount float64 `json:"credits_amount"`
		// BasicUsageAmount 国际版的美元已用量。实测 $0.00059 —— **必须 float**，
		// 走 int64 会截断成 0，面板就会显示成「一分没用」。
		BasicUsageAmount float64 `json:"basic_usage_amount"`
		BonusUsageAmount float64 `json:"bonus_usage_amount"`
	} `json:"usage"`
}

// 额度口径常量。
const (
	UnitCredit = "credit" // 国内：积分制
	UnitUSD    = "usd"    // 国际：按量计费（美元）
)

// EntUsage 一次额度查询的完整口径。
//
// 两种口径**不可换算**，所以不做任何折算：Unit 标明用的是哪一套，各自只填自己那几个字段。
// 面板按 Unit 决定显示「剩余/总额 积分」还是「$已用 / $额度」。
type EntUsage struct {
	Unit string // UnitCredit | UnitUSD
	// 积分口径（Unit=UnitCredit 时有效）
	Remain, Total int64
	// 美元口径（Unit=UnitUSD 时有效）
	UsedUSD, LimitUSD float64
	Expire            int64
	// ExpireCredits 在 Expire 那一天**真正会作废**的积分（只累加 expire==Expire 且
	// 还有余量的包）。Expire 本身是「最早到期日」，但账号余额往往分散在多个包里：
	// 拿总余额当「该日到期」会严重高估（实测某号 4334 分里只有 126 分当天到期，
	// 其余 4000 分 27 天后才到期），到期提醒会把人误导成「今天必须烧完 4334 分」。
	ExpireCredits int64
}

// ExpireAt 统一的到期时刻（两口径同一来源：packExpiry）。
func (u EntUsage) ExpireAt() int64 { return u.Expire }

// RemainUSD 剩余美元（不足 0 钳 0）。
func (u EntUsage) RemainUSD() float64 {
	r := u.LimitUSD - u.UsedUSD
	if r < 0 {
		return 0
	}
	return r
}

// packExpiry 包的到期时刻：expire_time → entitlement_base_info.end_time → yearly_expire_time。
// 国际版把 expire_time 恒填 0，真正的周期截止在 end_time（实测 Free plan：
// expire_time=0、end_time=1793491199）；只读 expire_time 会让国际号永远没有到期日。
func packExpiry(p entUsagePack) int64 {
	for _, v := range []int64{p.ExpireTime, p.EntitlementBaseInfo.EndTime, p.YearlyExpireTime} {
		if v > 0 {
			return v
		}
	}
	return 0
}

// entUsageResponse 额度接口的响应外壳（两地区同形，差异全在 entUsagePack 的字段口径里）。
type entUsageResponse struct {
	IsCreditsBilling  bool `json:"is_credits_billing"`
	IsDollarUsageBill bool `json:"is_dollar_usage_billing"`
	// UserEntitlementPackList 套餐列表（两地区同形，见 entUsagePack）。
	UserEntitlementPackList []entUsagePack `json:"user_entitlement_pack_list"`
	// UsageSummary 总额度口径（实测 {"total_amount":6000,"consumed_amount":434.19}）。
	// 与 Σpacks 逐位吻合，只在 packs 全都不带面额时当兜底（见 UserEntUsageDetail）。
	UsageSummary struct {
		TotalAmount    float64 `json:"total_amount"`
		ConsumedAmount float64 `json:"consumed_amount"`
	} `json:"usage_summary"`
}

// doJSONBalance 与 doJSON 同形，但走 balanceCli（独立 Transport，不被 SSE 长流堵）。
// 不持 timeoutMu：balanceCli 是独立的，热改超时走 SetTimeouts 同步写它（写瞬间的
// 读竞态可接受——balance 失败重试就好，不像 chat 流断开那么伤）。
func (c *Client) doJSONBalance(req *http.Request) (json.RawMessage, error) {
	cli := c.BalanceCli
	if cli == nil {
		cli = c.HTTP // 测试或老代码手造 Client 时的兜底
	}
	resp, err := cli.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, rerr := io.ReadAll(io.LimitReader(resp.Body, maxJSONBody+1))
	if rerr != nil {
		return nil, fmt.Errorf("读取上游响应中断（%d 字节已收）: %w", len(raw), rerr)
	}
	if len(raw) > maxJSONBody {
		return nil, fmt.Errorf("上游响应超过 %d 字节上限，放弃解析", maxJSONBody)
	}
	if resp.StatusCode >= 400 {
		kind := Classify(resp.StatusCode, string(raw))
		return nil, &Error{Kind: kind, Status: resp.StatusCode, Msg: truncate(string(raw), 200)}
	}
	return raw, nil
}

// FetchEntUsageFast 用 balanceCli 拉余额/积分包（面板 packages 视图、定时余额刷新用），
// 与 fetchEntUsage 的差别只在 HTTP 客户端：快、独立、不被 chat SSE 长流堵死。
func (c *Client) FetchEntUsageFast(a *auth.Auth) (entUsageResponse, error) {
	var resp entUsageResponse
	req, err := http.NewRequest(http.MethodPost, c.ugBaseFor(a)+entUsagePath(a), bytes.NewReader([]byte(entUsageBody)))
	if err != nil {
		return resp, err
	}
	UgHeaders(req, a)
	data, err := c.doJSONBalance(req)
	if err != nil {
		return resp, err
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return resp, fmt.Errorf("ent usage parse: %w", err)
	}
	if len(resp.UserEntitlementPackList) == 0 {
		return resp, errors.New("ent usage: 包列表为空（接口异常，不当作 0 分）")
	}
	return resp, nil
}

// BalancePackagesFast 与 BalancePackages 同义，走 FetchEntUsageFast。
func (c *Client) BalancePackagesFast(a *auth.Auth) ([]CreditPackage, EntUsage, error) {
	return c.balancePackagesFrom(a, c.FetchEntUsageFast)
}

// fetchEntUsage 打额度接口并解析（两地区共用，路径与网关见 entUsagePath / ugBaseFor）。
func (c *Client) fetchEntUsage(a *auth.Auth) (entUsageResponse, error) {
	var resp entUsageResponse
	req, err := http.NewRequest(http.MethodPost, c.ugBaseFor(a)+entUsagePath(a), bytes.NewReader([]byte(entUsageBody)))
	if err != nil {
		return resp, err
	}
	UgHeaders(req, a)
	data, err := c.doJSON(req)
	if err != nil {
		return resp, err
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return resp, fmt.Errorf("ent usage parse: %w", err)
	}
	if len(resp.UserEntitlementPackList) == 0 {
		// 空包列表 = 接口返回了异常结构（未登录/风控/结构变了），**不是**「剩余 0 分」。
		// 当成 0 分会把好号的积分静默清零（面板显示 0、选号权重一起塌），照
		// star620/TraeTools 的判据：宁可报解析失败，让调用方保留上一次的值。
		return resp, errors.New("ent usage: 包列表为空（接口异常，不当作 0 分）")
	}
	return resp, nil
}

// UserEntUsageDetail 返回**口径感知**的额度快照：国内给积分、国际给美元。
//
// 判别只用顶层 is_dollar_usage_billing（实测国内 false、国际 true）；不看字段有没有值，
// 因为国内版根本没有 basic_usage_limit 这个键，缺失与 0 分不开。
//
// 到期时间取两档（见 packExpiry 注释）：
//  1. 有额度的包里最早的一个未到期时刻——这是「额度到期」，也是选号「快过期优先」的依据；
//  2. 一个额度包都没有时（例如异常号），回落到所有包的周期截止 end_time。
func (c *Client) userEntUsageDetailFrom(a *auth.Auth, fetch func(*auth.Auth) (entUsageResponse, error)) (EntUsage, error) {
	resp, err := fetch(a)
	if err != nil {
		return EntUsage{}, err
	}
	u := EntUsage{Unit: UnitCredit}
	if resp.IsDollarUsageBill {
		u.Unit = UnitUSD
	}
	now := time.Now().Unix()
	var expireQuota, expireAny int64
	// dueCredits 是「expireQuota 那天到期的积分」；expireQuota 变早时同步重置，
	// 同一天到期则累加。两者必须一起维护，否则日期与金额会指向不同的包。
	var dueCredits int64
	dueAt := func(e int64, remain int64) {
		switch {
		case e <= 0:
		case expireQuota == 0 || e < expireQuota:
			expireQuota, dueCredits = e, remain
		case e == expireQuota:
			dueCredits += remain
		}
	}
	for _, p := range resp.UserEntitlementPackList {
		e := packExpiry(p)
		if e > now {
			expireAny = earlier(expireAny, e)
		}
		var limit float64
		var used float64
		if u.Unit == UnitUSD {
			// 赠送额度与基础额度是相加关系（实测 bonus_limit 可以大于 basic_limit），
			// 只读 basic 会把「Bonus only」这类包整条漏掉（limit=0 → continue）。
			limit = p.EntitlementBaseInfo.Quota.BasicUsageLimit + p.EntitlementBaseInfo.Quota.BonusUsageLimit
			used = p.Usage.BasicUsageAmount + p.Usage.BonusUsageAmount
			if limit <= 0 {
				continue
			}
			// 美元口径保留小数，不截断（实测 $0.00059 这种量级，取整会全变 0）。
			u.LimitUSD += limit
			u.UsedUSD += used
			// 与积分口径同理：已耗尽的包到期不作废任何额度，不该占最早到期位。
			if e > now && limit-used > 0 {
				expireQuota = earlier(expireQuota, e)
			}
			continue
		}
		limit = p.EntitlementBaseInfo.Quota.CreditsLimit
		if limit <= 0 {
			continue
		}
		l := int64(limit)
		usedCredits := int64(p.Usage.CreditsAmount)
		if remain := l - usedCredits; remain > 0 {
			// 与 CreditPackages 同口径：单包剩余不报负数（超支的包不该把别的包的余额抵掉）。
			u.Remain += remain
			// expire 只认还有余量的包：已用完的包到期不作废任何东西，若让它占据
			// 最早到期位，renderExpiry 会把全部余额错配给它（显示「今天到期 N 分」
			// 直到真实到期包顶替），ExpiringSoon 选号窗口也会被带偏。
			if e > now {
				dueAt(e, remain)
			}
		}
		u.Total += l
	}
	if expireQuota > 0 {
		u.Expire, u.ExpireCredits = expireQuota, dueCredits
	} else {
		u.Expire = expireAny
	}
	// 兜底：packs 一个都不带面额时（免费包只给布尔开关、没有 credits_limit/basic_usage_limit），
	// 用顶级 usage_summary——实测与 Σpacks 逐位吻合，是同一口径的另一条路。
	// shortcut: 只兜「packs 全无面额」；部分无面额（混档账号）仍以 packs 为准，需要时再细分。
	if sm := resp.UsageSummary; sm.TotalAmount > 0 && u.Total == 0 && u.LimitUSD == 0 {
		if u.Unit == UnitUSD {
			u.LimitUSD, u.UsedUSD = sm.TotalAmount, sm.ConsumedAmount
		} else {
			u.Total = int64(sm.TotalAmount)
			if u.Remain = u.Total - int64(sm.ConsumedAmount); u.Remain < 0 {
				u.Remain = 0
			}
		}
	}
	return u, nil
}

// UserEntUsageDetail 标准版：走主 HTTP（与 SSE 共享连接池，高并发下可能被饿死）。
func (c *Client) UserEntUsageDetail(a *auth.Auth) (EntUsage, error) {
	return c.userEntUsageDetailFrom(a, c.fetchEntUsage)
}

// UserEntUsageDetailFast 走 balanceCli（独立 Transport、15s 超时），
// 定时余额刷新（RunBalanceRefresh）应改用这个：高并发 chat 期间不再被堵死。
func (c *Client) UserEntUsageDetailFast(a *auth.Auth) (EntUsage, error) {
	return c.userEntUsageDetailFrom(a, c.FetchEntUsageFast)
}

// UserEntUsage 返回剩余积分、积分总额与最早的积分到期时间（积分口径的老接口，行为不变）。
//
// 国际版是美元计费、积分恒 0，走这个接口拿不到任何有用数字——需要国际版口径请用
// UserEntUsageDetail（它会返回 Unit=usd 与美元字段）。
func (c *Client) UserEntUsage(a *auth.Auth) (remain, total, expire int64, err error) {
	u, err := c.UserEntUsageDetail(a)
	if err != nil {
		return 0, 0, 0, err
	}
	return u.Remain, u.Total, u.Expire, nil
}

// earlier 取两个时刻里更早的非零值（0 = 未知）。
func earlier(cur, v int64) int64 {
	if v <= 0 {
		return cur
	}
	if cur == 0 || v < cur {
		return v
	}
	return cur
}

// CreditPackage 单个额度包的构成明细（面板「积分构成 / 额度构成」用）。
//
// 上游 ide_user_ent_usage 的 user_entitlement_pack_list 每条即一个包（国内实测 13 条：
// 老用户福利 / 免费 / 每月登录赠送 / 每日签到…）。只看聚合值看不出「余额为什么差
// 这么多」，差别藏在包的面额与到期时间里。
//
// 两地区币种不同（见 entUsagePack 的实测说明），所以带上 Unit：
// 积分口径填 Remain/Used/Size，美元口径填 RemainUSD/UsedUSD/SizeUSD。
// **不要**把两边数字相加或比较——单位不可换算。
type CreditPackage struct {
	Name string `json:"name"` // display_desc（老用户福利 / 每月登录赠送 / 签到奖励 / Free plan）
	// Group 分组名（用户福利 / 每月登录积分 / 每日签到），display_desc 缺失时的兜底展示名。
	Group     string `json:"group,omitempty"`
	GroupType int    `json:"group_type,omitempty"`
	Unit      string `json:"unit"`   // UnitCredit | UnitUSD
	Remain    int64  `json:"remain"` // credits_limit - credits_amount（Unit=credit）
	Used      int64  `json:"used"`
	Size      int64  `json:"size"` // credits_limit
	// 美元口径（Unit=usd）。实测 Free plan：limit=1、used=0.00059 —— 走 int64 会全是 0。
	RemainUSD float64 `json:"remain_usd,omitempty"`
	UsedUSD   float64 `json:"used_usd,omitempty"`
	SizeUSD   float64 `json:"limit_usd,omitempty"`
	Expire    int64   `json:"expire,omitempty"` // 到期时间（Unix 秒）；0 = 上游没给
	Start     int64   `json:"start,omitempty"`  // 发放时间（Unix 秒）
	EntID     string  `json:"ent_id,omitempty"` // entitlement_id 或 source_id，供面板去重/对齐
	Ratio     float64 `json:"-"`                // 占位：上游的用量比例已在 usage 里，暂不用
}

// BalancePackages 口径感知的逐包构成 + 汇总（美元号也拿得到东西）。
//
// 判口径同 UserEntUsageDetail：只看顶层 is_dollar_usage_billing。
// balancePackagesFrom 给定 fetcher 时的共用骨架（BalancePackages / BalancePackagesFast 共用）。
func (c *Client) balancePackagesFrom(a *auth.Auth, fetch func(*auth.Auth) (entUsageResponse, error)) ([]CreditPackage, EntUsage, error) {
	resp, err := fetch(a)
	if err != nil {
		return nil, EntUsage{}, err
	}
	u := EntUsage{Unit: UnitCredit}
	if resp.IsDollarUsageBill {
		u.Unit = UnitUSD
	}
	now := time.Now().Unix()
	var expireQuota, expireAny int64
	// 与 userEntUsageDetailFrom 同口径：expireQuota 与「该日到期积分」一起维护。
	var dueCredits int64
	dueAt := func(e int64, remain int64) {
		switch {
		case e <= 0:
		case expireQuota == 0 || e < expireQuota:
			expireQuota, dueCredits = e, remain
		case e == expireQuota:
			dueCredits += remain
		}
	}
	out := make([]CreditPackage, 0, len(resp.UserEntitlementPackList))
	for _, p := range resp.UserEntitlementPackList {
		e := packExpiry(p)
		if e > now {
			expireAny = earlier(expireAny, e)
		}
		id := p.EntitlementBaseInfo.EntitlementID
		if id == "" {
			id = p.SourceID
		}
		base := CreditPackage{
			Name: p.DisplayDesc, Group: p.GroupName, GroupType: p.GroupType,
			Unit: u.Unit, Expire: e, Start: p.EntitlementBaseInfo.StartTime, EntID: id,
		}
		if u.Unit == UnitUSD {
			// 与 UserEntUsageDetail 同口径：basic + bonus 才是这个包的总面额/总用量。
			limit := p.EntitlementBaseInfo.Quota.BasicUsageLimit + p.EntitlementBaseInfo.Quota.BonusUsageLimit
			used := p.Usage.BasicUsageAmount + p.Usage.BonusUsageAmount
			if limit <= 0 {
				continue // 没有面额的包（订阅权益那类）不算额度包
			}
			remain := limit - used
			if remain < 0 {
				remain = 0
			}
			base.RemainUSD, base.UsedUSD, base.SizeUSD = remain, used, limit
			u.LimitUSD += limit
			u.UsedUSD += used
			if e > now && remain > 0 {
				expireQuota = earlier(expireQuota, e)
			}
			out = append(out, base)
			continue
		}
		limit := int64(p.EntitlementBaseInfo.Quota.CreditsLimit)
		if limit <= 0 {
			continue
		}
		used := int64(p.Usage.CreditsAmount)
		remain := limit - used
		if remain < 0 {
			remain = 0
		}
		base.Remain, base.Used, base.Size = remain, used, limit
		u.Remain += remain
		u.Total += limit
		// 同 userEntUsageDetailFrom：只认还有余量的包（remain=0 的包到期无损失）。
		if e > now && remain > 0 {
			dueAt(e, remain)
		}
		out = append(out, base)
	}
	if expireQuota > 0 {
		u.Expire, u.ExpireCredits = expireQuota, dueCredits
	} else {
		u.Expire = expireAny
	}
	return out, u, nil
}

// BalancePackages 标准版：走主 HTTP（120s 超时，连接池跟 SSE 共享）。
// 高并发 chat 期间可能被饿死，调用方在意延迟用 BalancePackagesFast。
func (c *Client) BalancePackages(a *auth.Auth) ([]CreditPackage, EntUsage, error) {
	return c.balancePackagesFrom(a, c.fetchEntUsage)
}

// CreditPackages 积分口径的老接口（行为不变）：只收 credits_limit>0 的包。
// 国际版免费号（credits_limit=0）返回空列表 + 0/0 —— 需要国际版口径请用 BalancePackages。
func (c *Client) CreditPackages(a *auth.Auth) ([]CreditPackage, int64, int64, error) {
	packs, u, err := c.BalancePackages(a)
	if err != nil {
		return nil, 0, 0, err
	}
	out := make([]CreditPackage, 0, len(packs))
	for _, p := range packs {
		if p.Unit != UnitCredit {
			continue
		}
		out = append(out, p)
	}
	return out, u.Remain, u.Total, nil
}

// SetTimeouts 更新超时三元组：短 RPC 总时长 / 聊天首字节 / 聊天流内空闲。
// 流式整流仍不设总超时（长推理不该被截断），"上游既不吐数据也不断连"交给空闲看门狗。
// 持写锁：请求路径持读锁跨过 Do，两边互斥后才不会改到正在用的 http.Client。
func (c *Client) SetTimeouts(short, header, idle time.Duration) {
	if c == nil || c.HTTP == nil {
		return
	}
	c.timeoutMu.Lock()
	defer c.timeoutMu.Unlock()
	if short > 0 {
		c.HTTP.Timeout = short
	}
	if header > 0 {
		setHeaderTimeout(c.HTTP, header)
		setHeaderTimeout(c.StreamHTTP, header)
	}
	if idle > 0 {
		c.idleTimeout = idle
	}
}

// SetTimeout 兼容旧调用：只改短 RPC 与首字节超时，流内空闲保持原值。
func (c *Client) SetTimeout(d time.Duration) { c.SetTimeouts(d, d, 0) }

// idleTimeoutReader 流式读的空闲看门狗：idle 内没有字节到达就关掉底层 body，
// 让上层 Read 立刻报错，而不是永久挂着（上游既不吐数据也不断连的场景）。
// ponytail: 每次 Read 起一个 timer；SSE 分块数不多，真到高频再换单 timer 复用。
type idleTimeoutReader struct {
	rc   io.ReadCloser
	idle time.Duration
}

func (r *idleTimeoutReader) Read(p []byte) (int, error) {
	t := time.AfterFunc(r.idle, func() { _ = r.rc.Close() })
	n, err := r.rc.Read(p)
	t.Stop()
	return n, err
}

func (r *idleTimeoutReader) Close() error { return r.rc.Close() }

func setHeaderTimeout(hc *http.Client, d time.Duration) {
	if hc == nil {
		return
	}
	if tr, ok := hc.Transport.(*http.Transport); ok {
		tr.ResponseHeaderTimeout = d
	}
}

// GetUserInfo 查询账号信息（登录用）。
func (c *Client) GetUserInfo(a *auth.Auth) (uid, nickname, enterpriseID string, err error) {
	host := a.ApiHost
	if host == "" {
		host = c.oauthBase()
	}
	// 用 currentIdeVersion()（含 SetIdentity 覆盖）而不是内置常量：登录请求是账号上线时的
	// 第一批出站流量，版本覆盖没生效就等于新号一上来就用老版本握手。
	body := map[string]any{"ReqSource": "IDE", "IDEVersion": currentIdeVersion()}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, host+EpUserInfo, bytes.NewReader(raw))
	if err != nil {
		return "", "", "", err
	}
	OAuthHeaders(req)
	req.Header.Set("X-Cloudide-Token", a.JWT()) // 读锁快照
	data, err := c.doJSON(req)
	if err != nil {
		return "", "", "", err
	}
	var resp struct {
		Result struct {
			UserID       string `json:"UserID"`
			ScreenName   string `json:"ScreenName"`
			EnterpriseID string `json:"EnterpriseID"`
		} `json:"Result"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return "", "", "", fmt.Errorf("userinfo parse: %w", err)
	}
	return resp.Result.UserID, resp.Result.ScreenName, resp.Result.EnterpriseID, nil
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n]
	}
	return s
}
