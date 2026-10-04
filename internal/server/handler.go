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
	"traework2api/internal/reqlog"
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
	// RequestLog 可选，请求级指标/归档（nil = 不记录）。RequestClientInfo 决定事件里
	// 是否填调用来源（IP/UA）——比 token 计数敏感，运营可自行决定是否落盘。
	RequestLog        *reqlog.Recorder
	RequestClientInfo bool
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
	h.mux.HandleFunc("POST /v1/responses", h.withAuth(h.responses))
	h.mux.HandleFunc("POST /v1/messages", h.withAnthropicAuth(h.messages))
	h.mux.HandleFunc("POST /messages", h.withAnthropicAuth(h.messages))
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

// authOK 校验 Bearer 密钥；未设密钥（want 空）= 放行（仅本机）。
// 各入站协议的 withAuth 包装只负责把失败写成自己协议形状的错误体，比较逻辑只此一份。
func (h *Handler) authOK(r *http.Request) bool {
	// 密钥随配置热改（面板改了立刻用新的），所以每次都从 runtime 读，不缓存到局部闭包。
	want := h.runtime().APIKey
	if want == "" {
		return true
	}
	authz := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(authz) < len(prefix) || !strings.EqualFold(authz[:len(prefix)], prefix) {
		return false
	}
	// 常量时间比较，防时序攻击（本地代理但按规范）。
	return subtle.ConstantTimeCompare([]byte(authz[len(prefix):]), []byte(want)) == 1
}

