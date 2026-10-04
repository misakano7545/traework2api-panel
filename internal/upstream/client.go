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
	"strings"
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
const checkinAlreadyCode = 9095

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

var sessionDeadMarkers = []string{"login", "token 失效", "token invalid", "session", "unauthorized", "401"}

// Classify 按 HTTP 状态码 + body 判定错误类别（SPEC §4.3）。
func Classify(status int, body string) ErrKind {
	lower := strings.ToLower(body)
	// 1005 plan 权益不足
	if strings.Contains(body, `"code":1005`) || (strings.Contains(body, "1005") && strings.Contains(lower, "plan")) {
		return ErrPlanLimit
	}
	// session 失效
	if status == http.StatusUnauthorized {
		for _, m := range sessionDeadMarkers {
			if strings.Contains(lower, strings.ToLower(m)) {
				return ErrSessionDead
			}
		}
		return ErrSessionDead
	}
	if status == http.StatusTooManyRequests {
		return ErrSoftRate
	}
	if status == http.StatusNotFound {
		return ErrNotFound
	}
	if status >= 500 {
		return ErrServer
	}
	if status >= 400 {
		return ErrClient
	}
	return ErrNone
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

	AgentHost string // https://trae-api-cn.mchost.guru
	// AgentHostIntl 国际版（realm=intl）的 agent host；空则用内置默认。
	AgentHostIntl string // https://a0ai-api-sg.byteintlapi.com
	UgHost        string // https://api.trae.cn
	OAuthHost     string // https://api.trae.com.cn
	ClientID      string // en1oxy7wnw8j9n
}

// New 生产默认值。配置连接池减少 TLS 握手。
func New() *Client {
	tr := &http.Transport{
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   20,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: 120 * time.Second, // 首字节兜底（长推理预留），不限制整流时长
	}
	return &Client{
		HTTP:          &http.Client{Timeout: 120 * time.Second, Transport: tr},
		StreamHTTP:    &http.Client{Transport: tr}, // 无总超时
		AgentHost:     AgentHost,
		AgentHostIntl: AgentHostIntl,
		UgHost:        UgHost,
		OAuthHost:     OAuthHost,
		ClientID:      ClientID,
	}
}

func (c *Client) agentBase() string { return c.AgentHost }
func (c *Client) ugBase() string    { return c.UgHost }
func (c *Client) oauthBase() string { return c.OAuthHost }

// agentBaseFor 按账号地区选 agent host（对话 + 模型表都走这里）。
// Auth 为 nil（如配置校验路径）按国内版。
func (c *Client) agentBaseFor(a *auth.Auth) string {
	if a != nil && a.Realm() == auth.RealmIntl {
		if c.AgentHostIntl != "" {
			return c.AgentHostIntl
		}
		return AgentHostIntl
	}
	return c.agentBase()
}

// identFor 账号地区对应的身份版本组（User-Agent / X-Ide-Version / *-Version-Code）。
func identFor(a *auth.Auth) (version, versionCode string) {
	if a != nil && a.Realm() == auth.RealmIntl {
		return IdeVersionIntl, IdeVersionCodeIntl
	}
	return currentIdeVersion(), IdeVersionCode
}

