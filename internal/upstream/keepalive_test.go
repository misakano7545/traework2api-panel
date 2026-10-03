package upstream

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// flusherRecorder 记录每次 Flush：保活是靠 Flush 把注释帧推出去的，没 Flush 等于没发。
type flusherRecorder struct {
	*httptest.ResponseRecorder
	flushes int
}

func (f *flusherRecorder) Flush() { f.flushes++ }

func drainSSE(r io.Reader) string {
	b, _ := io.ReadAll(r)
	return string(b)
}

// TestStreamKeepaliveDuringUpstreamSilence 上游只思考不出字时，下游必须按 keepaliveInterval
// 收到 `: keepalive` 注释帧——否则中间盒（Cloudflare 空闲 ~100s）会把长思考流掐成「输出到一半断了」。
func TestStreamKeepaliveDuringUpstreamSilence(t *testing.T) {
	old := keepaliveInterval
	keepaliveInterval = 20 * time.Millisecond
	t.Cleanup(func() { keepaliveInterval = old })

	// 上游：先给一个 output（进入「已开始出内容」状态），然后长时间静默，最后 done。
	up := strings.NewReader(
		"event: output\ndata:{\"response\":\"hi\"}\n\n" +
			"event: done\ndata:{\"finish_reason\":\"stop\"}\n\n")
	slow := &slowReader{r: up, gap: 120 * time.Millisecond}

	rec := &flusherRecorder{ResponseRecorder: httptest.NewRecorder()}
	done := make(chan struct{})
	go func() { defer close(done); _ = Stream(rec, slow) }()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("流没结束")
	}
	body := rec.Body.String()
	if n := strings.Count(body, ": keepalive"); n < 2 {
		t.Fatalf("静默期应至少发出 2 个保活注释帧，实得 %d：%q", n, body)
	}
	if rec.flushes < 2 {
		t.Fatalf("注释帧必须 Flush 才能到客户端，flushes=%d", rec.flushes)
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("正常事件必须还在：%q", body)
	}
}

// TestStreamEmptyReturnsSentinel 上游 200 但零模型事件时：不写任何字节、返回 ErrEmptyStream，
// 好让上层换号重试。若这里已经写了字节，上层就只能把空回复交给客户端了。
func TestStreamEmptyReturnsSentinel(t *testing.T) {
	rec := &flusherRecorder{ResponseRecorder: httptest.NewRecorder()}
	err := Stream(rec, strings.NewReader("event: metadata\ndata:{\"model\":\"\"}\n\n"))
	if err != ErrEmptyStream {
		t.Fatalf("空流应返回 ErrEmptyStream，实得 %v", err)
	}
	if body := rec.Body.String(); body != "" {
		t.Fatalf("空流不该写给客户端任何字节：%q", body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "" {
		t.Fatalf("空流不该提交 SSE 头（上层要写 JSON 错误）：%q", ct)
	}
}

// TestAggregateEmptyReturnsSentinel 非流式同一口径：空流不许伪装成「模型回了空话」。
func TestAggregateEmptyReturnsSentinel(t *testing.T) {
	if _, err := Aggregate(strings.NewReader("event: done\ndata:{\"finish_reason\":\"stop\"}\n\n")); err != ErrEmptyStream {
		t.Fatalf("空聚合应返回 ErrEmptyStream，实得 %v", err)
	}
	// 有正文就不能误判成空流。
	resp, err := Aggregate(strings.NewReader("event: output\ndata:{\"response\":\"ok\"}\n\nevent: done\ndata:{}\n\n"))
	if err != nil {
		t.Fatalf("有正文不该报错: %v", err)
	}
	if !strings.Contains(toString(resp), `"ok"`) {
		t.Fatalf("正文丢了: %v", resp)
	}
}

// toString 把聚合结果摊平成字符串，供子串断言。
func toString(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// TestStreamLongSilenceThenEmptyCommits 钉住那条取舍：静默超过一个保活间隔后必然已经
// 发过注释帧（响应已提交），此时上游再以空流结束就只能按现状收尾（[DONE]），不能换号重试。
// 反过来说，真正空转的上游是立刻结束的——那条路径留给 ErrEmptyStream 重试。
func TestStreamLongSilenceThenEmptyCommits(t *testing.T) {
	old := keepaliveInterval
	keepaliveInterval = 20 * time.Millisecond
	t.Cleanup(func() { keepaliveInterval = old })

	// 上游什么都不发，但拖过一个保活间隔才 EOF。
	rec := &flusherRecorder{ResponseRecorder: httptest.NewRecorder()}
	slow := &slowReader{r: strings.NewReader(""), gap: 100 * time.Millisecond}
	err := Stream(rec, slow)
	if err != ErrEmptyStream {
		// 空 reader 在第一次 Read 就 EOF，没有静默窗 → 仍应走重试路径。
		t.Fatalf("立即 EOF 的空流应返回 ErrEmptyStream，实得 %v", err)
	}
	if body := rec.Body.String(); body != "" {
		t.Fatalf("立即 EOF 不该写字节：%q", body)
	}

	// 有静默窗的空流：先保活，再按现状收尾。
	rec2 := &flusherRecorder{ResponseRecorder: httptest.NewRecorder()}
	slow2 := &slowReader{r: strings.NewReader("event: metadata\ndata:{}\n\n"), gap: 60 * time.Millisecond}
	if err := Stream(rec2, slow2); err != nil {
		t.Fatalf("静默过的流不该报错（已提交无法重试）：%v", err)
	}
	body2 := rec2.Body.String()
	if !strings.Contains(body2, ": keepalive") {
		t.Fatalf("静默期应发过保活：%q", body2)
	}
	if !strings.Contains(body2, "data: [DONE]") {
		t.Fatalf("已提交的流要正常收尾：%q", body2)
	}
}

// slowReader 在第一段之后插入 gap 静默，模拟「上游只思考不吐字」。
type slowReader struct {
	r      io.Reader
	gap    time.Duration
	paused bool
}

func (s *slowReader) Read(p []byte) (int, error) {
	if s.paused {
		time.Sleep(s.gap)
		s.paused = false
	}
	n, err := s.r.Read(p)
	if err == nil {
		s.paused = true // 每次读到数据后都停顿一下
	}
	return n, err
}
