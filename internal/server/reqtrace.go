package server

import (
	"context"
	"net"
	"net/http"
	"strings"
	"time"

	"traework2api/internal/reqlog"
)

// 请求级指标追踪：给对话类入站路径套一层中间件，记录时间/结果/模型/账号/来源/耗时/请求 ID。
// 只记元数据——不写提示词、响应正文、Authorization 或其它凭证（与 reqlog 的约定一致）。
// 这是「运行日志」页请求记录的取数来源；归档开关与 client_info 见 config.logging。

type reqTraceKey struct{}

// reqTrace 一次请求的追踪游标。model / account 由业务 handler 回填（中间件拿不到它们）。
type reqTrace struct {
	ID       string
	Start    time.Time
	ClientIP string
	UA       string
	Model    string
	Account  string
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
		Time:       tr.Start,
		RequestID:  tr.ID,
		Path:       r.URL.Path,
		Account:    tr.Account,
		Model:      tr.Model,
		Status:     status,
		OK:         ok,
		Outcome:    outcome,
		DurationMs: time.Since(tr.Start).Milliseconds(),
		ClientIP:   tr.ClientIP,
		UserAgent:  tr.UA,
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
