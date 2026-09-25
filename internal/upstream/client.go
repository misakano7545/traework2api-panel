// client.go SOLO 上游客户端：llm_utils_chat / get_detail_param / ExchangeToken /
// checkin_credits / ide_user_ent_usage + 错误分类。
package upstream

import (
	"bytes"
	"encoding/json"
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
	UgHost    string // https://api.trae.cn
	OAuthHost string // https://api.trae.com.cn
	ClientID  string // en1oxy7wnw8j9n
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
		HTTP:       &http.Client{Timeout: 120 * time.Second, Transport: tr},
		StreamHTTP: &http.Client{Transport: tr}, // 无总超时
		AgentHost:  AgentHost,
		UgHost:     UgHost,
		OAuthHost:  OAuthHost,
		ClientID:   ClientID,
	}
}

func (c *Client) agentBase() string { return c.AgentHost }
func (c *Client) ugBase() string    { return c.UgHost }
func (c *Client) oauthBase() string { return c.OAuthHost }

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
	req, err := http.NewRequest(http.MethodPost, c.agentBase()+EpChat, bytes.NewReader(PrepareBody(body)))
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
// 实测上游返回 39 条内部配置，其中官方模型 15 条：
// 排除 9 条 is_invisible_to_user（子代理等）+ 14 条 custom_model_* + 1 条 summary。
func pickOfficialModels(list []paramConfig) []ModelInfo {
	out := make([]ModelInfo, 0, len(list))
	for _, cfg := range list {
		if cfg.ConfigName == "" || cfg.IsInvisibleToUser || cfg.Usage != usageChat {
			continue
		}
		mi := ModelInfo{
			ID:            cfg.ConfigName,
			Name:          cfg.DisplayConfig.DisplayName,
			ContextWindow: cfg.ContextWindowTokens.Dev,
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
	raw, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, c.agentBase()+EpModels, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	SOLOHeaders(req, a, false)
	data, err := c.doJSON(req)
	if err != nil {
		return nil, err
	}
	var resp struct {
		ConfigInfoList []paramConfig `json:"config_info_list"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("models parse: %w", err)
	}
	out := pickOfficialModels(resp.ConfigInfoList)
	if len(out) == 0 {
		return nil, fmt.Errorf("models api returned no official model (got %d raw configs)", len(resp.ConfigInfoList))
	}
	return out, nil
}

// CheckinStatus 查询签到状态。
func (c *Client) CheckinStatus(a *auth.Auth) (checkedIn bool, credits int64, enable bool, err error) {
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
		CheckedIn bool  `json:"checked_in"`
		Credits   int64 `json:"credits"`
		Enable    bool  `json:"enable"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return false, 0, false, fmt.Errorf("checkin status parse: %w", err)
	}
	return resp.CheckedIn, resp.Credits, resp.Enable, nil
}

// CheckinClaim 执行签到。
func (c *Client) CheckinClaim(a *auth.Auth) error {
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
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return fmt.Errorf("checkin claim parse: %w", err)
	}
	if resp.Code != 0 {
		return &BusinessError{Code: resp.Code, Message: "签到失败：" + resp.Message}
	}
	return nil
}

// UserEntUsage 返回剩余积分、积分总额与最早的积分到期时间。
// 每个 credits_limit>0 的包：remain += limit - used，
// used 为 usage.credits_amount（与 cmd/credit 同一口径，截断为整数）。
// expire 只在 expire_time > now 的包里取最小值（已过期的包不影响紧迫度排序），无则 0。
func (c *Client) UserEntUsage(a *auth.Auth) (remain, total, expire int64, err error) {
	req, err := http.NewRequest(http.MethodPost, c.ugBase()+EpEntUsage, bytes.NewReader([]byte("{}")))
	if err != nil {
		return 0, 0, 0, err
	}
	UgHeaders(req, a)
	data, err := c.doJSON(req)
	if err != nil {
		return 0, 0, 0, err
	}
	var resp struct {
		IsCreditsBilling        bool `json:"is_credits_billing"`
		UserEntitlementPackList []struct {
			ExpireTime          int64 `json:"expire_time"`
			EntitlementBaseInfo struct {
				Quota struct {
					CreditsLimit int64 `json:"credits_limit"`
				} `json:"quota"`
			} `json:"entitlement_base_info"`
			Usage struct {
				CreditsAmount float64 `json:"credits_amount"`
			} `json:"usage"`
		} `json:"user_entitlement_pack_list"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return 0, 0, 0, fmt.Errorf("ent usage parse: %w", err)
	}
	now := time.Now().Unix()
	for _, p := range resp.UserEntitlementPackList {
		limit := p.EntitlementBaseInfo.Quota.CreditsLimit
		if limit <= 0 {
			continue
		}
		remain += limit - int64(p.Usage.CreditsAmount)
		total += limit // 面板「积分」列显示 剩余/总额
		if p.ExpireTime > now && (expire == 0 || p.ExpireTime < expire) {
			expire = p.ExpireTime
		}
	}
	return remain, total, expire, nil
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
