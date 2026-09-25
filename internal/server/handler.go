// Package server 暴露 OpenAI 兼容 HTTP 接口，内部驱动 pool 挑号 + upstream 转发。
package server

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"traework2api/internal/auth"
	"traework2api/internal/pool"
	"traework2api/internal/prompt"
	"traework2api/internal/session"
	"traework2api/internal/upstream"
	"traework2api/internal/usage"
)

// Config handler 依赖。
type Config struct {
	Pool         *pool.Pool
	Upstream     *upstream.Client
	APIKey       string          // 空 = 不鉴权
	MaxRotate    int             // 单请求最多换号次数，默认 3
	RefreshSkew  time.Duration   // token 预刷新窗口，默认 24h
	DefaultModel string          // 默认 glm-5.2
	PromptMode   string          // passthrough / custom / append
	PromptText   string          // custom/append 用的提示词文本
	Session      *session.Router // 可选，会话粘性（同一会话粘同一账号）
	Panel        http.Handler    // 可选，/panel/
	Usage        *usage.Recorder // 可选，逐请求用量台账（按 账号×模型×时间片 分桶）
	// 冷却/熔断/在途等池参数不在这里：它们在 pool.Limits（pool.ApplyLimits 热改），
	// 由 pool 自己持有，避免"参数在 handler、执行在 pool"的两处漂移。
}

// maxBodyBytes 请求体大小上限（8MB），超过返回 413。
const maxBodyBytes = 8 << 20

// Handler 主路由。
type Handler struct {
	cfg  Config
	mux  *http.ServeMux
	rtMu sync.RWMutex
}

// NewHandler 构建 handler。
func NewHandler(cfg Config) *Handler {
	if cfg.MaxRotate <= 0 {
		cfg.MaxRotate = 3
	}
	if cfg.RefreshSkew <= 0 {
		cfg.RefreshSkew = 24 * time.Hour
	}
	if cfg.DefaultModel == "" {
		cfg.DefaultModel = upstream.DefaultConfigName
	}
	h := &Handler{cfg: cfg, mux: http.NewServeMux()}
	h.mux.HandleFunc("POST /v1/chat/completions", h.withAuth(h.chatCompletions))
	h.mux.HandleFunc("GET /v1/models", h.withAuth(h.models))
	h.mux.HandleFunc("GET /status", h.withAuth(h.status))
	h.mux.HandleFunc("GET /healthz", h.healthz)
	if cfg.Panel != nil {
		mountPanel(h.mux, cfg.Panel)
	}
	return h
}

func mountPanel(mux *http.ServeMux, p http.Handler) {
	mux.Handle("/panel/", p)
	mux.HandleFunc("/panel", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/panel/", http.StatusPermanentRedirect)
	})
}

// MountPanel 在 NewHandler 之后挂上面板。
func (h *Handler) MountPanel(p http.Handler) {
	if h != nil && p != nil {
		mountPanel(h.mux, p)
	}
}

// Models 复用 /v1/models 的动态缓存与静态回退。
func (h *Handler) Models() []map[string]any { return h.modelList() }

// Runtime handler 侧可热改的设置（池参数在 pool 里，见 Config 注释）。
type Runtime struct {
	DefaultModel string
	PromptMode   string
	PromptText   string
	// APIKey 空 = 不鉴权。整份 Runtime 一起套用，所以"面板里把密钥清空"也能立即生效。
	APIKey string
}

// ApplyRuntime 立即改默认模型、出站提示词改写与 API 密钥（面板保存配置时调用）。
func (h *Handler) ApplyRuntime(rt Runtime) {
	h.rtMu.Lock()
	defer h.rtMu.Unlock()
	if rt.DefaultModel != "" {
		h.cfg.DefaultModel = rt.DefaultModel
	}
	h.cfg.PromptMode, h.cfg.PromptText = rt.PromptMode, rt.PromptText
	h.cfg.APIKey = rt.APIKey
}

func (h *Handler) runtime() Runtime {
	h.rtMu.RLock()
	defer h.rtMu.RUnlock()
	return Runtime{
		DefaultModel: h.cfg.DefaultModel,
		PromptMode:   h.cfg.PromptMode,
		PromptText:   h.cfg.PromptText,
		APIKey:       h.cfg.APIKey,
	}
}