func (h *Handler) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !h.authOK(r) {
			writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "missing or invalid API key")
			return
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
func (h *Handler) mapModel(model, realm string) (string, error) {
	model = strings.TrimSpace(model)
	if model == "" || model == "auto" {
		// 网页端的「TraeWork Auto Model」= 各地区各自的旗舰：国际账号上没有 glm-5.2，
		// 反之国内账号上没有 gpt-5.2。国内版保留面板里可配的默认模型。
		if realm == auth.RealmIntl {
			return upstream.DefaultConfigNameIntl, nil
		}
		return h.runtime().DefaultModel, nil
	}
	// 去掉内部名后缀（__dev / __max 等）
	base := model
	if i := strings.Index(model, "__"); i >= 0 {
		base = model[:i]
	}
	if canon, ok := h.resolveKnownModel(base); ok {
		return canon, nil
	}
	// 宽松匹配：下划线 → 横线，大小写不敏感（deepseek_v4_pro → DeepSeek-V4-Pro）
	norm := normalizeModelName(base)
	if canon, ok := h.resolveKnownModel(norm); ok {
		return canon, nil
	}
	// 上游表里没有：查客户端别名（claude-* / gpt-4o …），照社区实现的别名表做法。
	if alias, ok := aliasModel(realm, norm); ok {
		return alias, nil
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

// resolveKnownModel 在模型表里按「大小写不敏感」找这个裸名，命中就返回**表里的规范 id**。
// 列表 id 带 realm 前缀（见 modelList），剥掉前缀再比：客户端带不带前缀都认。
// 客户端抄的是网页端显示名（GPT-5.4 / MiniMax-M2.7 / GLM-5.2），大小写跟表里不一样；
// 出站必须用表里的规范名，否则上游回 4001。
func (h *Handler) resolveKnownModel(model string) (string, bool) {
	for _, m := range h.modelList() {
		id, _ := m["id"].(string)
		if _, bare := resolveModel(id); strings.EqualFold(bare, model) {
			return bare, true
		}
	}
	return "", false
}

// 静态官方模型表（上游拉取失败时的回退快照）。
// 只列账号可选的官方模型：与 upstream.pickOfficialModels 同一口径，
// 不含 is_invisible_to_user 的内部子代理、custom_model_* 槽位与 summary。
// context_length 取上游 context_window_tokens.dev。
// 快照口径 = 版本码 20260811（k3 已放量）；上游的可见集合会随版本码/灰度漂移，
// 动态拉取始终是权威来源，这里只是拉不到时的兜底。
var staticModels = []map[string]any{
	{"id": "Doubao-Seed-Evolving", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 256000},
	{"id": "Doubao-Seed-2.1-Pro", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 256000},
	{"id": "Doubao-Seed-2.1-Turbo", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 256000},
	{"id": "step-5-preview", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 200000},
	{"id": "glm-5.3", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 200000},
	{"id": "glm-5.2", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 200000},
	{"id": "deepseek-v4.1-flash", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 200000},
	{"id": "DeepSeek-V4-Flash-Official", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 200000},
	{"id": "DeepSeek-V4-Pro-Official", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 200000},
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
	ids []upstream.ModelInfo
	// realmByModel 模型 → 归属地区（国际版模型国内号接不了，选号要用它过滤）。
	realmByModel map[string]string
	// pickOnlyByModel 批量视图独有（聊天配置表里没有）的模型：出站要去掉 config_name，
	// 否则上游 4001。见 upstream.ModelInfo.PickOnly 的实测说明。
	pickOnlyByModel map[string]bool
	// fnByModel 模型 → 所属通道（solo_work_lite / solo_coder）：出站 function 跟着模型走。
	fnByModel map[string]string
	fetched   time.Time
	lastFail  time.Time
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
			// realm 前缀是网关路由协议（见 resolve_model.go）：两侧都带前缀，
			// 客户端把 id 原样回传就带上了地区，不用猜。
			realm := h.modelRealm(mi.ID)
			if realm == "" {
				realm = auth.RealmCN
			}
			entry := map[string]any{
				"id":             realm + ":" + mi.ID,
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
			// 面板模型页要用的字段：展示名 / 最大输出 / 所属通道（仅面板消费，网关客户端忽略）。
			if mi.Name != "" {
				entry["name"] = mi.Name
			}
			if mi.MaxTokens > 0 {
				entry["max_output_tokens"] = mi.MaxTokens
			}
			if mi.Function != "" {
				entry["function"] = mi.Function
			}
			// 客户端选择器视图带来的权威元数据（面板消费）：声明上下文双口径、积分倍率、
			// 多模态、以及「客户端已隐藏但本仓仍放行」的上代标记。
			if mi.ContextMax > 0 {
				entry["context_length_max"] = mi.ContextMax
			}
			if mi.Rate != nil {
				entry["credit_rate"] = *mi.Rate
			}
			if mi.SupportsImage != nil {
				entry["supports_image"] = *mi.SupportsImage
			}
			if mi.Legacy {
				entry["legacy"] = true
				entry["legacy_reason"] = mi.LegacyWhy
			}
			out = append(out, entry)
		}
		return out
	}
	return staticModels
}

// fetchDynamicModels 拉模型列表（get_detail_param），缓存 1h。
// 池里出现过的每个地区（realm）各拉一份并合并：国内版与国际版的模型表不是同一张，
// 只拉一边会让另一边的模型在 /v1/models 与 mapModel 校验里凭空消失。
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

	var merged []upstream.ModelInfo
	realmByModel := map[string]string{}
	fnByModel := map[string]string{}
	pickOnlyByModel := map[string]bool{}
	seen := map[string]bool{}
	for _, acct := range h.realmReps() {
		realm := acct.Realm()
		// 只读探询不占在途名额：这里若带租约，几次列模型就会把账号占到不可用。
		infos, err := h.cfg.Upstream.FetchModels(acct)
		if err != nil {
			continue
		}
		for _, mi := range infos {
			// 两个地区同名的（如 kimi-k3）只留先到的那个：名字一样，调用哪个号都能出。
			if seen[mi.ID] {
				continue
			}
			seen[mi.ID] = true
			merged = append(merged, mi)
			realmByModel[mi.ID] = realm
			fnByModel[mi.ID] = mi.Function
			if mi.PickOnly {
				pickOnlyByModel[mi.ID] = true
			}
		}
	}
	if len(merged) == 0 {
		dynamicModelsCache.Lock()
		dynamicModelsCache.lastFail = time.Now()
		dynamicModelsCache.Unlock()
		return nil
	}
	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = merged
	dynamicModelsCache.realmByModel = realmByModel
	dynamicModelsCache.fnByModel = fnByModel
	dynamicModelsCache.pickOnlyByModel = pickOnlyByModel
	dynamicModelsCache.fetched = time.Now()
	dynamicModelsCache.lastFail = time.Time{}
	dynamicModelsCache.Unlock()
	return merged
}

// modelRealm 该模型归属的地区；未知返回空（= 不限制选号，交给上游判）。
// modelPickOnly 该模型是不是「只在客户端批量视图里可见」（聊天配置表没有它）。
// 是的话出站必须去掉 config_name：传了上游回 4001。
func (h *Handler) modelPickOnly(model string) bool {
	dynamicModelsCache.RLock()
	defer dynamicModelsCache.RUnlock()
	return dynamicModelsCache.pickOnlyByModel[model]
}

func (h *Handler) modelRealm(model string) string {
	dynamicModelsCache.RLock()
	defer dynamicModelsCache.RUnlock()
	return dynamicModelsCache.realmByModel[model]
}

