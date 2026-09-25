package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"traework2api/internal/auth"
	"traework2api/internal/pool"
	"traework2api/internal/session"
	"traework2api/internal/upstream"
	"traework2api/internal/usage"
)

// 模拟 SOLO SSE 响应（glm-5.2 回答"你好"）。
const soloSSE = "event:metadata\ndata:{\"model\":\"\",\"session_id\":\"s1\"}\n\n" +
	"event:output\ndata:{\"response\":\"你好\",\"reasoning_content\":\"想一下\",\"tool_calls\":null}\n\n" +
	"event:token_usage\ndata:{\"prompt_tokens\":5,\"completion_tokens\":2,\"total_tokens\":7}\n\n" +
	"event:done\ndata:{\"finish_reason\":\"stop\"}\n\n"

// newFakeUpstream 返回 ChatStream 走 fake 的 upstream.Client。
func newFakeUpstream(t *testing.T, behavior func(auth string) (status int, body string, isStream bool)) *upstream.Client {
	t.Helper()
	return &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			authz := r.Header.Get("Authorization")
			status, body, isStream := behavior(authz)
			ct := "application/json"
			if isStream {
				ct = "text/event-stream"
			}
			return &http.Response{
				StatusCode: status,
				Header:     http.Header{"Content-Type": []string{ct}},
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		})},
		AgentHost: "https://fake.example",
		UgHost:    "https://fake.example",
		OAuthHost: "https://fake.example",
		ClientID:  upstream.ClientID,
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func testPoolWith(auths ...*auth.Auth) *pool.Pool {
	p := pool.New("")
	for _, a := range auths {
		p.Add(a)
		p.SetCredits(a.UID, 1000)
	}
	return p
}

func TestChatNonStreamAggregates(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		if authz != "Cloud-IDE-JWT at1" {
			t.Errorf("auth=%q", authz)
		}
		return 200, soloSSE, true
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("resp not json: %v body=%s", err, rec.Body)
	}
	if resp["object"] != "chat.completion" {
		t.Errorf("object=%v", resp["object"])
	}
	msg := resp["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "你好" {
		t.Errorf("content=%q", msg["content"])
	}
	if msg["reasoning_content"] != "想一下" {
		t.Errorf("reasoning=%q", msg["reasoning_content"])
	}
}

func TestChatStreamPassthrough(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, soloSSE, true
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Errorf("ct=%q", ct)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "你好") || !strings.Contains(body, "data: [DONE]") {
		t.Errorf("body=%q", body)
	}
}

func TestChatRotatesOnPlanLimit(t *testing.T) {
	calls := map[string]int{}
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls[authz]++
		if authz == "Cloud-IDE-JWT at-bad" {
			return 200, "event:error\ndata:{\"code\":1005,\"message\":\"plan limit\",\"extra\":{\"plan\":2}}\n\n", true
		}
		return 200, soloSSE, true
	})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	p.SetCredits("bad", 2000)
	p.SetCredits("good", 1000)
	h := NewHandler(Config{Pool: p, Upstream: up})
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if calls["Cloud-IDE-JWT at-bad"] != 1 || calls["Cloud-IDE-JWT at-good"] != 1 {
		t.Errorf("calls=%v", calls)
	}
}

// TestChatStreamCooldownOnStreamError 流式请求中上游 event:error（如 5xx/参数错误）
// 应触发 NoteError 冷却（而非仅透传不冷却）。
func TestChatStreamCooldownOnStreamError(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, "event:error\ndata:{\"code\":5001,\"message\":\"upstream broke\"}\n\n", true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	// 流内错误不应中断整个请求的 SSE 输出（仍有 event:error + [DONE]）。
	body := rec.Body.String()
	if !strings.Contains(body, "solo error") || !strings.Contains(body, "data: [DONE]") {
		t.Errorf("body=%q want error event + [DONE]", body)
	}
	st, _ := p.Status("u1")
	if st.ErrCount == 0 {
		t.Errorf("stream error should bump errCount: %+v", st)
	}
}