// applyPrompt 出站前按模式改写系统提示词；passthrough（默认）原样返回。
func (h *Handler) applyPrompt(body []byte) []byte {
	rt := h.runtime()
	switch rt.PromptMode {
	case "custom":
		return prompt.Rewrite(body, rt.PromptText)
	case "append":
		return prompt.Append(body, rt.PromptText)
	}
	return body
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mux.ServeHTTP(w, r)
}

func (h *Handler) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// 密钥随配置热改（面板改了立刻用新的），所以每次都从 runtime 读，不缓存到局部闭包。
		want := h.runtime().APIKey
		if want != "" {
			authz := r.Header.Get("Authorization")
			const prefix = "Bearer "
			if len(authz) < len(prefix) || !strings.EqualFold(authz[:len(prefix)], prefix) {
				writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "missing or invalid API key")
				return
			}
			key := authz[len(prefix):]
			// 常量时间比较，防时序攻击（本地代理但按规范）。
			if subtle.ConstantTimeCompare([]byte(key), []byte(want)) != 1 {
				writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "missing or invalid API key")
				return
			}
		}
		next(w, r)
	}
}

func (h *Handler) healthz(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func (h *Handler) status(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"accounts": h.cfg.Pool.List(),
	})
}

// ---------------------------------------------------------------------------
// 模型映射
// ---------------------------------------------------------------------------

// mapModel 将客户端传入的 model 映射为 config_name（SPEC §4.5）：
//
//	"glm-5.2"（config_name）        → 直接转发
//	"glm-5.2__dev"（内部名）        → 去掉后缀映射回 config_name
//	"auto" / ""                     → 默认模型
//	其他未知                        → 400
func (h *Handler) mapModel(model string) (string, error) {
	model = strings.TrimSpace(model)
	if model == "" || model == "auto" {
		def := h.runtime().DefaultModel
		return def, nil
	}
	// 去掉内部名后缀（__dev / __max 等）
	base := model
	if i := strings.Index(model, "__"); i >= 0 {
		base = model[:i]
	}
	if h.knownModel(base) {
		return base, nil
	}
	// 宽松匹配：下划线 → 横线，大小写不敏感（deepseek_v4_pro → DeepSeek-V4-Pro）
	norm := normalizeModelName(base)
	if h.knownModel(norm) {
		return norm, nil
	}
	return "", fmt.Errorf("unknown model %q", model)
}

// normalizeModelName 将下划线命名的内部名归一化为 config_name 风格（横线分隔）。
func normalizeModelName(s string) string {
	parts := strings.Split(s, "_")
	for i, p := range parts {
		if p == "" {
			continue
		}
		parts[i] = strings.ToUpper(p[:1]) + strings.ToLower(p[1:])
	}
	return strings.Join(parts, "-")
}

// knownModel 判断 model 是否在动态/静态模型表中。
func (h *Handler) knownModel(model string) bool {
	for _, m := range h.modelList() {
		if m["id"] == model {
			return true
		}
	}
	return false
}

// 静态官方模型表（上游拉取失败时的回退快照）。
// 只列账号可选的官方模型：与 upstream.pickOfficialModels 同一口径，
// 不含 is_invisible_to_user 的内部子代理、custom_model_* 槽位与 summary。
// context_length 取上游 context_window_tokens.dev。
var staticModels = []map[string]any{
	{"id": "Doubao-Seed-Evolving", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 256000},
	{"id": "Doubao-Seed-2.1-Pro", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 256000},
	{"id": "Doubao-Seed-2.1-Turbo", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 256000},
	{"id": "glm-5.2", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 200000},
	{"id": "glm-5", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 200000},
	{"id": "DeepSeek-V4-Flash-Official", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 200000},
	{"id": "DeepSeek-V4-Flash", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 200000},
	{"id": "DeepSeek-V4-Pro-Official", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 200000},
	{"id": "DeepSeek-V4-Pro", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 200000},
	{"id": "kimi-k3", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 200000},
	{"id": "kimi-k2.7-code", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 200000},
	{"id": "kimi-k2.6", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 200000},
	{"id": "minimax-m3", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 200000},
	{"id": "qwen3.8-max", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 200000},
	{"id": "qwen-3.7-plus", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 200000},
}

// dynamicModelsCache 动态模型缓存（成功 1h / 失败负缓存 5min）。
var dynamicModelsCache struct {
	sync.RWMutex
	ids      []upstream.ModelInfo
	fetched  time.Time
	lastFail time.Time
}

