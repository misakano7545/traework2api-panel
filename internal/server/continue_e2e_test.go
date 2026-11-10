// continue_e2e_test.go 截断自动续写的端到端口径：假上游 + 真 Handler，断言三个适配器
// （/v1/chat/completions、/v1/responses、/v1/messages）拿到的终态与正文。
//
// 这里跑的是「上游不发 done 就 EOF」这条最容易被伪装成成功的路径：旧行为下 solosse
// 会补 [DONE]、responses 报 response.completed，客户端拿着半句话当完整答复。
package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"traework2api/internal/auth"
	"traework2api/internal/upstream"
)

// soloNoDoneSSE 上游偶发形态：正文 + usage，但没有 done 事件。
const soloNoDoneSSE = "event:output\ndata:{\"response\":\"半句话\",\"tool_calls\":null}\n\n" +
	"event:token_usage\ndata:{\"prompt_tokens\":10,\"completion_tokens\":1200,\"total_tokens\":1210}\n\n"

// soloDoneSSE 正常收尾段（续写/盲重试成功时用）。
const soloDoneSSE = "event:output\ndata:{\"response\":\"继续写完\",\"tool_calls\":null}\n\n" +
	"event:token_usage\ndata:{\"prompt_tokens\":20,\"completion_tokens\":8,\"total_tokens\":28}\n\n" +
	"event:done\ndata:{\"finish_reason\":\"stop\"}\n\n"

// cutUpstream 造一个「第一次对话调用断流、其后按 next 回」的假上游；非对话路径恒回空表
// （mapModel 落到静态表，glm-5.2 可在）。
func cutUpstream(next func(chatCall int) (int, string, bool)) *upstream.Client {
	n := 0
	var calls []string
	return fakeWithPaths(&calls, func(path, _ string) (int, string, bool) {
		if path != upstream.EpChat {
			return 200, `{"user_info":{"config_info_list":[]}}`, false
		}
		n++
		if n == 1 {
			return 200, soloNoDoneSSE, true
		}
		return next(n)
	})
}

func post(h *Handler, path, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", path, strings.NewReader(body)))
	return rec
}

func cutHandler(up *upstream.Client) *Handler {
	return NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
}

// 断流能救回来（盲重试成功）时：客户端只看到一条连续输出、一个终态，且不标截断。
func TestChatCutNoDoneBlindRetrySucceeds(t *testing.T) {
	h := cutHandler(cutUpstream(func(int) (int, string, bool) { return 200, soloDoneSSE, true }))

	rec := post(h, "/v1/chat/completions",
		`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	s := rec.Body.String()
	if !strings.Contains(s, "半句话") || !strings.Contains(s, "继续写完") {
		t.Fatalf("两段正文都应在场: %s", s)
	}
	if strings.Contains(s, `"finish_reason":"length"`) {
		t.Fatalf("救回来了就不该标截断: %s", s)
	}
	if n := strings.Count(s, `"finish_reason":"stop"`); n != 1 {
		t.Fatalf("终态帧数=%d want 1: %s", n, s)
	}
}

// 断流救不回来（盲重试被上游拒）时：/v1/responses 必须如实报 response.incomplete，
// 不能报 completed —— Codex 据此判断输出完整性并决定要不要继续。
func TestResponsesCutNoDoneReportsIncomplete(t *testing.T) {
	h := cutHandler(cutUpstream(func(int) (int, string, bool) {
		return 500, `{"code":4008,"msg":"quota exceeded"}`, false
	}))

	rec := post(h, "/v1/responses", `{"model":"glm-5.2","stream":true,"input":"hi"}`)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	types := sseTypes(rec.Body.String())
	if len(types) == 0 || types[len(types)-1] != "response.incomplete" {
		t.Fatalf("末事件=%v（截断必须如实报 incomplete）", types)
	}
	if containsStr(types, "response.completed") {
		t.Fatalf("截断流不得报 completed: %v", types)
	}
	if !strings.Contains(rec.Body.String(), "半句话") {
		t.Fatalf("已输出的正文应留给客户端: %s", rec.Body.String())
	}
}

// 同上，Anthropic 侧：stop_reason 必须是 max_tokens，不能伪装成 end_turn。
func TestMessagesCutNoDoneReportsMaxTokens(t *testing.T) {
	h := cutHandler(cutUpstream(func(int) (int, string, bool) {
		return 500, `{"code":4008,"msg":"quota exceeded"}`, false
	}))

	rec := post(h, "/v1/messages",
		`{"model":"glm-5.2","max_tokens":32000,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"stop_reason":"max_tokens"`) {
		t.Fatalf("截断应报 stop_reason=max_tokens: %s", body)
	}
	if strings.Contains(body, `"stop_reason":"end_turn"`) {
		t.Fatalf("截断不得伪装成 end_turn: %s", body)
	}
}

// 传输层读错误仍按失败上抛（不许被当成「截断的正常结束」而记成功）。
func TestChatCutTransportErrorStillFails(t *testing.T) {
	// 上游吐完全部数据后报一个非 EOF 读错误（模拟中途断连）。
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if r.URL.Path != upstream.EpChat {
				return &http.Response{
					StatusCode: 200,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(strings.NewReader(`{"user_info":{"config_info_list":[]}}`)),
				}, nil
			}
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(&errAfterReader{data: soloNoDoneSSE}),
			}, nil
		})},
		AgentHost: "https://fake.example",
		UgHost:    "https://fake.example",
		OAuthHost: "https://fake.example",
		ClientID:  upstream.ClientID,
	}
	h := cutHandler(up)
	rec := post(h, "/v1/chat/completions",
		`{"model":"glm-5.2","stream":true,"max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`)
	s := rec.Body.String()
	// 已写出的正文要留下，但整条流不能以「正常 stop」收尾。
	if strings.Contains(s, `"finish_reason":"stop"`) {
		t.Fatalf("传输层失败被伪装成正常收尾: %s", s)
	}
}

// errAfterReader 先吐完全部数据、再返回一个非 EOF 读错误（模拟上游中途断连）。
type errAfterReader struct {
	data string
	sent bool
}

func (e *errAfterReader) Read(p []byte) (int, error) {
	if !e.sent {
		e.sent = true
		return copy(p, e.data), nil
	}
	return 0, io.ErrUnexpectedEOF
}
