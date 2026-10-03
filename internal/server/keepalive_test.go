package server

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// 保活注释帧必须在协议包装层被原样透传：这两个 writer 各自解析 `data:` 帧，
// 早先的非 data 帧一律丢弃——那样上游/网关发的空闲保活就到不了客户端，白加。
// 两条都走同一份断言：注释帧进 → 注释帧出。
func TestProtocolWritersForwardKeepaliveComments(t *testing.T) {
	for _, tc := range []struct {
		name  string
		write func(rec *httptest.ResponseRecorder) error
	}{
		{"responses", func(rec *httptest.ResponseRecorder) error {
			w := &responsesWriter{ResponseWriter: rec, stream: true}
			w.Header().Set("Content-Type", "text/event-stream")
			_, err := w.Write([]byte(": keepalive\n\n"))
			return err
		}},
		{"messages", func(rec *httptest.ResponseRecorder) error {
			w := &messagesWriter{ResponseWriter: rec, stream: true}
			w.Header().Set("Content-Type", "text/event-stream")
			_, err := w.Write([]byte(": keepalive\n\n"))
			return err
		}},
	} {
		rec := httptest.NewRecorder()
		if err := tc.write(rec); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if !strings.Contains(rec.Body.String(), ": keepalive") {
			t.Fatalf("%sWriter 吞掉了保活注释帧：%q", tc.name, rec.Body.String())
		}
	}
}
