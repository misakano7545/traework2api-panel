// stream_resilience_test.go 断流四条防线的端到端口径（假上游 + 真 Handler）：
//  1. 首帧心跳把「等上游预填充」写成客户端的可见进度，不再让客户端 3~4s 自己断连；
//  2. 心跳之后空流/首帧错误仍能换号（注释帧不算内容已开始）；
//  3. 换号全部失败时，已开口的流只能以协议内 error 帧收尾，不能追加裸 JSON；
//  4. 非流式（stream:false）也接续写器：撞 cap / 无 done 不再伪装成完整答复。
package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"traework2api/internal/auth"
	"traework2api/internal/upstream"
)

// delayBodyReader 第一次 Read 前先睡 d：造出「上游已回 200、首帧还在预填充」的时序。
type delayBodyReader struct {
	r     io.Reader
	d     time.Duration
	first bool
}

func (d *delayBodyReader) Read(p []byte) (int, error) {
	if !d.first {
		d.first = true
		time.Sleep(d.d)
	}
	return d.r.Read(p)
}

// chatClient 按「对话调用序号」回内容的假上游；delay>0 时在读 body 前先静默。
func chatClient(fn func(call int) (status int, body string, delay time.Duration)) *upstream.Client {
	n := 0
	return &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if r.URL.Path != upstream.EpChat {
				return &http.Response{
					StatusCode: 200,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(strings.NewReader(`{"user_info":{"config_info_list":[]}}`)),
				}, nil
			}
			n++
			status, body, delay := fn(n)
			var rd io.Reader = strings.NewReader(body)
			if delay > 0 {
				rd = &delayBodyReader{r: rd, d: delay}
			}
			return &http.Response{
				StatusCode: status,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(rd),
			}, nil
		})},
		AgentHost: "https://fake.example",
		UgHost:    "https://fake.example",
		OAuthHost: "https://fake.example",
		ClientID:  upstream.ClientID,
	}
}

func fastKeepalive(t *testing.T) {
	t.Helper()
	upstream.SetKeepaliveIntervals(5*time.Millisecond, 10*time.Millisecond)
	t.Cleanup(func() { upstream.SetKeepaliveIntervals(1500*time.Millisecond, 15*time.Second) })
}

// capStopSeg 撞该模型输出 cap（completion=32000）但 done 仍假报 stop 的段。
const capStopSeg = "event:output\ndata:{\"response\":\"前半段\",\"tool_calls\":null}\n\n" +
	"event:token_usage\ndata:{\"prompt_tokens\":10,\"completion_tokens\":32000,\"total_tokens\":32010}\n\n" +
	"event:done\ndata:{\"finish_reason\":\"stop\"}\n\n"

// tailSeg 续写段（自然收尾）。
const tailSeg = "event:output\ndata:{\"response\":\"后半段\",\"tool_calls\":null}\n\n" +
	"event:token_usage\ndata:{\"prompt_tokens\":20,\"completion_tokens\":5,\"total_tokens\":25}\n\n" +
	"event:done\ndata:{\"finish_reason\":\"stop\"}\n\n"

// noDoneSeg 有正文与 usage、但没有 done 的段（上游干脆不说就 EOF）。
const noDoneSeg = "event:output\ndata:{\"response\":\"半句话\",\"tool_calls\":null}\n\n" +
	"event:token_usage\ndata:{\"prompt_tokens\":10,\"completion_tokens\":1200,\"total_tokens\":1210}\n\n"

// TestNonStreamCutContinuesSplice 非流式撞 cap → 续写段拼进来，终态仍是 stop，
// usage 输出侧跨段求和（输入侧取末段）。
func TestNonStreamCutContinuesSplice(t *testing.T) {
	up := chatClient(func(call int) (int, string, time.Duration) {
		if call == 1 {
			return 200, capStopSeg, 0
		}
		return 200, tailSeg, 0
	})
	h := cutHandler(up)
	rec := post(h, "/v1/chat/completions",
		`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var obj map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &obj); err != nil {
		t.Fatalf("非 JSON 响应：%s", rec.Body)
	}
	choice := obj["choices"].([]any)[0].(map[string]any)
	msg := choice["message"].(map[string]any)
	if got := msg["content"]; got != "前半段后半段" {
		t.Fatalf("正文=%v want 前半段后半段（续写段要拼进来）", got)
	}
	if fr := choice["finish_reason"]; fr != "stop" {
		t.Fatalf("finish_reason=%v want stop（救回来了就不是截断）", fr)
	}
	usage, _ := obj["usage"].(map[string]any)
	if c, _ := usage["completion_tokens"].(float64); c != 32005 {
		t.Fatalf("completion_tokens=%v want 32005（跨段求和）", usage["completion_tokens"])
	}
	if p, _ := usage["prompt_tokens"].(float64); p != 20 {
		t.Fatalf("prompt_tokens=%v want 20（末段值，不求和）", usage["prompt_tokens"])
	}
}

// TestNonStreamNoDoneReportsLength 非流式 + 上游连续两次不发 done → 必须如实报 length，
// 不能按 Aggregate 的初值 "stop" 把半句话当完整答复。
func TestNonStreamNoDoneReportsLength(t *testing.T) {
	up := chatClient(func(int) (int, string, time.Duration) { return 200, noDoneSeg, 0 })
	h := cutHandler(up)
	rec := post(h, "/v1/chat/completions",
		`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var obj map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &obj); err != nil {
		t.Fatalf("非 JSON 响应：%s", rec.Body)
	}
	choice := obj["choices"].([]any)[0].(map[string]any)
	if fr := choice["finish_reason"]; fr != "length" {
		t.Fatalf("finish_reason=%v want length（无 done 收尾不得伪装成 stop）", fr)
	}
}

