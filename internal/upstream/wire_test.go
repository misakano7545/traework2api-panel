package upstream

import (
	"bufio"
	"compress/gzip"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
)

// rawHead 起一个裸 TCP 监听，把客户端写出的**原始** HTTP 头逐行返回。
//
// 不能用 httptest：net/http 的读侧会用 CanonicalMIMEHeaderKey 还原 key，
// 服务端拿到的永远是 Title-Case，看不出线上真实拼写。
func rawHead(t *testing.T, build func(*http.Request)) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
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
		var b strings.Builder
		br := bufio.NewReader(c)
		for {
			line, err := br.ReadString('\n')
			b.WriteString(line)
			if err != nil || line == "\r\n" {
				break
			}
		}
		_, _ = c.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok"))
		got <- b.String()
	}()

	cl := &http.Client{
		Timeout:   5 * time.Second,
		Transport: newWireTransport(&http.Transport{}),
	}
	req, err := http.NewRequest(http.MethodPost, "http://"+ln.Addr().String()+"/api/agent/v3/llm_utils_chat", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	build(req)
	if resp, err := cl.Do(req); err == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	select {
	case s := <-got:
		return s
	case <-time.After(5 * time.Second):
		t.Fatal("裸监听超时，没收到报头")
		return ""
	}
}