// modelFunction 该模型属于哪条出站通道（solo_work_lite / solo_coder）；未知返回空（用默认）。
func (h *Handler) modelFunction(model string) string {
	dynamicModelsCache.RLock()
	defer dynamicModelsCache.RUnlock()
	return dynamicModelsCache.fnByModel[model]
}

// setFunctionInBody 把出站 function 换成该模型所属通道（PrepareBody 不覆盖显式 function）。
func setFunctionInBody(body []byte, fn string) []byte {
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	obj["function"] = fn
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}

// realmReps 池里每个地区挑一个账号代表去拉模型表。
// ponytail: 挑到谁不定（List 顺序来自 map），也不挑健康度——拉模型表只用 token，
// 冷却中的号照样能拉；真要按健康度挑再加 pool 的按 realm 选号。
func (h *Handler) realmReps() []*auth.Auth {
	seen := map[string]bool{}
	var out []*auth.Auth
	for _, st := range h.cfg.Pool.List() {
		a := h.cfg.Pool.AuthByUID(st.UID)
		if a == nil || seen[a.Realm()] {
			continue
		}
		seen[a.Realm()] = true
		out = append(out, a)
	}
	return out
}

// ---------------------------------------------------------------------------
// chat
// ---------------------------------------------------------------------------

// setModelInBody 出站 model/config_name 一起在这里定（PrepareBody 不再补默认值，见那边注释）。
//
// pickOnly=true 表示该模型只在客户端批量视图里可见、账号聊天配置表里没有它：**必须不带
// config_name**——实测传 config_name=<自己> 上游回 4001「param is invalid」，不传正常出正文
// （2026-10-04，glm-5.3-flash / qwen3.8-flash / kimi-k2.8-preview / glm-5.3-flashx）。
// 聊天配置表里有的模型（glm-5.2 等）仍照旧带 config_name=model，不动现有报文形状。
func setModelInBody(body []byte, configName string, pickOnly bool) []byte {
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	obj["model"] = configName
	if pickOnly {
		delete(obj, "config_name")
	} else if _, has := obj["config_name"]; !has {
		obj["config_name"] = configName
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}

// setSlotInBody 把 body 换成「槽位 + 具体模型」：上游拿 config_name 找槽位、
// 拿 model 找槽位里的具体模型（PrepareBody 不再覆盖显式 config_name）。
func setSlotInBody(body []byte, r upstream.SlotRoute) []byte {
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	obj["config_name"] = r.Slot
	obj["model"] = r.Model
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

	// 地区前缀（[realm:]model）在这里剥掉：后面 mapModel / 出站 body / 台账都用裸名。
	realm, bareModel := resolveModel(peek.Model)
	if tr := traceFrom(r); tr != nil {
		tr.Model = bareModel
	}
	prefixed := bareModel != peek.Model
	if !prefixed {
		// 裸名：按模型表里该名字的归属地区兜底（表里没有 = 国内版，老客户端零回归）。
		if r := h.modelRealm(strings.TrimSpace(bareModel)); r != "" {
			realm = r
		}
	}

	// 会话键必须在改写提示词之前取：改写会动 messages，之后取会让键漂移。
	sessKey := session.ExtractKey(body)

	configName, err := h.mapModel(bareModel, realm)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	// 裸名经别名落到别的地区（如 Gemini-3.1-Pro-Preview → gemini-3.1-pro）：地区以模型为准，
	// 否则会拿国内号去接国际模型。带前缀的请求_prefix 权威，不动。
	if !prefixed {
		if r := h.modelRealm(configName); r != "" {
			realm = r
		}
	}

	body = setModelInBody(body, configName, h.modelPickOnly(configName))
	body = h.applyPrompt(body)
	// 国际版：没开档位的模型走租户槽位（config_name=槽位 + model=具体模型）。
	if route, ok := upstream.IntlSlotRoute(configName); ok && realm == auth.RealmIntl {
		body = setSlotInBody(body, route)
	}
	// 出站通道跟着模型走：网页端那批（gemini-3.1-pro / minimax-m2.7 / kimi-k2.5 …）在
	// solo_coder 通道，发在 Work 通道会被判 4001。
	if fn := h.modelFunction(configName); fn != "" {
		body = setFunctionInBody(body, fn)
	}

	tried := map[string]bool{}
	var lastErr error
	for i := 0; i < h.cfg.MaxRotate; i++ {
		// 按地区选号：国际版模型只有国际号能做，反之亦然。
		acct, release := h.acquireAccount(tried, sessKey, realm)
		if acct == nil {
			break
		}
		tried[acct.UID] = true
		if tr := traceFrom(r); tr != nil && tr.Account == "" {
			tr.Account = accountLabel(acct.UID, acct.Nickname)
		}
		done, err := h.attempt(w, acct, body, peek, sessKey, traceFrom(r))
		release()
		if done {
			return
		}
		if err != nil {
			lastErr = err
		}
	}
	// 上游明确说是「这个模型/通道不行」（4001 参数、4011 通道限流…，Kind()==ErrNone）时，
	// 别再报「账号都不可用」——号是好的，是请求本身被上游拒了。照实回上游的原话。
	// 实测 2026-09-28：coder 通道 4011 期间，同一账号的 Work 通道照常出正文。
	if lastErr != nil {
		var se *upstream.SOLOStreamError
		if errors.As(lastErr, &se) && se.Kind() == upstream.ErrNone {
			writeOpenAIError(w, http.StatusTooManyRequests, "upstream_rejected", se.Error())
			return
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
func (h *Handler) acquireAccount(tried map[string]bool, sessKey, realm string) (*auth.Auth, func()) {
	if sessKey != "" && h.cfg.Session != nil {
		if uid, ok := h.cfg.Session.Resolve(sessKey); ok && !tried[uid] {
			if a, ok := h.cfg.Pool.AcquireIfHealthy(uid); ok {
				if realm == "" || a.Realm() == realm {
					return a, func() { h.cfg.Pool.Release(uid) }
				}
				// 粘住的号地区不符（换模型/换地区了）→ 还回在途名额再解绑，别白占着。
				h.cfg.Pool.Release(uid)
			}
			// 粘住的号不可用（冷却/禁用/在途满/地区不符）→ 解绑，本轮重新分配。
			h.cfg.Session.Unbind(sessKey)
		}
	}
	a := h.cfg.Pool.PickRealmExcluding(realm, tried)
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
func (h *Handler) attempt(w http.ResponseWriter, acct *auth.Auth, body []byte, peek chatRequest, sessKey string, tr *reqTrace) (bool, error) {
	refreshed, err := h.cfg.Upstream.RefreshTokenIfNeeded(acct, h.cfg.RefreshSkew)
	if err != nil {
		var ue *upstream.Error
		if errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead {
			// 一次 401 不杀号：连续 NoteSessionDead 达阈值才禁用（阈值见 pool）。
			h.cfg.Pool.NoteSessionDead(acct.UID)
		} else {
			// token 刷新失败不是一次对话调用，不计用量；但账号本身有问题，按罚号记一次。
			h.cfg.Pool.NoteError(acct.UID)
		}
		return false, err
	}
	if refreshed {
		_ = acct.SaveAtomic()
		// 刷新成功 = 号还活着，清连续失效计数（没有真刷新就不清，否则每请求清零，
		// 连续计数永远到不了禁用阈值）。
		h.cfg.Pool.ClearSessionDead(acct.UID)
	}

	attemptStart := time.Now()
	rc, status, respBody, terr := h.cfg.Upstream.ChatStream(acct, body)
	if terr != nil {
		h.noteUsage(acct.UID, peek.Model, attemptStart, false, nil, tr)
		h.cfg.Pool.NoteError(acct.UID)
		return false, terr
	}
	if status >= 400 {
		kind := upstream.Classify(status, string(respBody))
		h.noteUsage(acct.UID, peek.Model, attemptStart, false, nil, tr)
		h.noteFailure(acct.UID, kind)
		return false, &upstream.Error{Kind: kind, Status: status, Msg: string(respBody)}
	}

	if peek.Stream {
		h.cfg.Pool.NoteSuccess(acct.UID)
		// 截断自动续写（见 upstream/continue.go）：SOLO 32000 硬截断（done 仍报 stop）
		// 时同账号同模型补发「已输出内容+续写指令」，多段拼成一条客户端可见流；
		// 续写失败/显式限额/工具分片 → 自动降级为旧行为（截断终态）。
		cont := upstream.NewContinueReader(h.cfg.Upstream, acct, body, rc)
		// 流内业务错误（1005 plan/5xx 等）→ 冷却账号，错误信息注入 SSE。
		usg, serr := upstream.StreamWithError(w, cont, func(se *upstream.SOLOStreamError) {
			h.handleStreamError(acct.UID, se)
		})
		cont.Close()
		rc.Close()
		// 空流（上游 200 但零模型事件）：这次尝试一个字节都没写给客户端，
		// 换号重试比把空回复当成功更合适（照 Trae2api-cn 的「首事件前空响应可重试」）。
		// 注意不能算 NoteError：空转不是账号故障，罚它会白白把好号熔断掉。
		if errors.Is(serr, upstream.ErrEmptyStream) {
			h.noteUsage(acct.UID, peek.Model, attemptStart, false, nil, tr)
			return false, serr
		}
		// 流内 error 事件走 onErr 回调后本函数仍返回 nil，所以「有 token_usage」才是这次
		// 尝试真的产出了回复的判据；只看返回 err 会把 1005 记成成功。
		ok := serr == nil && len(usg) > 0
		h.noteUsage(acct.UID, peek.Model, attemptStart, ok, usg, tr)
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
			// 流内错误与 HTTP 级错误走同一张分类表（noteFailure），避免两处口径漂移。
			h.noteUsage(acct.UID, peek.Model, attemptStart, false, nil, tr)
			h.noteFailure(acct.UID, se.Kind())
			return false, err
		}
		if errors.Is(err, upstream.ErrEmptyStream) {
			// 同上：空转可换号重试，但不算账号故障（不 NoteError）。
			h.noteUsage(acct.UID, peek.Model, attemptStart, false, nil, tr)
			return false, err
		}
		h.noteUsage(acct.UID, peek.Model, attemptStart, false, nil, tr)
		writeOpenAIError(w, http.StatusBadGateway, "upstream_parse", err.Error())
		return true, err
	}
	h.cfg.Pool.NoteSuccess(acct.UID)
	h.noteUsage(acct.UID, peek.Model, attemptStart, true, usageOf(resp), tr)
	h.bindSession(sessKey, acct.UID)
	writeJSON(w, http.StatusOK, resp)
	return true, nil
}

// noteFailure 按错误类型记账（HTTP 级与流内错误共用这一张表）：
//   - 1005 权益不足 → 计划硬冷却（plan_credit）；
//   - 429/4008 限流 → 软冷却（指数退避到 soft_rate_max）；
//   - 401 session 失效 → 连续达阈值才禁用；
//   - 5xx → 熔断计数；
//   - 其余 4xx（参数/404 等）→ 降权计数，不罚号；
//   - ErrNone（模型/参数问题，如 4001）→ 不记账：与账号无关，写进去只会污染状态。
func (h *Handler) noteFailure(uid string, kind upstream.ErrKind) {
	switch kind {
	case upstream.ErrNone:
		// no-op
	case upstream.ErrPlanLimit:
		h.cfg.Pool.CooldownPlan(uid)
	case upstream.ErrSoftRate:
		h.cfg.Pool.CooldownSoft(uid, "429 rate limit")
	case upstream.ErrSessionDead:
		h.cfg.Pool.NoteSessionDead(uid)
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
// 与 HTTP 级错误同一张分类表：Kind() 现在能区分「模型/参数问题」（ErrNone，不记账）
// 与「限流/会话失效/服务端」，所以不再需要"一律按罚号计数"的粗口径。
func (h *Handler) handleStreamError(uid string, se *upstream.SOLOStreamError) {
	h.noteFailure(uid, se.Kind())
}

// noteUsage 把一次出站尝试记进用量台账。
//
// 失败尝试也要记（请求数与失败数）——重试放大正是靠这一列才在面板里看得见；
// 但「刷新 token 失败」不记：那不是一次对话调用，记进去只会让用量虚高。
// ok 以「上游是否给了 usage」为准：流内 error 事件（如 1005）响应体正常写完、
// 函数返回 nil，只有 token_usage 才证明这次真的产出了回复。
func (h *Handler) noteUsage(uid, model string, started time.Time, ok bool, upstreamUsage map[string]any, tr *reqTrace) {
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
	// 缓存命中：SOLO 的 token_usage 用 cache_read_input_tokens 报命中量。
	// 上游没报这个键就是没有缓存信息（不是 0% 命中），HasCacheHit 保持 false。
	if ch, has := numField(upstreamUsage, "cache_read_input_tokens"); has {
		d.CacheHitTokens, d.HasCacheHit = ch, true
	}
	// 请求记录（面板「运行日志」页的请求表）与用量台账吃同一个 Delta：token/积分/缓存命中
	// 在这里一次性带上，避免两处各算一遍再对不上。失败尝试同样带 token（上游有时在错误
	// 响应里也回了 usage），积分则由单价估算——和台账的「积分≈」是同一套口径。
	//
	// 先喂请求记录、再写台账：台账 recorder 为 nil（未接用量）时请求记录仍要有 token，
	// 否则这两件事被隐式绑在一起——「没用量台账」会顺带让运行日志的请求行也变成 tok=-。
	tr.addUsage(model, d)
	if h.cfg.Usage == nil {
		return
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