const (
	dynamicModelsTTL        = time.Hour
	modelsFetchFailCooldown = 5 * time.Minute
)

func (h *Handler) models(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data":   h.modelList(),
	})
}

// modelList 动态获取模型列表并包装成 OpenAI 格式；失败回退静态表。
func (h *Handler) modelList() []map[string]any {
	if infos := h.fetchDynamicModels(); len(infos) > 0 {
		out := make([]map[string]any, 0, len(infos))
		for _, mi := range infos {
			entry := map[string]any{
				"id":             mi.ID,
				"object":         "model",
				"created":        1753600000,
				"owned_by":       "trae-solo",
				"context_length": mi.ContextWindow,
			}
			if entry["context_length"] == 0 {
				entry["context_length"] = 131072
			}
			// 思考相关字段按上游原值透出（OpenAI 客户端忽略未知键，面板用它显示「能力 / 思考」）。
			if mi.Capability != "" {
				entry["capability"] = mi.Capability
			}
			if mi.Thinking != "" {
				entry["thinking"] = mi.Thinking
			}
			if mi.Effort != "" {
				entry["reasoning_effort_config"] = mi.Effort
			}
			out = append(out, entry)
		}
		return out
	}
	return staticModels
}

// fetchDynamicModels 从池中任一健康账号拉模型列表（get_detail_param），缓存 1h。
func (h *Handler) fetchDynamicModels() []upstream.ModelInfo {
	dynamicModelsCache.RLock()
	if len(dynamicModelsCache.ids) > 0 && time.Since(dynamicModelsCache.fetched) < dynamicModelsTTL {
		out := dynamicModelsCache.ids
		dynamicModelsCache.RUnlock()
		return out
	}
	if !dynamicModelsCache.lastFail.IsZero() && time.Since(dynamicModelsCache.lastFail) < modelsFetchFailCooldown {
		dynamicModelsCache.RUnlock()
		return nil
	}
	dynamicModelsCache.RUnlock()

	// 只读探询不占在途名额：这里的 Pick 若带租约，几次列模型就会把账号占到不可用。
	acct := h.cfg.Pool.Peek()
	if acct == nil {
		return nil
	}
	infos, err := h.cfg.Upstream.FetchModels(acct)
	if err != nil || len(infos) == 0 {
		dynamicModelsCache.Lock()
		dynamicModelsCache.lastFail = time.Now()
		dynamicModelsCache.Unlock()
		return nil
	}
	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = infos
	dynamicModelsCache.fetched = time.Now()
	dynamicModelsCache.lastFail = time.Time{}
	dynamicModelsCache.Unlock()
	return infos
}

// ---------------------------------------------------------------------------
// chat
// ---------------------------------------------------------------------------

// setModelInBody 将 body 中 model 字段替换为 configName，并返回改写后的 body。
func setModelInBody(body []byte, configName string) []byte {
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	obj["model"] = configName
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}

// chatRequest 请求体里要用的几个字段（解析一次，别在多处重复 peek）。
type chatRequest struct {
	Stream bool   `json:"stream"`
	Model  string `json:"model"`
}

func (h *Handler) chatCompletions(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return
	}
	if len(body) > maxBodyBytes {
		writeOpenAIError(w, http.StatusRequestEntityTooLarge, "request_too_large", "request body exceeds 8MB limit")
		return
	}
	var peek chatRequest
	_ = json.Unmarshal(body, &peek)

	// 会话键必须在改写提示词之前取：改写会动 messages，之后取会让键漂移。
	sessKey := session.ExtractKey(body)

	configName, err := h.mapModel(peek.Model)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	body = setModelInBody(body, configName)
	body = h.applyPrompt(body)

	tried := map[string]bool{}
	var lastErr error
	for i := 0; i < h.cfg.MaxRotate; i++ {
		acct, release := h.acquireAccount(tried, sessKey)
		if acct == nil {
			break
		}
		tried[acct.UID] = true
		done, err := h.attempt(w, acct, body, peek, sessKey)
		release()
		if done {
			return
		}
		if err != nil {
			lastErr = err
		}
	}
	msg := "all accounts unavailable (cooling/disabled)"
	if lastErr != nil {
		msg += ": " + lastErr.Error()
	}
	writeOpenAIError(w, http.StatusServiceUnavailable, "no_healthy_account", msg)
}