// TestHeadKeepaliveThenEmptyRotatesAfterCommit 第一个号静默（心跳已提交 200）后空流：
// 仍要换号，第二个号的正文接在同一条流里，客户端只看到一条正常回复。
func TestHeadKeepaliveThenEmptyRotatesAfterCommit(t *testing.T) {
	fastKeepalive(t)
	up := chatClient(func(call int) (int, string, time.Duration) {
		if call == 1 {
			return 200, "", 40 * time.Millisecond // 静默 40ms → 心跳先落笔
		}
		return 200, soloDoneSSE, 0
	})
	h := NewHandler(Config{
		Pool: testPoolWith(
			&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
			&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
		),
		Upstream:  up,
		MaxRotate: 2,
	})
	rec := post(h, "/v1/chat/completions",
		`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	body := rec.Body.String()
	if !strings.Contains(body, ": keepalive") {
		t.Fatalf("首帧静默期应先发过心跳：%q", body)
	}
	if !strings.Contains(body, "继续写完") {
		t.Fatalf("第二个号的正文没接上（空流之后应换号）：%q", body)
	}
	if strings.Contains(body, "event: error") {
		t.Fatalf("换号成功后不该写 error 帧：%q", body)
	}
	if n := strings.Count(body, `"finish_reason":"stop"`); n != 1 {
		t.Fatalf("终态帧数=%d want 1：%q", n, body)
	}
}

// TestReadErrorBeforeContentRotates 上游还没吐出任何东西就断链（连接重置/首字节超时）：
// 客户端一个字节都没收到，必须换号重试，而不是给它一个空的 200。
func TestReadErrorBeforeContentRotates(t *testing.T) {
	first := true
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if r.URL.Path != upstream.EpChat {
				return &http.Response{
					StatusCode: 200,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(strings.NewReader(`{"user_info":{"config_info_list":[]}}`)),
				}, nil
			}
			if first {
				first = false
				return &http.Response{
					StatusCode: 200,
					Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
					Body:       io.NopCloser(iotest.ErrReader(errors.New("connection reset by peer"))),
				}, nil
			}
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(soloDoneSSE)),
			}, nil
		})},
		AgentHost: "https://fake.example",
		UgHost:    "https://fake.example",
		OAuthHost: "https://fake.example",
		ClientID:  upstream.ClientID,
	}
	h := NewHandler(Config{
		Pool: testPoolWith(
			&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
			&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
		),
		Upstream:  up,
		MaxRotate: 2,
	})
	rec := post(h, "/v1/chat/completions",
		`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	body := rec.Body.String()
	if !strings.Contains(body, "继续写完") {
		t.Fatalf("零字节断链应换号重试，第二个号的正文没接上：%q", body)
	}
	if strings.Contains(body, "event: error") {
		t.Fatalf("换号成功后不该写 error 帧：%q", body)
	}
}

// TestCommittedStreamErrorIsFrame 全部号都失败、且心跳已经提交了 200 时，错误必须以
// 协议内 error 帧收尾——往 SSE 流里追加裸 JSON 就是另一种「莫名断流」。
func TestCommittedStreamErrorIsFrame(t *testing.T) {
	fastKeepalive(t)
	up := chatClient(func(int) (int, string, time.Duration) { return 200, "", 40 * time.Millisecond })
	h := NewHandler(Config{
		Pool:      testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream:  up,
		MaxRotate: 1,
	})
	rec := post(h, "/v1/chat/completions",
		`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	body := rec.Body.String()
	if !strings.Contains(body, ": keepalive") {
		t.Fatalf("静默期应先发过心跳：%q", body)
	}
	if !strings.Contains(body, "event: error") {
		t.Fatalf("已提交的流要以 error 事件收尾：%q", body)
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("error 事件之后要补 [DONE]：%q", body)
	}
	// 裸 JSON（非 SSE 行）落进流里就是脏数据：每个非空行都必须是注释/字段/数据行。
	for _, ln := range strings.Split(body, "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" || strings.HasPrefix(ln, ":") || strings.HasPrefix(ln, "event:") ||
			strings.HasPrefix(ln, "data:") || strings.HasPrefix(ln, "id:") {
			continue
		}
		t.Fatalf("流里出现非 SSE 行 %q（裸 JSON 会被客户端当脏帧）：\n%s", ln, body)
	}
}
