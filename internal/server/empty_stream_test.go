package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"traework2api/internal/auth"
	"traework2api/internal/upstream"
)

// fakeWithPaths 与 newFakeUpstream 同形，但把「路径｜Authorization」记下来：
// 模型表探测（get_detail_param）与对话（llm_utils_chat）打的是同一个假上游，
// 不按路径区分就会把模型探测也当成轮转次数。
func fakeWithPaths(calls *[]string, behavior func(path, authz string) (int, string, bool)) *upstream.Client {
	return &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			authz := r.Header.Get("Authorization")
			*calls = append(*calls, r.URL.Path+"|"+authz)
			status, body, isStream := behavior(r.URL.Path, authz)
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

// chatCalls 只留对话请求，用于断言轮转次序。
func chatCalls(calls []string) []string {
	var out []string
	for _, c := range calls {
		if strings.HasPrefix(c, upstream.EpChat+"|") {
			out = append(out, strings.TrimPrefix(c, upstream.EpChat+"|"))
		}
	}
	return out
}

// TestChatRotatesOnEmptyStream 上游 200 但零模型事件（空流）时：不许把空回复当成功
// 交给客户端，而要换号重试。这是「上游偶尔空转一次」的恢复路径
// （照 autumnsentiment/Trae2api-cn 的首事件前空响应可重试）。
func TestChatRotatesOnEmptyStream(t *testing.T) {
	var calls []string
	up := fakeWithPaths(&calls, func(path, authz string) (int, string, bool) {
		if path != upstream.EpChat {
			return 200, `{"user_info":{"config_info_list":[]}}`, false
		}
		if authz == "Cloud-IDE-JWT at-bad" {
			// 只发非模型事件：等价于「上游空转了一次」。
			return 200, "event: metadata\ndata:{\"model\":\"\"}\n\n", true
		}
		return 200, soloSSE, true
	})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	p.SetCredits("bad", 2000)
	p.SetCredits("good", 1000)
	p.SetRandInt64N(func(int64) int64 { return 0 }) // 钉死先选积分高的 bad

	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`)))

	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "你好") {
		t.Fatalf("空转后应换号拿到正文，实得: %q", rec.Body.String())
	}
	got := chatCalls(calls)
	if len(got) != 2 || got[0] != "Cloud-IDE-JWT at-bad" || got[1] != "Cloud-IDE-JWT at-good" {
		t.Fatalf("应先在 bad 上空转、再换 good 一次，chat calls=%v", got)
	}
	// 空转不是账号故障：不能把号误判成坏号（否则空转几次就把池子熔断空了）。
	for _, st := range p.List() {
		if st.ErrCount != 0 {
			t.Fatalf("%s 被误记熔断计数 %d", st.UID, st.ErrCount)
		}
	}
}

// TestChatAllEmptyReturns502 全部账号都空转时：给客户端一个明确的 JSON 错误，
// 而不是一个 content 为空的 200（后者客户端看起来像「模型回了空话」）。
func TestChatAllEmptyReturns502(t *testing.T) {
	var calls []string
	up := fakeWithPaths(&calls, func(path, _ string) (int, string, bool) {
		if path != upstream.EpChat {
			return 200, `{"user_info":{"config_info_list":[]}}`, false
		}
		return 200, "event: metadata\ndata:{\"model\":\"\"}\n\n", true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[]}`)))

	if rec.Code == 200 {
		t.Fatalf("全空转不该回 200 空流: %s", rec.Body)
	}
	// 关键：一个字节都没写过，所以还能回干净的 JSON 错误（不能是 SSE 头 + JSON 体）。
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("全空转时应回 JSON 错误，实得 Content-Type=%q body=%q", ct, rec.Body)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("错误体不是 JSON: %v %s", err, rec.Body)
	}
	if _, ok := body["error"]; !ok {
		t.Fatalf("错误体缺 error 字段: %s", rec.Body)
	}
}

// TestChatNonStreamEmptyReturnsError 非流式同一口径。
func TestChatNonStreamEmptyReturnsError(t *testing.T) {
	var calls []string
	up := fakeWithPaths(&calls, func(path, _ string) (int, string, bool) {
		if path != upstream.EpChat {
			return 200, `{"user_info":{"config_info_list":[]}}`, false
		}
		return 200, "event: done\ndata:{\"finish_reason\":\"stop\"}\n\n", true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code == 200 {
		t.Fatalf("全空转不该回 200: %s", rec.Body)
	}
}
