package main

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	utls "github.com/refraction-networking/utls"
)

// sendHello 用 utls 打一条 ClientHello 过来，可选先走 CONNECT 代握。
//
// 返回 error 而不是直接 t.Fatalf：它在独立 goroutine 里跑，testing 的 Fatal
// 只能在测试自己的 goroutine 上调（go vet 会拦）。
func sendHello(addr string, viaConnect bool) error {
	conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		return fmt.Errorf("拨号失败: %w", err)
	}
	defer conn.Close()

	if viaConnect {
		_, _ = conn.Write([]byte("CONNECT example.com:443 HTTP/1.1\r\nHost: example.com:443\r\n\r\n"))
		br := bufio.NewReader(conn)
		line, err := br.ReadString('\n')
		if err != nil || !strings.Contains(line, "200") {
			return fmt.Errorf("代握没拿到 200: %q %v", line, err)
		}
		for {
			l, err := br.ReadString('\n')
			if err != nil || l == "\r\n" {
				break
			}
		}
	}

	u := utls.UClient(conn, &utls.Config{ServerName: "example.com", InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}, utls.HelloChrome_133)
	_ = u.Handshake() // 桩不回包，握手必然失败，但 ClientHello 已经发出去了
	return nil
}

// 走 HTTP 代理（CONNECT）时必须能先代握再抓 TLS。
func TestCaptureViaConnect(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	type res struct {
		raw []byte
		ch  *ClientHello
		err error
	}
	out := make(chan res, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			out <- res{err: err}
			return
		}
		defer conn.Close()
		raw, ch, err := capture(conn, 8*time.Second)
		out <- res{raw, ch, err}
	}()

	go func() {
		if err := sendHello(ln.Addr().String(), true); err != nil {
			t.Error(err)
		}
	}()

	select {
	case r := <-out:
		if r.err != nil {
			t.Fatalf("capture 失败: %v", r.err)
		}
		if r.ch == nil {
			t.Fatal("没解析出 ClientHello")
		}
		if r.ch.ServerName != "example.com" {
			t.Errorf("SNI=%q want example.com", r.ch.ServerName)
		}
		if len(r.ch.CipherSuites) == 0 {
			t.Error("套件为空")
		}
		ja3, _ := r.ch.JA3()
		if len(ja3) != 32 {
			t.Errorf("JA3 长度异常: %q", ja3)
		}
		if !strings.HasPrefix(r.ch.JA4(), "t13d") {
			t.Errorf("JA4 前缀异常（版本/SNI 段错了）: %q", r.ch.JA4())
		}
		if len(r.raw) == 0 {
			t.Error("原始字节为空")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("超时")
	}
}

// 直连（无代理）时第一个字节就是 TLS，也要能抓。
func TestCaptureDirectTLS(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	out := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			out <- err
			return
		}
		defer conn.Close()
		_, ch, err := capture(conn, 8*time.Second)
		if err == nil && ch == nil {
			err = net.ErrClosed
		}
		out <- err
	}()

	go func() {
		if err := sendHello(ln.Addr().String(), false); err != nil {
			t.Error(err)
		}
	}()

	select {
	case err := <-out:
		if err != nil {
			t.Fatalf("直连抓取失败: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("超时")
	}
}

// GREASE 必须在 JA4 的三处都被滤掉 —— 不过滤会把版本算成 00、计数算多。
// 这条是实际踩过的坑：utls 的 133 在 supported_versions 里塞 0xbaba。
func TestJA4FiltersGREASE(t *testing.T) {
	ch, err := chrome133Hello()
	if err != nil {
		t.Fatalf("取 chrome_133 的 ClientHello 失败: %v", err)
	}
	ja4 := ch.JA4()
	if !strings.HasPrefix(ja4, "t13d") {
		t.Fatalf("JA4 版本段错误（GREASE 没滤掉？）: %q", ja4)
	}
	var grease int
	for _, v := range ch.CipherSuites {
		if isGREASE(v) {
			grease++
		}
	}
	if grease == 0 {
		t.Skip("这一版 utls 没发 GREASE 套件，跳过计数校验")
	}
	// 套件计数要等于「去 GREASE 后」的个数。
	parts := strings.Split(ja4, "_")
	if len(parts) != 3 {
		t.Fatalf("JA4 结构异常: %q", ja4)
	}
	a := parts[0]
	// JA4_a 布局：t(1) version(2) sni(1) cipherCount(2) extCount(2) alpn(2)
	if len(a) < 10 {
		t.Fatalf("JA4_a 太短: %q", a)
	}
	want := len(ch.CipherSuites) - grease
	if got := int(a[4]-'0')*10 + int(a[5]-'0'); got != want {
		t.Errorf("JA4 套件计数=%d want %d（原始 %d，其中 GREASE %d）", got, want, len(ch.CipherSuites), grease)
	}
}

// 与 133 的 diff 必须能正常产出（解析两条不同来源的 ClientHello 并对比）。
func TestCompareWithChrome133(t *testing.T) {
	ch, err := chrome133Hello()
	if err != nil {
		t.Fatal(err)
	}
	report := ch.compareWithChrome133()
	for _, want := range []string{"套件", "扩展", "JA3 抓包", "JA4 抓包"} {
		if !strings.Contains(report, want) {
			t.Errorf("报告缺少 %q 段:\n%s", want, report)
		}
	}
	if strings.Contains(report, "对比失败") {
		t.Fatalf("diff 失败:\n%s", report)
	}
}