// acquireAccount 挑一个账号并占用在途名额：会话粘性命中优先，否则按权重挑。
//
// 返回的 release 必须在这次尝试结束（无论成败）后调用一次——在途名额泄漏会把账号
// 慢慢挤出池（达到 max_in_flight 后不再可选）。
func (h *Handler) acquireAccount(tried map[string]bool, sessKey string) (*auth.Auth, func()) {
	if sessKey != "" && h.cfg.Session != nil {
		if uid, ok := h.cfg.Session.Resolve(sessKey); ok && !tried[uid] {
			if a, ok := h.cfg.Pool.AcquireIfHealthy(uid); ok {
				return a, func() { h.cfg.Pool.Release(uid) }
			}
			// 粘住的号不可用（冷却/禁用/在途满）→ 解绑，本轮重新分配。
			h.cfg.Session.Unbind(sessKey)
		}
	}
	a := h.cfg.Pool.PickExcluding(tried)
	if a == nil {
		return nil, func() {}
	}
	uid := a.UID
	return a, func() { h.cfg.Pool.Release(uid) }
}

// attempt 对一个账号跑一次完整尝试：预刷新 token → 出站 → 分类记账 → 写响应。
//
// done=true 表示响应已写给客户端，调用方必须停止轮转；err 非 nil 表示这次尝试失败、
// 可以换下一个号（错误只用于最终 503 的说明文案）。
func (h *Handler) attempt(w http.ResponseWriter, acct *auth.Auth, body []byte, peek chatRequest, sessKey string) (bool, error) {
	refreshed, err := h.cfg.Upstream.RefreshTokenIfNeeded(acct, h.cfg.RefreshSkew)
	if err != nil {
		var ue *upstream.Error
		if errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead {
			h.cfg.Pool.Disable(acct.UID, "refresh session dead")
		} else {
			// token 刷新失败不是一次对话调用，不计用量；但账号本身有问题，按罚号记一次。
			h.cfg.Pool.NoteError(acct.UID)
		}
		return false, err
	}
	if refreshed {
		_ = acct.SaveAtomic()
	}

	attemptStart := time.Now()
	rc, status, respBody, terr := h.cfg.Upstream.ChatStream(acct, body)
	if terr != nil {
		h.noteUsage(acct.UID, peek.Model, attemptStart, false, nil)
		h.cfg.Pool.NoteError(acct.UID)
		return false, terr
	}
	if status >= 400 {
		kind := upstream.Classify(status, string(respBody))
		h.noteUsage(acct.UID, peek.Model, attemptStart, false, nil)
		h.noteFailure(acct.UID, kind)
		return false, &upstream.Error{Kind: kind, Status: status, Msg: string(respBody)}
	}

	if peek.Stream {
		h.cfg.Pool.NoteSuccess(acct.UID)
		// 流内业务错误（1005 plan/5xx 等）→ 冷却账号，错误信息注入 SSE。
		usg, serr := upstream.StreamWithError(w, rc, func(se *upstream.SOLOStreamError) {
			h.handleStreamError(acct.UID, se)
		})
		rc.Close()
		// 流内 error 事件走 onErr 回调后本函数仍返回 nil，所以「有 token_usage」才是这次
		// 尝试真的产出了回复的判据；只看返回 err 会把 1005 记成成功。
		ok := serr == nil && len(usg) > 0
		h.noteUsage(acct.UID, peek.Model, attemptStart, ok, usg)
		if ok {
			h.bindSession(sessKey, acct.UID)
		} else if sessKey != "" && h.cfg.Session != nil {
			h.cfg.Session.Unbind(sessKey)
		}
		return true, nil
	}

	resp, err := upstream.Aggregate(rc)
	rc.Close() // 已完全消费，立即释放上游连接（防轮转 continue 泄漏 body）
	if err != nil {
		var se *upstream.SOLOStreamError
		if errors.As(err, &se) {
			h.noteUsage(acct.UID, peek.Model, attemptStart, false, nil)
			switch se.Kind() {
			case upstream.ErrPlanLimit:
				h.cfg.Pool.CooldownPlan(acct.UID)
			default:
				h.cfg.Pool.NoteError(acct.UID)
			}
			return false, err
		}
		h.noteUsage(acct.UID, peek.Model, attemptStart, false, nil)
		writeOpenAIError(w, http.StatusBadGateway, "upstream_parse", err.Error())
		return true, err
	}
	h.cfg.Pool.NoteSuccess(acct.UID)
	h.noteUsage(acct.UID, peek.Model, attemptStart, true, usageOf(resp))
	h.bindSession(sessKey, acct.UID)
	writeJSON(w, http.StatusOK, resp)
	return true, nil
}