// 用量台账：成功的调用记 token，失败的尝试只记请求数+失败数（没有 usage 可记）。
func TestUsageRecordsSuccessAndFailure(t *testing.T) {
	rec := usage.New("") // 不落盘
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		if authz == "Cloud-IDE-JWT at-bad" {
			return 429, `rate limited`, false
		}
		return 200, soloSSE, true
	})
	// 坏号积分更高，先被选中：第一次调用撞 429 → 冷却换号，好号成功。失败的那次只
	// 记请求数与失败数（没有 usage 可记），成功的那次记 token。
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	p.SetCredits("bad", 2000)
	p.SetCredits("good", 1000)
	h := NewHandler(Config{Pool: p, Upstream: up, Usage: rec})

	post := func(body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))
		return rec
	}
	if code := post(`{"model":"glm-5.2","messages":[]}`).Code; code != 200 {
		t.Fatalf("非流式应换号后成功: code=%d", code)
	}
	if code := post(`{"model":"glm-5.2","stream":true,"messages":[]}`).Code; code != 200 {
		t.Fatalf("流式 code=%d", code)
	}

	snap := rec.Snapshot(72, map[string]string{"good": "好的号"})
	// 第一次调用记 2 次尝试（1 失败 + 1 成功），第二次记 1 次成功。
	if snap.Totals.Requests != 3 || snap.Totals.Errors != 1 {
		t.Fatalf("请求/失败 = %d/%d，期望 3/1: %+v", snap.Totals.Requests, snap.Totals.Errors, snap.Totals)
	}
	// soloSSE 每帧 usage: prompt 5 / completion 2 / total 7，两次成功共 10/4/14。
	if snap.Totals.PromptTokens != 10 || snap.Totals.CompletionTok != 4 || snap.Totals.TotalTokens != 14 {
		t.Fatalf("token 汇总 = %+v", snap.Totals)
	}
	if len(snap.ByModel) != 1 || snap.ByModel[0].Key != "glm-5.2" || snap.ByModel[0].Requests != 3 {
		t.Fatalf("按模型 = %+v", snap.ByModel)
	}
	if len(snap.ByAccount) != 2 || snap.ByAccount[0].Key != "good" || snap.ByAccount[0].Extra != "好的号" ||
		snap.ByAccount[0].Requests != 2 || snap.ByAccount[0].TotalTokens != 14 {
		t.Fatalf("按账号 = %+v", snap.ByAccount)
	}
	if snap.ByAccount[1].Key != "bad" || snap.ByAccount[1].Errors != 1 || snap.ByAccount[1].TotalTokens != 0 {
		t.Fatalf("失败账号行 = %+v", snap.ByAccount[1])
	}
	if snap.Totals.AvgLatencyMs <= 0 {
		t.Fatalf("延迟应有样本: %+v", snap.Totals)
	}
}

// 流内 1005：响应体正常写完、函数返回 nil，仍须记为失败而不是成功。
func TestUsageMarksStreamPlanLimitAsFailure(t *testing.T) {
	rec := usage.New("")
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, "event:error\ndata:{\"code\":1005,\"message\":\"plan limit\"}\n\n", true
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
		Usage:    rec,
	})
	h.ServeHTTP(httptest.NewRecorder(),
		httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[]}`)))
	snap := rec.Snapshot(72, nil)
	if snap.Totals.Requests != 1 || snap.Totals.Errors != 1 {
		t.Fatalf("流内 1005 应记 1 请求 1 失败: %+v", snap.Totals)
	}
}

func TestChatAllUnavailableReturns503(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 429, `rate limited`, false
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 503 {
		t.Errorf("code=%d body=%s", rec.Code, rec.Body)
	}
	var e map[string]any
	json.Unmarshal(rec.Body.Bytes(), &e)
	if e["error"] == nil {
		t.Errorf("want error envelope: %s", rec.Body)
	}
}

func TestChatSessionDeadNeedsThreeStrikes(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 401, `{"code":1001,"msg":"login required"}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	for i := 1; i <= 3; i++ {
		req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 503 {
			t.Errorf("第 %d 次 code=%d", i, rec.Code)
		}
		st, _ := p.Status("u1")
		if i < 3 && st.Disabled {
			t.Fatalf("第 %d 次 401 就禁用（一次抖动不该杀号）: %+v", i, st)
		}
		if i == 3 && !st.Disabled {
			t.Errorf("连续 3 次 session 失效仍不禁用: %+v", st)
		}
	}
}

func TestChatUnknownModel400(t *testing.T) {
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}), Upstream: upstream.New()})
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"does-not-exist-xyz","messages":[]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 400 {
		t.Errorf("code=%d body=%s", rec.Code, rec.Body)
	}
	var e map[string]any
	json.Unmarshal(rec.Body.Bytes(), &e)
	if e["error"] == nil {
		t.Errorf("want error envelope: %s", rec.Body)
	}
}

func TestModelsEndpoint(t *testing.T) {
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}), Upstream: upstream.New()})
	req := httptest.NewRequest("GET", "/v1/models", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	var resp map[string]any
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["object"] != "list" {
		t.Errorf("object=%v", resp["object"])
	}
	data := resp["data"].([]any)
	if len(data) != 15 {
		t.Errorf("models count=%d want 15（官方模型，非上游全量表）", len(data))
	}
	found := false
	for _, m := range data {
		if m.(map[string]any)["id"] == "glm-5.2" {
			found = true
		}
	}
	if !found {
		t.Error("glm-5.2 missing")
	}
}

