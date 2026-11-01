package upstream

import (
	"bufio"
	"io"
	"crypto/tls"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// readAllFrom 把 pipe 另一端读完（带超时），用于断言 headConn 实际写出了什么。
func readAllFrom(t *testing.T, c net.Conn, want int) string {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, want)
	n, _ := io.ReadFull(c, buf)
	return string(buf[:n])
}

// 单次 Write 里带完整头：必须按客户端顺序重排，且 body 原样跟在后面。
func TestHeadConnReordersSingleWrite(t *testing.T) {
	cli, srv := net.Pipe()
	defer cli.Close()
	defer srv.Close()

	hc := newHeadConn(cli, "cn")
	raw := "POST /x HTTP/1.1\r\n" +
		"User-Agent: ua\r\n" +
		"Content-Type: application/json\r\n" +
		"X-App-Id: app\r\n" +
		"Host: h\r\n" +
		"Accept: */*\r\n" +
		"\r\n" +
		`{"a":1}`

	go func() { _, _ = hc.Write([]byte(raw)) }()
	got := readAllFrom(t, srv, len(raw))

	// Host 必须排第一（客户端的 host 在表首），Accept 在表尾。
	if !strings.Contains(got, "Host: h\r\n") {
		t.Fatalf("缺少 Host:\n%s", got)
	}
	hi := strings.Index(got, "Host: h")
	ui := strings.Index(got, "User-Agent: ua")
	ai := strings.Index(got, "accept: */*")
	if !(hi < ui && ui < ai) {
		t.Errorf("顺序不对（期望 Host < User-Agent < Accept）：hi=%d ui=%d ai=%d\n%s", hi, ui, ai, got)
	}
	if !strings.HasSuffix(got, `{"a":1}`) {
		t.Errorf("body 被改动：%q", got)
	}
}

// 连接复用：第二次 Write 是新请求的头，也必须重排。
func TestHeadConnReordersSecondRequest(t *testing.T) {
	cli, srv := net.Pipe()
	defer cli.Close()
	defer srv.Close()

	hc := newHeadConn(cli, "cn")
	req := "GET /a HTTP/1.1\r\nUser-Agent: ua\r\nHost: h\r\nAccept: */*\r\n\r\n"

	// 两次请求各写一次，服务端把它们都读出来。
	out := make(chan string, 1)
	go func() {
		_ = srv.SetReadDeadline(time.Now().Add(2 * time.Second))
		buf := make([]byte, 512)
		n, _ := srv.Read(buf)
		out <- string(buf[:n])
	}()

	if _, err := hc.Write([]byte(req)); err != nil {
		t.Fatal(err)
	}
	got := <-out
	if !strings.Contains(got, "HTTP/1.1\r\nHost: h\r\n") {
		t.Errorf("第二次请求头没重排：\n%s", got)
	}
}

// 头被拆成多次 Write 时：退回透传，不能拼坏。
func TestHeadConnPassesThroughSplitHead(t *testing.T) {
	cli, srv := net.Pipe()
	defer cli.Close()
	defer srv.Close()

	hc := newHeadConn(cli, "cn")
	part1 := "GET /a HTTP/1.1\r\nUser-Agent: ua\r\n"
	part2 := "Host: h\r\n\r\n"

	out := make(chan string, 1)
	go func() {
		_ = srv.SetReadDeadline(time.Now().Add(2 * time.Second))
		var b strings.Builder
		buf := make([]byte, 256)
		for b.Len() < len(part1)+len(part2) {
			n, err := srv.Read(buf)
			b.Write(buf[:n])
			if err != nil {
				break
			}
		}
		out <- b.String()
	}()

	_, _ = hc.Write([]byte(part1))
	_, _ = hc.Write([]byte(part2))
	got := <-out

	if got != part1+part2 {
		t.Errorf("拆分写不该被改写\n got=%q\nwant=%q", got, part1+part2)
	}
}

// reorderHead 自带的基本保证：请求行不动、头名小写化后按表排、值不动。
func TestReorderHeadBasics(t *testing.T) {
	in := []byte("POST /p HTTP/1.1\r\nX-Os-Version: win\r\nReferer: https://a/b\r\nHost: h")
	got := string(reorderHead(in, "cn"))

	if !strings.HasPrefix(got, "POST /p HTTP/1.1\r\n") {
		t.Fatalf("请求行被改：%q", got)
	}
	if !strings.Contains(got, "x-os-version: win") {
		t.Errorf("头名没小写化：%q", got)
	}
	// Referer 的 value 里有冒号和斜杠，不能被截断
	if !strings.Contains(got, "Referer: https://a/b") {
		t.Errorf("Referer 值被破坏：%q", got)
	}
	// host 在顺序表首位，应在 referer 之前
	if strings.Index(got, "Host:") > strings.Index(got, "Referer:") {
		t.Errorf("host 应排在 referer 前：%q", got)
	}
}

// 端到端：走完整 newWireTransport（utls 握手 + headConn 重排），在 TLS listener
// 侧读**明文**报文头，确认顺序真的按客户端表来。
func TestWireTransportHeaderOrderEndToEnd(t *testing.T) {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.StartTLS()
	defer srv.Close()

	// 用同一套证书起一个裸 TLS listener，自己读明文头（httptest 的 handler 拿不到顺序）。
	cfg := srv.TLS.Clone()
	ln, err := tls.Listen("tcp", "127.0.0.1:0", cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	got := make(chan string, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			got <- ""
			return
		}
		defer c.Close()
		_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
		var b strings.Builder
		buf := make([]byte, 4096)
		for !strings.Contains(b.String(), "\r\n\r\n") {
			n, err := c.Read(buf)
			if n > 0 {
				b.Write(buf[:n])
			}
			if err != nil {
				break
			}
		}
		got <- b.String()
	}()

	cl := &http.Client{Timeout: 5 * time.Second, Transport: newWireTransport(&http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	})}
	req, _ := http.NewRequest(http.MethodPost, "https://"+ln.Addr().String()+"/api/agent/v3/llm_utils_chat", strings.NewReader(`{}`))
	SOLOHeaders(req, nil, false)
	go func() { resp, err := cl.Do(req); if err == nil { resp.Body.Close() } }()

	raw := <-got
	if raw == "" {
		t.Fatal("没读到报文头")
	}
	head := raw[:strings.Index(raw, "\r\n\r\n")]

	// 关键几条的相对位置：host 最前、content-type 在 user-agent 前、accept 靠后。
	pos := func(name string) int {
		i := strings.Index(strings.ToLower(head), strings.ToLower(name)+":")
		return i
	}
	host, ct, ua, acc := pos("Host"), pos("content-type"), pos("user-agent"), pos("accept")
	if host < 0 || ct < 0 || ua < 0 || acc < 0 {
		t.Fatalf("缺头: host=%d ct=%d ua=%d accept=%d\n%s", host, ct, ua, acc, head)
	}
	if !(host < ct && ct < ua) {
		t.Errorf("顺序不符（期望 Host < content-type < user-agent）：host=%d ct=%d ua=%d\n%s", host, ct, ua, head)
	}
	if acc < ua {
		t.Errorf("accept 应排在 user-agent 之后：ua=%d acc=%d\n%s", ua, acc, head)
	}
	// 头名大小写：客户端那 8 条大写，其余小写。
	if !strings.Contains(head, "Host: ") {
		t.Errorf("Host 应为大写开头\n%s", head)
	}
	if strings.Contains(head, "\r\nX-App-Id: ") {
		t.Errorf("x-app-id 应为小写\n%s", head)
	}
}

var _ = bufio.NewReader