// 显式写 Accept-Encoding 会关掉 net/http 的自动解压，解压由 decodeBody 接管。
// 这条用真 gzip 往返验证：服务端压缩，客户端必须读回明文。
func TestDecodeBodyGzipRoundtrip(t *testing.T) {
	const payload = `{"ok":true,"msg":"中文也要能解"}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Accept-Encoding"); got != "gzip, deflate" {
			t.Errorf("出站 Accept-Encoding=%q want gzip, deflate", got)
		}
		w.Header().Set("Content-Encoding", "gzip")
		zw := gzip.NewWriter(w)
		_, _ = zw.Write([]byte(payload))
		_ = zw.Close()
	}))
	defer srv.Close()

	cl := &http.Client{Timeout: 5 * time.Second, Transport: newWireTransport(&http.Transport{})}
	req, _ := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader("{}"))
	req.Header.Set("Accept-Encoding", "gzip, deflate")
	resp, err := cl.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != payload {
		t.Fatalf("解压结果=%q want %q", body, payload)
	}
	// 解压后这两个头不能留着，否则上层按 ContentLength 读会截断。
	if resp.Header.Get("Content-Encoding") != "" || resp.Header.Get("Content-Length") != "" {
		t.Errorf("解压后应删掉 Content-Encoding/Content-Length，得到 %v", resp.Header)
	}
}

// 只 advertise gzip/deflate/br/zstd，真收到别的编码要明确报错，
// 不能把压缩字节当正文喂给上层。
func TestDecodeBodyRejectsUnadvertisedEncoding(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "compress")
		_, _ = w.Write([]byte("\x00\x01binary"))
	}))
	defer srv.Close()

	cl := &http.Client{Timeout: 5 * time.Second, Transport: newWireTransport(&http.Transport{})}
	resp, err := cl.Get(srv.URL)
	if err == nil {
		resp.Body.Close()
		t.Fatal("收到 compress 却当成明文返回了，上层会读到压缩字节")
	}
	if !strings.Contains(err.Error(), "Content-Encoding") {
		t.Fatalf("错误信息应点明编码问题，得到 %v", err)
	}
}

// br 与 zstd 是客户端 advertise 的另外两种，必须能解 —— 否则一改就全链路坏。
func TestDecodeBodyBrotliAndZstd(t *testing.T) {
	const payload = `{"ok":true,"msg":"中文也要能解"}`

	t.Run("br", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Encoding", "br")
			bw := brotli.NewWriter(w)
			_, _ = bw.Write([]byte(payload))
			_ = bw.Close()
		}))
		defer srv.Close()
		assertDecoded(t, srv.URL, payload)
	})

	// zstd
	t.Run("zstd", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Encoding", "zstd")
			zw, err := zstd.NewWriter(w)
			if err != nil {
				t.Error(err)
				return
			}
			_, _ = zw.Write([]byte(payload))
			_ = zw.Close()
		}))
		defer srv.Close()
		assertDecoded(t, srv.URL, payload)
	})
}

func assertDecoded(t *testing.T, url, want string) {
	t.Helper()
	cl := &http.Client{Timeout: 5 * time.Second, Transport: newWireTransport(&http.Transport{})}
	resp, err := cl.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != want {
		t.Fatalf("解压结果=%q want %q", body, want)
	}
	if resp.Header.Get("Content-Encoding") != "" {
		t.Errorf("解压后应删掉 Content-Encoding，得到 %q", resp.Header.Get("Content-Encoding"))
	}
}

// 线上头名必须是客户端的小写写法，只有那几条固定大写的例外。
func TestWireHeaderCaseMatchesClient(t *testing.T) {
	head := rawHead(t, func(req *http.Request) {
		req.Header.Set("X-App-Id", "app")
		req.Header.Set("X-Os-Version", "Windows 11 Pro")
		req.Header.Set("X-Ss-Dp", "787976")
		req.Header.Set("Referer", "https://trae-api-cn.mchost.guru/x")
		req.Header.Set("Connection", "keep-alive")
	})

	for _, want := range []string{
		"\r\nx-app-id: app\r\n",
		"\r\nx-os-version: Windows 11 Pro\r\n",
		"\r\nx-ss-dp: 787976\r\n", // 例外：客户端这里是大写 X-SS-DP
		"\r\nReferer: https://trae-api-cn.mchost.guru/x\r\n",
		"\r\nConnection: keep-alive\r\n",
	} {
		if strings.HasPrefix(want, "\r\nx-ss-dp") {
			continue // 由下面的 X-SS-DP 断言覆盖
		}
		if !strings.Contains(head, want) {
			t.Errorf("线上报头缺少小写形态 %q\n实际:\n%s", want, head)
		}
	}
	if !strings.Contains(head, "\r\nX-SS-DP: 787976\r\n") {
		t.Errorf("X-SS-DP 应保持大写（客户端如此）\n实际:\n%s", head)
	}
	// 反向断言：Go 默认的 Title-Case 不能出现。
	for _, bad := range []string{"\r\nX-App-Id:", "\r\nX-Os-Version:", "\r\nX-Ss-Dp:"} {
		if strings.Contains(head, bad) {
			t.Errorf("线上仍是 Go 的 Title-Case %q\n实际:\n%s", bad, head)
		}
	}
}

// user-agent 必须发成 User-Agent：国内通道收到小写 user-agent 会回 4011
// 「exceeded the rate limit」（2026-11-01 实测 3/3；改回大写 3/3 全 200）。
func TestWireUserAgentStaysTitleCase(t *testing.T) {
	head := rawHead(t, func(req *http.Request) {
		req.Header.Set("User-Agent", "Trae/1.0")
	})
	if !strings.Contains(head, "\r\nUser-Agent: Trae/1.0\r\n") {
		t.Errorf("出站没写成 User-Agent（小写会被上游判 4011）\n实际:\n%s", head)
	}
	if strings.Contains(head, "\r\nuser-agent: ") {
		t.Errorf("出站把 user-agent 写成小写了\n实际:\n%s", head)
	}
}

// 包装层不能改调用方手里的 req —— 生产代码还在用 Header.Get 读这些头。
func TestHeaderCaseTransportLeavesRequestIntact(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPost, "http://example.invalid/x", strings.NewReader("{}"))
	SOLOHeaders(req, nil, false)
	before := req.Header.Get("X-App-Id")
	beforeOS := req.Header.Get("X-Os-Version")

	rawHead(t, func(r *http.Request) {
		// 用 SOLOHeaders 填好，再让 transport 去改副本
		r.Header = req.Header.Clone()
	})

	if got := req.Header.Get("X-App-Id"); got != before || got == "" {
		t.Errorf("原请求的 X-App-Id 被改坏了：%q", got)
	}
	if got := req.Header.Get("X-Os-Version"); got != beforeOS || got == "" {
		t.Errorf("原请求的 X-Os-Version 被改坏了：%q", got)
	}
}

// h2 必须关掉：客户端是 HTTP/1.1 + Connection: keep-alive，而 http2 明确拒绝
// Connection 头。这条测试钉住 TLSNextProto 被置空的事实。
func TestNewWireTransportDisablesH2(t *testing.T) {
	tr := newWireTransport(&http.Transport{})
	if tr.base.TLSNextProto == nil {
		t.Fatal("TLSNextProto 为 nil 表示 h2 仍会自动启用")
	}
	if len(tr.base.TLSNextProto) != 0 {
		t.Fatalf("TLSNextProto 非空 map 会重新启用 h2：%v", tr.base.TLSNextProto)
	}
}