func TestAPIKeyAuth(t *testing.T) {
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		Upstream: upstream.New(),
		APIKey:   "test-key",
	})
	// 无 key → 401
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Errorf("no key: code=%d", rec.Code)
	}
	// 错 key → 401
	req = httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer wrong")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Errorf("wrong key: code=%d", rec.Code)
	}
	// 对 key → 200（models）
	req = httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Errorf("right key: code=%d", rec.Code)
	}
}

func TestRequestBodyTooLarge(t *testing.T) {
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}), Upstream: upstream.New()})
	big := strings.Repeat("a", maxBodyBytes+1)
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(big))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("code=%d body=%s", rec.Code, rec.Body)
	}
	var e map[string]any
	json.Unmarshal(rec.Body.Bytes(), &e)
	if e["error"] == nil {
		t.Errorf("want error envelope: %s", rec.Body)
	}
}

func TestAPIKeyAuthCaseInsensitivePrefix(t *testing.T) {
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		Upstream: upstream.New(),
		APIKey:   "test-key",
	})
	// 大小写不同的 Bearer 前缀也应接受（按规范，前缀大小写不敏感）。
	req := httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Set("Authorization", "bearer test-key")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Errorf("lowercase bearer: code=%d", rec.Code)
	}
	// 错误 key 仍拒绝。
	req = httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Errorf("wrong key: code=%d", rec.Code)
	}
}

func TestStatusEndpoint(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", Nickname: "nick", AccessToken: "at", ExpiresAt: 9999999999})
	p.SetCredits("u1", 42)
	h := NewHandler(Config{Pool: p, Upstream: upstream.New()})
	req := httptest.NewRequest("GET", "/status", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"uid":"u1"`) || !strings.Contains(body, `"credits":42`) {
		t.Errorf("body=%s", body)
	}
	if strings.Contains(body, "AccessToken") || strings.Contains(body, `"at"`) {
		t.Error("token leaked in status output")
	}
}

func TestHealthz(t *testing.T) {
	h := NewHandler(Config{Pool: pool.New(""), Upstream: upstream.New()})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != 200 {
		t.Errorf("code=%d", rec.Code)
	}
}

// 会话粘性：同一条会话的请求粘到已绑定的号，即使别的号积分更高。
func TestSessionStickyPinsAccount(t *testing.T) {
	calls := map[string]int{}
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls[authz]++
		return 200, soloSSE, true
	})
	p := testPoolWith(
		&auth.Auth{UID: "a", AccessToken: "at-a", ExpiresAt: 9999999999},
		&auth.Auth{UID: "b", AccessToken: "at-b", ExpiresAt: 9999999999},
	)
	p.SetCredits("a", 100)
	p.SetCredits("b", 9000) // 正常挑号会选 b
	sess := session.New(session.Config{Enabled: true, TTL: time.Minute, GCInterval: time.Minute})
	defer sess.Stop()
	h := NewHandler(Config{Pool: p, Upstream: up, Session: sess})

	body := `{"model":"glm-5.2","conversation_id":"c-1","messages":[{"role":"user","content":"hi"}]}`
	sess.Bind(session.ExtractKey([]byte(body)), "a") // 会话已绑定 a
	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))
		if rec.Code != 200 {
			t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
		}
	}
	if calls["Cloud-IDE-JWT at-a"] != 2 || calls["Cloud-IDE-JWT at-b"] != 0 {
		t.Fatalf("会话应粘在 a: %v", calls)
	}

	// 无会话键的请求走正常挑号（积分最高的 b）。
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))
	if calls["Cloud-IDE-JWT at-b"] != 1 {
		t.Fatalf("无会话键应选 b: %v", calls)
	}
}

// 在途名额不泄漏：无论成败，尝试结束后账号都能继续被选中（MaxInFlight=1 时最容易暴露）。
func TestAttemptReleasesInFlight(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		if authz == "Cloud-IDE-JWT at1" {
			return 429, `rate limited`, false
		}
		return 200, soloSSE, true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	p.ApplyLimits(pool.Limits{MaxInFlight: 1, SoftCooldown: time.Millisecond, SoftCooldownMax: time.Millisecond})
	h := NewHandler(Config{Pool: p, Upstream: up, Usage: usage.New("")})

	for i := 0; i < 3; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
			strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
		time.Sleep(2 * time.Millisecond) // 等软冷却过期
	}
	if st, _ := p.Status("u1"); st.InFlight != 0 {
		t.Fatalf("在途名额泄漏: %+v", st)
	}
}