// noteFailure 按错误类型记账：
//   - 1005 权益不足 → 计划硬冷却（plan_credit）；
//   - 429 限流 → 软冷却（指数退避到 soft_rate_max）；
//   - 401 session 失效 → 禁用；
//   - 5xx → 熔断计数；
//   - 其余 4xx（参数/404 等）→ 降权计数，不罚号。
func (h *Handler) noteFailure(uid string, kind upstream.ErrKind) {
	switch kind {
	case upstream.ErrPlanLimit:
		h.cfg.Pool.CooldownPlan(uid)
	case upstream.ErrSoftRate:
		h.cfg.Pool.CooldownSoft(uid, "429 rate limit")
	case upstream.ErrSessionDead:
		h.cfg.Pool.Disable(uid, "session dead")
	case upstream.ErrServer:
		h.cfg.Pool.NoteError(uid)
	default:
		h.cfg.Pool.NoteDegrade(uid)
	}
}

// bindSession 会话粘性绑定：只有真的产出回复才绑，失败继续留在池里轮换。
func (h *Handler) bindSession(sessKey, uid string) {
	if sessKey != "" && h.cfg.Session != nil {
		h.cfg.Session.Bind(sessKey, uid)
	}
}

// handleStreamError 流式响应中的上游业务错误 → pool 状态机。
//
// 1005 → 计划冷却；其余一律按罚号计数：流内错误在上游侧，"不罚号"判定拿不到可靠依据
// （Kind() 只能区分 1005），宁可让它退到熔断而不是当成客户端问题继续用这个号。
func (h *Handler) handleStreamError(uid string, se *upstream.SOLOStreamError) {
	switch se.Kind() {
	case upstream.ErrPlanLimit:
		h.cfg.Pool.CooldownPlan(uid)
	default:
		h.cfg.Pool.NoteError(uid)
	}
}

// noteUsage 把一次出站尝试记进用量台账。
//
// 失败尝试也要记（请求数与失败数）——重试放大正是靠这一列才在面板里看得见；
// 但「刷新 token 失败」不记：那不是一次对话调用，记进去只会让用量虚高。
// ok 以「上游是否给了 usage」为准：流内 error 事件（如 1005）响应体正常写完、
// 函数返回 nil，只有 token_usage 才证明这次真的产出了回复。
func (h *Handler) noteUsage(uid, model string, started time.Time, ok bool, upstreamUsage map[string]any) {
	if h.cfg.Usage == nil {
		return
	}
	// 延迟下界 1ms：本地极快响应算 TPS 时不能除以 0。
	latencyMs := time.Since(started).Milliseconds()
	if latencyMs < 1 {
		latencyMs = 1
	}
	d := usage.Delta{LatencyMs: latencyMs, HasLatency: true}
	if pt, has := numField(upstreamUsage, "prompt_tokens"); has {
		d.PromptTokens, d.HasPromptTokens = pt, true
	}
	if ct, has := numField(upstreamUsage, "completion_tokens"); has {
		d.CompletionTokens, d.HasCompletion = ct, true
		if ct > 0 {
			d.TokensPerSecond, d.HasTPS = float64(ct)*1000/float64(latencyMs), true
		}
	}
	if tt, has := numField(upstreamUsage, "total_tokens"); has {
		d.TotalTokens, d.HasTotal = tt, true
	}
	h.cfg.Usage.Add(time.Now(), uid, model, d, ok)
}

// usageOf 取聚合响应里的 usage（上游没给时为 nil，不伪造）。
func usageOf(resp map[string]any) map[string]any {
	m, _ := resp["usage"].(map[string]any)
	return m
}

// numField 读 usage 里的整数字段：JSON 解出来是 float64，探针/测试里可能是 int。
func numField(m map[string]any, key string) (int64, bool) {
	if m == nil {
		return 0, false
	}
	switch v := m[key].(type) {
	case float64:
		return int64(v), true
	case int:
		return int64(v), true
	case int64:
		return v, true
	case json.Number:
		if n, err := v.Int64(); err == nil {
			return n, true
		}
	}
	return 0, false
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	raw, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

func writeOpenAIError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message": msg,
			"type":    "api_error",
			"code":    code,
		},
	})
}
