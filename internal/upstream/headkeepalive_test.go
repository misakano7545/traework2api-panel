// headkeepalive_test.go 首帧心跳（headKeepaliveInterval）的回归：等上游第一个数据帧的
// 空窗里要按秒级节拍写注释帧，首帧之后回到 15s 档，且心跳**不能**把换号（空流重试、
// 首帧错误否决）堵死——那三件事是同一个改动的两面。
package upstream

import (
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// headDelayReader 第一次 Read 之前先睡 delay：模拟「上游已回 200、首帧还在预填充」。
type headDelayReader struct {
	r     io.Reader
	delay time.Duration
	first bool
}

func (h *headDelayReader) Read(p []byte) (int, error) {
	if !h.first {
		h.first = true
		time.Sleep(h.delay)
	}
	return h.r.Read(p)
}

// stagedReader 每次 Read 只吐一段，段与段之间睡 gap：能造出「正文先到、随后静默」的
// 真实时序（bufio 一次只填一段，所以静默落在 parser 需要下一行的时候）。
type stagedReader struct {
	stages []string
	gap    time.Duration
	i      int
}

func (s *stagedReader) Read(p []byte) (int, error) {
	if s.i >= len(s.stages) {
		return 0, io.EOF
	}
	if s.i > 0 {
		time.Sleep(s.gap)
	}
	b := []byte(s.stages[s.i])
	s.i++
	return copy(p, b), nil
}

// setKeepalive 临时改两档节拍（测试串行，包级变量，照 keepalive_test.go 的写法）。
func setKeepalive(t *testing.T, head, idle time.Duration) {
	t.Helper()
	oldHead, oldIdle := headKeepaliveInterval, keepaliveInterval
	headKeepaliveInterval, keepaliveInterval = head, idle
	t.Cleanup(func() { headKeepaliveInterval, keepaliveInterval = oldHead, oldIdle })
}

// TestKeepaliveDefaults 两档节拍的缺省值本身也是契约：首帧 1.5s、首帧后 15s。
// 首帧回到 15s 就等于这条防线不存在（客户端 3~4s 就自己断连了），所以在默认值上钉一枪。
func TestKeepaliveDefaults(t *testing.T) {
	if headKeepaliveInterval != 1500*time.Millisecond {
		t.Fatalf("headKeepaliveInterval=%v want 1.5s", headKeepaliveInterval)
	}
	if keepaliveInterval != 15*time.Second {
		t.Fatalf("keepaliveInterval=%v want 15s", keepaliveInterval)
	}
}

// TestHeadKeepaliveBeforeFirstChunk 首帧迟到 80ms、首帧档 10ms：客户端必须在正文之前
// 就收到心跳——这正是客户端在 3~4s 自己断连的那段空窗。
func TestHeadKeepaliveBeforeFirstChunk(t *testing.T) {
	setKeepalive(t, 10*time.Millisecond, 2*time.Second)

	raw := "event:output\ndata:{\"response\":\"hi\"}\n\n" +
		"event:done\ndata:{\"finish_reason\":\"stop\"}\n\n"
	rec := &flusherRecorder{ResponseRecorder: httptest.NewRecorder()}
	if err := Stream(rec, &headDelayReader{r: strings.NewReader(raw), delay: 80 * time.Millisecond}); err != nil {
		t.Fatalf("stream: %v", err)
	}
	body := rec.Body.String()
	i := strings.Index(body, "data: {")
	if i < 0 {
		t.Fatalf("没有数据帧：%q", body)
	}
	head := body[:i]
	if n := strings.Count(head, ": keepalive"); n < 3 {
		t.Fatalf("首帧前只有 %d 个心跳（want >=3，档位 10ms / 静默 80ms）：%q", n, head)
	}
}

// TestHeadKeepaliveFallsBackToIdleAfterChunk 首帧之后必须回到 idle 档：正文已到、
// 上游进入长思考时按 15s 打点，不能继续按 1.5s 刷注释帧。
func TestHeadKeepaliveFallsBackToIdleAfterChunk(t *testing.T) {
	setKeepalive(t, 10*time.Millisecond, 600*time.Millisecond)

	sr := &stagedReader{
		stages: []string{
			"event:output\ndata:{\"response\":\"hi\"}\n\n",
			"event:done\ndata:{\"finish_reason\":\"stop\"}\n\n",
		},
		gap: 90 * time.Millisecond,
	}
	rec := &flusherRecorder{ResponseRecorder: httptest.NewRecorder()}
	if err := Stream(rec, sr); err != nil {
		t.Fatalf("stream: %v", err)
	}
	body := rec.Body.String()
	if n := strings.Count(body, ": keepalive"); n != 0 {
		t.Fatalf("首帧后 90ms 静默不该有心跳（idle 档 600ms），实得 %d 个：%q", n, body)
	}
	if !strings.Contains(body, "hi") {
		t.Fatalf("正文丢了：%q", body)
	}
}

// TestHeadKeepaliveDoesNotBlockEmptyRotation 心跳写过注释帧之后，空流仍必须返回
// ErrEmptyStream 交回换号（handler 会把下一个号的帧接在同一条 200 上，客户端无感）。
func TestHeadKeepaliveDoesNotBlockEmptyRotation(t *testing.T) {
	setKeepalive(t, 10*time.Millisecond, 20*time.Millisecond)

	rec := &flusherRecorder{ResponseRecorder: httptest.NewRecorder()}
	err := Stream(rec, &headDelayReader{r: strings.NewReader(""), delay: 60 * time.Millisecond})
	if err != ErrEmptyStream {
		t.Fatalf("空流应返回 ErrEmptyStream，实得 %v", err)
	}
	if body := rec.Body.String(); !strings.Contains(body, ": keepalive") {
		t.Fatalf("空窗里应先发过心跳：%q", body)
	}
}

// TestFirstFrameErrorRotatesAfterHeadKeepalive 首帧是账号级错误时，即便心跳已经提交了
// 200，也要把错误交回调用方换号（注释帧不算「内容已开始」）。
func TestFirstFrameErrorRotatesAfterHeadKeepalive(t *testing.T) {
	setKeepalive(t, 10*time.Millisecond, 20*time.Millisecond)

	raw := "event:error\ndata:{\"code\":1005,\"message\":\"plan limit\"}\n\n"
	rec := &flusherRecorder{ResponseRecorder: httptest.NewRecorder()}
	err := Stream(rec, &headDelayReader{r: strings.NewReader(raw), delay: 60 * time.Millisecond})
	if err == nil {
		t.Fatal("首帧账号级错误应交回调用方换号")
	}
	if _, ok := err.(*SOLOStreamError); !ok {
		t.Fatalf("err=%#v，want *SOLOStreamError", err)
	}
	body := rec.Body.String()
	if strings.Contains(body, "event: error") {
		t.Fatalf("已交回换号的流里不该写 error 帧（客户端会收到两套终态）：%q", body)
	}
	if !strings.Contains(body, ": keepalive") {
		t.Fatalf("空窗里应先发过心跳：%q", body)
	}
}