// doJSON 发请求并解 JSON；HTTP 非 2xx 时返回带 body 片段的 *Error。
func (c *Client) doJSON(req *http.Request) (json.RawMessage, error) {
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
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
		return err
	}
	var resp struct {
		Result struct {
			Token               string `json:"Token"`
			TokenExpireAt       int64  `json:"TokenExpireAt"`
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
	if resp.Result.TokenExpireAt > 0 {
		a.ExpiresAt = normalizeExpiresAt(resp.Result.TokenExpireAt)
	} else if resp.Result.TokenExpireDuration > 0 {
		a.ExpiresAt = time.Now().Add(time.Duration(resp.Result.TokenExpireDuration) * time.Second).Unix()
	}
	return nil
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
	hc := c.HTTP
	if c.StreamHTTP != nil {
		hc = c.StreamHTTP
	}
	resp, err := hc.Do(req)
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
	if c.idleTimeout > 0 {
		return &idleTimeoutReader{rc: resp.Body, idle: c.idleTimeout}, resp.StatusCode, nil, nil
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

	// Function 该模型属于哪条通道（solo_work_lite / solo_coder）：出站 function 要跟着模型走，
	// 不然 coder 面的模型会被 Work 通道判 4001。
	Function string
}

// paramConfig 是 get_detail_param 响应里的一条模型配置。
type paramConfig struct {
	ConfigName        string `json:"config_name"`
	IsInvisibleToUser bool   `json:"is_invisible_to_user"`
	Usage             string `json:"usage"`
	DisplayConfig     struct {
		DisplayName     string `json:"display_name"`
		ModelCapability string `json:"model_capability"`
	} `json:"display_config"`
	ContextWindowTokens struct {
		Dev int64 `json:"dev"`
	} `json:"context_window_tokens"`
	ModelDetailList []struct {
		MaxTokens int64 `json:"max_tokens"`
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
		mi := ModelInfo{
			ID:            cfg.ConfigName,
			Name:          cfg.DisplayConfig.DisplayName,
			ContextWindow: cfg.ContextWindowTokens.Dev,
			Function:      function,
		}
		if len(cfg.ModelDetailList) > 0 {
			mi.MaxTokens = cfg.ModelDetailList[0].MaxTokens
			mi.Thinking = thinkingType(cfg.ModelDetailList[0].ModelExtraConfig)
		}
		mi.Capability = cfg.DisplayConfig.ModelCapability
		if len(cfg.ReasoningEffortConfig) > 0 && string(cfg.ReasoningEffortConfig) != "null" {
			mi.Effort = string(cfg.ReasoningEffortConfig)
		}
		out = append(out, mi)
	}
	return out
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
	return out, nil
}

// CheckinStatus 查询签到状态；credits 返回**本次可领**的积分（上游 credits + extra_credits，
// 实测免费档 150+50=200，与官方「每日签到」口径一致）。
func (c *Client) CheckinStatus(a *auth.Auth) (checkedIn bool, credits int64, enable bool, err error) {
	if a != nil && a.Realm() == auth.RealmIntl {
		return false, 0, false, ErrNoIntlUG
	}
	req, err := http.NewRequest(http.MethodPost, c.ugBase()+EpCheckinStatus, bytes.NewReader([]byte("{}")))
	if err != nil {
		return false, 0, false, err
	}
	UgHeaders(req, a)
	data, err := c.doJSON(req)
	if err != nil {
		return false, 0, false, err
	}
	var resp struct {
		CheckedIn    bool   `json:"checked_in"`
		Credits      int64  `json:"credits"`
		ExtraCredits int64  `json:"extra_credits"`
		Enable       bool   `json:"enable"`
		Code         int    `json:"code"`
		Message      string `json:"message"`
		Msg          string `json:"msg"`
		Success      *bool  `json:"success"`
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
	return resp.CheckedIn, resp.Credits + resp.ExtraCredits, resp.Enable, nil
}

// CheckinClaim 执行签到。
func (c *Client) CheckinClaim(a *auth.Auth) error {
	if a != nil && a.Realm() == auth.RealmIntl {
		return ErrNoIntlUG
	}
	req, err := http.NewRequest(http.MethodPost, c.ugBase()+EpCheckinClaim, bytes.NewReader([]byte("{}")))
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

// entUsageBody 积分接口请求体。用**客户端自己的形状**，不是空体：
// 客户端自带 SDK（`GetIdeUserEntUsageV2`）发的是 {require_usage, req_source, full_data, Request}，
// 调用点默认 {require_usage:true, full_data:true}（NextAgentX/trae-workbuddy-switch 从客户端
// 缓存里翻出的原文）。实测本机账号上 `{}` 与这个体返回的 pack 列表完全一致，所以这是去掉
// 一个潜在失败模式（服务端若哪天把 full_data 默认成 false，`{}` 会拿不到完整 pack 列表）
// 而非修 bug —— 值不值得改就看这一点。
const entUsageBody = `{"require_usage":true,"full_data":true}`

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

// entUsagePack 一个套餐包的原始字段（国内 v2 与国际 v1 同形）。
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
			CreditsLimit int64 `json:"credits_limit"`
		} `json:"quota"`
	} `json:"entitlement_base_info"`
	Usage struct {
		CreditsAmount float64 `json:"credits_amount"`
	} `json:"usage"`
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

// UserEntUsage 返回剩余积分、积分总额与最早的积分到期时间。
// 每个 credits_limit>0 的包：remain += limit - used，
// used 为 usage.credits_amount（与 cmd/credit 同一口径，截断为整数）。
//
// 到期时间取两档（见下 packExpiry 注释）：
//  1. 有积分面额的包里最早的一个未到期时刻——这是「积分到期」，也是选号「快过期优先」的依据；
//  2. 一个积分包都没有时（国际版免费号只有 Free plan，credits_limit=0），回落到所有包的
//     周期截止 end_time——否则国际号的「到期日期」恒为空，面板看着像功能坏了。
//
// 国际版走 v1 路径 + 计费网关（见 entUsagePath / ugBaseFor）。
func (c *Client) UserEntUsage(a *auth.Auth) (remain, total, expire int64, err error) {
	req, err := http.NewRequest(http.MethodPost, c.ugBaseFor(a)+entUsagePath(a), bytes.NewReader([]byte(entUsageBody)))
	if err != nil {
		return 0, 0, 0, err
	}
	UgHeaders(req, a)
	data, err := c.doJSON(req)
	if err != nil {
		return 0, 0, 0, err
	}
	var resp struct {
		IsCreditsBilling bool `json:"is_credits_billing"`
		// UserEntitlementPackList 套餐列表（两地区同形，见 entUsagePack）。
		UserEntitlementPackList []entUsagePack `json:"user_entitlement_pack_list"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return 0, 0, 0, fmt.Errorf("ent usage parse: %w", err)
	}
	if len(resp.UserEntitlementPackList) == 0 {
		// 空包列表 = 接口返回了异常结构（未登录/风控/结构变了），**不是**「剩余 0 分」。
		// 当成 0 分会把好号的积分静默清零（面板显示 0、选号权重一起塌），照
		// star620/TraeTools 的判据：宁可报解析失败，让调用方保留上一次的值。
		return 0, 0, 0, errors.New("ent usage: 包列表为空（接口异常，不当作 0 分）")
	}
	now := time.Now().Unix()
	var expireCredit, expireAny int64
	for _, p := range resp.UserEntitlementPackList {
		e := packExpiry(p)
		if e > now {
			expireAny = earlier(expireAny, e)
		}
		limit := p.EntitlementBaseInfo.Quota.CreditsLimit
		if limit <= 0 {
			continue
		}
		remain += limit - int64(p.Usage.CreditsAmount)
		total += limit // 面板「积分」列显示 剩余/总额
		if e > now {
			expireCredit = earlier(expireCredit, e)
		}
	}
	if expireCredit > 0 {
		return remain, total, expireCredit, nil
	}
	return remain, total, expireAny, nil
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

// CreditPackage 单个积分包的构成明细（面板「积分构成」用）。
//
// 上游 ide_user_ent_usage 的 user_entitlement_pack_list 每条即一个包（实测 5 条：
// 老用户福利 / 免费 / 每月登录赠送 / 每日签到…）。只看聚合值看不出「余额为什么差
// 这么多」，差别藏在包的面额（credits_limit）与到期时间（expire_time）里。
type CreditPackage struct {
	Name string `json:"name"` // display_desc（老用户福利 / 每月登录赠送 / 签到奖励）
	// Group 分组名（用户福利 / 每月登录积分 / 每日签到），display_desc 缺失时的兜底展示名。
	Group     string  `json:"group,omitempty"`
	GroupType int     `json:"group_type,omitempty"`
	Remain    int64   `json:"remain"` // credits_limit - credits_amount
	Used      int64   `json:"used"`
	Size      int64   `json:"size"`             // credits_limit
	Expire    int64   `json:"expire,omitempty"` // 到期时间（Unix 秒）；0 = 上游没给
	Start     int64   `json:"start,omitempty"`  // 发放时间（Unix 秒）
	EntID     string  `json:"ent_id,omitempty"` // entitlement_id 或 source_id，供面板去重/对齐
	Ratio     float64 `json:"-"`                // 占位：上游的用量比例已在 usage 里，暂不用
}

// CreditPackages 拉账号当前的逐包构成，并返回各包 remain / size 之和。
// 只有 credits_limit > 0 的包算积分包（国内「免费」那条是订阅权益、没有面额，跳过）。
// 国际版走 v1 路径（见 entUsagePath）：付费国际号有面额、免费号只有一条
// `Free plan`（credits_limit=0）→ 返回空列表 + 0/0，面板据此显示「无积分套餐」，
// 而不是把国际号整条当成接口不可用。
func (c *Client) CreditPackages(a *auth.Auth) ([]CreditPackage, int64, int64, error) {
	req, err := http.NewRequest(http.MethodPost, c.ugBaseFor(a)+entUsagePath(a), bytes.NewReader([]byte(entUsageBody)))
	if err != nil {
		return nil, 0, 0, err
	}
	UgHeaders(req, a)
	data, err := c.doJSON(req)
	if err != nil {
		return nil, 0, 0, err
	}
	var resp struct {
		UserEntitlementPackList []entUsagePack `json:"user_entitlement_pack_list"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, 0, 0, fmt.Errorf("packages parse: %w", err)
	}
	out := make([]CreditPackage, 0, len(resp.UserEntitlementPackList))
	var sumRemain, sumSize int64
	for _, p := range resp.UserEntitlementPackList {
		limit := p.EntitlementBaseInfo.Quota.CreditsLimit
		if limit <= 0 {
			continue
		}
		used := int64(p.Usage.CreditsAmount)
		remain := limit - used
		if remain < 0 {
			remain = 0
		}
		id := p.EntitlementBaseInfo.EntitlementID
		if id == "" {
			id = p.SourceID
		}
		out = append(out, CreditPackage{
			Name:      p.DisplayDesc,
			Group:     p.GroupName,
			GroupType: p.GroupType,
			Remain:    remain,
			Used:      used,
			Size:      limit,
			Expire:    packExpiry(p),
			Start:     p.EntitlementBaseInfo.StartTime,
			EntID:     id,
		})
		sumRemain += remain
		sumSize += limit
	}
	return out, sumRemain, sumSize, nil
}

// SetTimeouts 更新超时三元组：短 RPC 总时长 / 聊天首字节 / 聊天流内空闲。
// 流式整流仍不设总超时（长推理不该被截断），"上游既不吐数据也不断连"交给空闲看门狗。
// ponytail: 与在途 Do 并发写；管理页保存很稀，-race 报警再加锁。
func (c *Client) SetTimeouts(short, header, idle time.Duration) {
	if c == nil || c.HTTP == nil {
		return
	}
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
	body := map[string]any{"ReqSource": "IDE", "IDEVersion": IdeVersion}
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
