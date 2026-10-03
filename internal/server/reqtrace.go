package server

import (
	"context"
	"net"
	"net/http"
	"strings"
	"time"

	"traework2api/internal/reqlog"
	"traework2api/internal/usage"
)

// 请求级指标追踪：给对话类入站路径套一层中间件，记录时间/结果/模型/账号/来源/耗时/请求 ID。
// 只记元数据——不写提示词、响应正文、Authorization 或其它凭证（与 reqlog 的约定一致）。
// 这是「运行日志」页请求记录的取数来源；归档开关与 client_info 见 config.logging。

type reqTraceKey struct{}

// reqTrace 一次请求的追踪游标。model / account 由业务 handler 回填（中间件拿不到它们）；
// 用量字段由 noteUsage 在同一请求上下文里累加（一次请求可能换号重试多次）。
type reqTrace struct {
	ID       string
	Start    time.Time
	ClientIP string
	UA       string
	Model    string
	Account  string

	// 以下由 addUsage 累加：多账号重试时 token 相加，积分按单价估算（与用量台账同一套
	// pricing，见 internal/usage）。上游没报的维度保持零值 —— 零值在 Event 上被 omitempty
	// 省略，面板显示「—」而不是 0。
	PromptTokens     int64
	CompletionTokens int64
	TotalTokens      int64
	CacheHitTokens   int64
	Credit           float64
	HasCredit        bool
}

// addUsage 把一次出站尝试的用量吸收进本请求的追踪游标。
//
// 只吸收上游真报了的维度（Has* 置位）：这些字段在面板上区分「没观测到」与「就是 0」，
// 无脑累加会把没上报的请求显示成 0 token。积分用 usage.EstimateCredit 估算（上游不报
// 单请求积分），没收录单价的模型不计分——面板据此显示「—」而不是编一个数。
func (t *reqTrace) addUsage(model string, d usage.Delta) {
	if t == nil {
		return
	}
	if d.HasPromptTokens {
		t.PromptTokens += d.PromptTokens
	}
	if d.HasCompletion {
		t.CompletionTokens += d.CompletionTokens
	}
	if d.HasTotal {
		t.TotalTokens += d.TotalTokens
	}
	if d.HasCacheHit {
		t.CacheHitTokens += d.CacheHitTokens
	}
	// 零 token 不标「已计价」：EstimateCredit 对 0/0/0 会正常返回 0，标了就等于在面板上
	// 声称「这次扣了 0 积分」，而实际是这次没有可计价的产出（失败尝试大多如此）。
	if d.PromptTokens+d.CompletionTokens > 0 {
		if v, ok := usage.EstimateCredit(model, d.PromptTokens, d.CompletionTokens, d.CacheHitTokens); ok {
			t.Credit += v
			t.HasCredit = true
		}
	}
}

// cacheMissTokens 缓存未命中量 = prompt − 命中（上游只报命中量，未命中量由此推导；
// 两者都缺失时不猜，保持 0 = 面板不显示命中率）。
func (t *reqTrace) cacheMissTokens() int64 {
	if t.PromptTokens <= 0 {
		return 0
	}
	miss := t.PromptTokens - t.CacheHitTokens
	if miss < 0 {
		return 0
	}
	return miss
}

func traceFrom(r *http.Request) *reqTrace {
	t, _ := r.Context().Value(reqTraceKey{}).(*reqTrace)
	return t
}

// tracedPath 只追对话类入站；/v1/models、/status、/panel/ 不产生请求记录。
func tracedPath(p string) bool {
	switch p {
	case "/v1/chat/completions", "/v1/responses", "/v1/messages", "/messages":
		return true
	}
	return false
}

// clientIPForLog 取调用来源 IP：X-Forwarded-For 首段，无代理头时回落 TCP 对端。
func clientIPForLog(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i >= 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// accountLabel 请求记录里的账号显示名：昵称(uid 前 8 位)，与面板账号表/用量台账同一口径。
// 只写裸 uid 在表里扫不动——同一批账号的 uid 前缀都一样，肉眼分不出是哪个号。
func accountLabel(uid, nick string) string {
	uid, nick = strings.TrimSpace(uid), strings.TrimSpace(nick)
	if nick == "" {
		return uid
	}
	if len(uid) > 8 {
		uid = uid[:8]
	}
	return nick + "(" + uid + ")"
}

// traceWriter 只捕获响应状态码：写头/写体任一先到都立刻定格。
// 必须实现 Flush，否则内层 SSE 写手的 `w.(http.Flusher)` 断言会失败，流式响应会被静默降级成缓冲。
type traceWriter struct {
	http.ResponseWriter
	status int
}

func (t *traceWriter) WriteHeader(code int) {
	if t.status == 0 {
		t.status = code
	}
	t.ResponseWriter.WriteHeader(code)
}

func (t *traceWriter) Write(b []byte) (int, error) {
	if t.status == 0 {
		t.status = 200
	}
	return t.ResponseWriter.Write(b)
}

func (t *traceWriter) Flush() {
	if f, ok := t.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap 让 http.ResponseController 能穿透这层包装（Go 1.20+）。
func (t *traceWriter) Unwrap() http.ResponseWriter { return t.ResponseWriter }

// ServeHTTP 在有记录器时给对话路径套追踪，其余路径原样透传。
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rec, clientInfo := h.cfg.RequestLog, h.traceClientInfo()
	if rec == nil || !tracedPath(r.URL.Path) {
		h.mux.ServeHTTP(w, r)
		return
	}
	tr := &reqTrace{ID: reqlog.NewRequestID(), Start: time.Now()}
	if clientInfo {
		tr.ClientIP = clientIPForLog(r)
		tr.UA = r.Header.Get("User-Agent")
	}
	rec.Begin()
	tw := &traceWriter{ResponseWriter: w}
	h.mux.ServeHTTP(tw, r.WithContext(context.WithValue(r.Context(), reqTraceKey{}, tr)))
	status := tw.status
	if status == 0 {
		status = 200 // 处理器什么都没写也已在 WriteHeader 前定格；此处兜底
	}
	outcome := reqlog.OutcomeSuccess
	ok := status >= 200 && status < 300
	if !ok {
		outcome = reqlog.OutcomeHTTPError
	}
	rec.Record(reqlog.Event{
		Time:             tr.Start,
		RequestID:        tr.ID,
		Path:             r.URL.Path,
		Account:          tr.Account,
		Model:            tr.Model,
		Status:           status,
		OK:               ok,
		Outcome:          outcome,
		DurationMs:       time.Since(tr.Start).Milliseconds(),
		PromptTokens:     tr.PromptTokens,
		CompletionTokens: tr.CompletionTokens,
		TotalTokens:      tr.TotalTokens,
		Credit:           tr.Credit,
		HasCredit:        tr.HasCredit,
		CacheHitTokens:   tr.CacheHitTokens,
		CacheMissTokens:  tr.cacheMissTokens(),
		ClientIP:         tr.ClientIP,
		UserAgent:        tr.UA,
	})
}

// traceClientInfo 读「记录调用来源」开关（热改，每次请求现读）。
func (h *Handler) traceClientInfo() bool {
	h.rtMu.RLock()
	defer h.rtMu.RUnlock()
	return h.cfg.RequestClientInfo
}

// SetRequestClientInfo 热改来源记录开关（面板保存 logging.request_client_info 时调用）。
func (h *Handler) SetRequestClientInfo(on bool) {
	h.rtMu.Lock()
	h.cfg.RequestClientInfo = on
	h.rtMu.Unlock()
}
