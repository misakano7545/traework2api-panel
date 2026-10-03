package server

import (
	"log"
	"net/http/httptest"
	"strings"
	"testing"

	"traework2api/internal/auth"
	"traework2api/internal/panel"
	"traework2api/internal/reqlog"
	"traework2api/internal/upstream"
)

// 请求要进「运行日志」页的对话频道，且要带调用来源——这是用户直接报的两件事：
//   ① 请求不打日志（原先只有出错才打 chat_stream，成功请求在运行日志里看不见）；
//   ② 请求记录不取来源 IP/UA（缺省值是 false，而同族的 WB 缺省 true）。
//
// 两条都在同一条路径上验：日志环（面板运行日志的取数来源）里出现对话行、且带 tok/src/ua；
// 同时请求记录事件里 ClientIP/UserAgent 有值。

// chamLogHarness 起一条成功的对话请求，返回日志环与请求记录里的那条事件。
func runChatWithRing(t *testing.T, clientInfo bool) (*panel.Ring, reqlog.Event) {
	t.Helper()
	var calls []string
	up := fakeWithPaths(&calls, func(path, _ string) (int, string, bool) {
		if path != upstream.EpChat {
			return 200, `{"user_info":{"config_info_list":[]}}`, false
		}
		return 200, soloSSE, true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	rec := reqlog.New(reqlog.Config{})
	h := NewHandler(Config{Pool: p, Upstream: up, RequestLog: rec, RequestClientInfo: clientInfo})

	ring := panel.NewRing(50)
	prev := log.Writer()
	log.SetOutput(ring)
	t.Cleanup(func() { log.SetOutput(prev) })

	r := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	r.Header.Set("User-Agent", "probe/1.0 (+hermes)")
	r.RemoteAddr = "203.0.113.7:5555"
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)
	if rr.Code != 200 {
		t.Fatalf("请求没成功，code=%d body=%s", rr.Code, rr.Body)
	}
	recs := rec.Snapshot().Recent
	if len(recs) == 0 {
		t.Fatal("请求记录里没有事件")
	}
	return ring, recs[0]
}

func TestChatRequestLoggedToChatChannel(t *testing.T) {
	ring, _ := runChatWithRing(t, false)
	var chat []panel.LogEntry
	for _, e := range ring.Snapshot() {
		if e.Ch == panel.ChChat {
			chat = append(chat, e)
		}
	}
	if len(chat) == 0 {
		t.Fatalf("对话频道没有请求行（用户报的「请求不会记录在运行日志里面」）：%+v", ring.Snapshot())
	}
	line := chat[len(chat)-1].Text
	for _, must := range []string{"| #", "glm-5.2", "tok=7", "out=success", "rid=req-"} {
		if !strings.Contains(line, must) {
			t.Errorf("对话行缺 %q：%s", must, line)
		}
	}
}

// TestChatRequestSourceInLogAndRecord 来源字段：开启时日志行与请求记录都要有 IP/UA，
// 关闭时两处都不出现（同一个开关管两处，不能一处有一处没有）。
func TestChatRequestSourceInLogAndRecord(t *testing.T) {
	ring, ev := runChatWithRing(t, true)
	if ev.ClientIP != "203.0.113.7" || !strings.Contains(ev.UserAgent, "probe") {
		t.Errorf("请求记录没取到来源：ip=%q ua=%q", ev.ClientIP, ev.UserAgent)
	}
	var line string
	for _, e := range ring.Snapshot() {
		if e.Ch == panel.ChChat {
			line = e.Text
		}
	}
	if !strings.Contains(line, "src=203.0.113.7") || !strings.Contains(line, "ua=") {
		t.Errorf("日志行没带来源：%s", line)
	}

	ring2, ev2 := runChatWithRing(t, false)
	if ev2.ClientIP != "" || ev2.UserAgent != "" {
		t.Errorf("关掉来源记录后事件里仍有来源：ip=%q ua=%q", ev2.ClientIP, ev2.UserAgent)
	}
	for _, e := range ring2.Snapshot() {
		if e.Ch == panel.ChChat && (strings.Contains(e.Text, "src=") || strings.Contains(e.Text, "ua=")) {
			t.Errorf("关掉来源记录后日志行仍有来源：%s", e.Text)
		}
	}
}
