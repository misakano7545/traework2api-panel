// readerr_test.go 上游传输层断链的两种收尾：写过内容 → 补截断终态再上抛（客户端
// 不会停在半截）；一个字节都没写过 → 什么都不写，交给调用方换号。
package upstream

import (
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
)

// errAfterReader 先吐一段数据，然后一直报 err（模拟连接中途被重置）。
type errAfterReader struct {
	first string
	err   error
	sent  bool
}

func (e *errAfterReader) Read(p []byte) (int, error) {
	if !e.sent && e.first != "" {
		e.sent = true
		return copy(p, e.first), nil
	}
	return 0, e.err
}

// TestStreamReadErrorEmitsTruncationTerminal 正文写到一半断链：必须补
// finish_reason=length + [DONE]，客户端才不会「停在半截且什么都不报」。
func TestStreamReadErrorEmitsTruncationTerminal(t *testing.T) {
	setKeepalive(t, 0, 0) // 关掉心跳，避免注释帧干扰断言

	raw := "event:output\ndata:{\"response\":\"半句\"}\n\n"
	rd := &errAfterReader{first: raw, err: errors.New("connection reset by peer")}
	rec := &flusherRecorder{ResponseRecorder: httptest.NewRecorder()}
	err := Stream(rec, rd)
	if err == nil {
		t.Fatal("读错误必须上抛（调用方据此记账/不解绑会话）")
	}
	body := rec.Body.String()
	if !strings.Contains(body, "半句") {
		t.Fatalf("已写出的正文丢了：%q", body)
	}
	if n := strings.Count(body, `"finish_reason":"length"`); n != 1 {
		t.Fatalf("截断终态帧数=%d want 1：%q", n, body)
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("终态之后要补 [DONE]：%q", body)
	}
}

// TestStreamReadErrorBeforeContentWritesNothing 首帧都没拿到就断链：一个字节都不写，
// 由 handler 换号重试（写了字节就只能就地收尾）。
func TestStreamReadErrorBeforeContentWritesNothing(t *testing.T) {
	setKeepalive(t, 0, 0)

	rd := &errAfterReader{err: errors.New("connection reset by peer")}
	rec := &flusherRecorder{ResponseRecorder: httptest.NewRecorder()}
	if err := Stream(rec, rd); err == nil {
		t.Fatal("读错误必须上抛")
	}
	if body := rec.Body.String(); body != "" {
		t.Fatalf("零字节断链不该写任何东西（否则不能换号）：%q", body)
	}
}
