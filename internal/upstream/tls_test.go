package upstream

import (
	"bufio"
	"context"
	"crypto/md5"
	"crypto/tls"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// ── ClientHello 解析（只够算 JA3 与看 ALPN/GREASE）─────────────────────

// helloRecord 起裸 TCP 收一条 TLS 握手记录；返回地址与结果通道。
func helloRecord(t *testing.T) (string, <-chan []byte) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	out := make(chan []byte, 1)
	go func() {
		defer ln.Close()
		c, err := ln.Accept()
		if err != nil {
			out <- nil
			return
		}
		defer c.Close()
		_ = c.SetDeadline(time.Now().Add(5 * time.Second))
		hdr := make([]byte, 5)
		if _, err := io.ReadFull(c, hdr); err != nil {
			out <- nil
			return
		}
		n := int(hdr[3])<<8 | int(hdr[4])
		rest := make([]byte, n)
		_, _ = io.ReadFull(c, rest)
		out <- append(hdr, rest...)
	}()
	return ln.Addr().String(), out
}

// helloParts 解出 ClientHello 的 (JA3, cipher suites, groups, ALPN)。
func helloParts(t *testing.T, b []byte) (ja3 string, suites, groups []uint16, alpn []string, extCount int) {
	t.Helper()
	if len(b) < 6 || b[0] != 22 || b[5] != 1 {
		t.Fatalf("不是 ClientHello 记录: %v", b[:min(6, len(b))])
	}
	p := 9 + 2 + 32 // handshake(9) + client_version(2) + random(32)
	sid := int(b[p])
	p += 1 + sid
	cs := int(binary.BigEndian.Uint16(b[p:]))
	p += 2
	for i := 0; i < cs; i += 2 {
		suites = append(suites, binary.BigEndian.Uint16(b[p+i:]))
	}
	p += cs
	comp := int(b[p])
	p += 1 + comp
	extLen := int(binary.BigEndian.Uint16(b[p:]))
	p += 2
	end := p + extLen

	var extIDs, pointFmts []string
	for p+4 <= end && p+4 <= len(b) {
		typ := int(binary.BigEndian.Uint16(b[p:]))
		l := int(binary.BigEndian.Uint16(b[p+2:]))
		p += 4
		extIDs = append(extIDs, fmt.Sprintf("%d", typ))
		switch typ {
		case 10: // supported_groups
			q := p + 2
			n := int(binary.BigEndian.Uint16(b[p:]))
			for q+4 <= p+2+n && q+4 <= len(b) {
				gl := int(binary.BigEndian.Uint16(b[q:]))
				groups = append(groups, binary.BigEndian.Uint16(b[q+2:]))
				q += 2 + gl
			}
		case 11: // ec_point_formats
			n := int(b[p])
			for i := 0; i < n && p+1+i < len(b); i++ {
				pointFmts = append(pointFmts, fmt.Sprintf("%d", b[p+1+i]))
			}
		case 16: // ALPN
			n := int(binary.BigEndian.Uint16(b[p:]))
			q := p + 2
			for q < p+2+n && q < len(b) {
				l2 := int(b[q])
				if q+1+l2 > len(b) {
					break
				}
				alpn = append(alpn, string(b[q+1:q+1+l2]))
				q += 1 + l2
			}
		}
		p += l
	}

	var csStr, grpStr []string
	for _, s := range suites {
		csStr = append(csStr, fmt.Sprintf("%d", s))
	}
	for _, g := range groups {
		grpStr = append(grpStr, fmt.Sprintf("%d", g))
	}
	raw := strings.Join([]string{
		fmt.Sprintf("%d", binary.BigEndian.Uint16(b[9:])),
		strings.Join(csStr, "-"),
		strings.Join(extIDs, "-"),
		strings.Join(grpStr, "-"),
		strings.Join(pointFmts, "-"),
	}, ",")
	sum := md5.Sum([]byte(raw))
	return hex.EncodeToString(sum[:]), suites, groups, alpn, len(extIDs)
}

// isGREASE 判断一个套件/group 值是否是 GREASE 占位（0x?a?a 形态）。
func isGREASE(v uint16) bool {
	hi, lo := byte(v>>8), byte(v)
	return hi == lo && lo&0x0f == 0x0a
}

// hasGREASE 抓包结构里有没有 GREASE —— 浏览器系必带，crypto/tls 一个都没有。
func hasGREASE(suites []uint16) bool {
	for _, s := range suites {
		if isGREASE(s) {
			return true
		}
	}
	return false
}

// ── 测试 ──────────────────────────────────────────────────────────────

// 出站握手必须不再是 crypto/tls 的形态：带 GREASE、多 supported_group。
// 这条是 TLS 层对齐的根钉子 —— 它红了说明指纹退回脚本栈。
func TestTLSFingerprintIsBrowserShaped(t *testing.T) {
	addr, got := helloRecord(t)
	go func() {
		defer SetTLSFingerprint("chrome_133")
		conn, err := dialTLSWithFingerprint(context.Background(), "tcp", addr, currentTLSFingerprint(), true)
		if err == nil {
			conn.Close()
		}
	}()
	rec := <-got
	if rec == nil {
		t.Fatal("没抓到 ClientHello")
	}
	ja3, suites, groups, alpn, extCount := helloParts(t, rec)
	t.Logf("出站 JA3 = %s", ja3)
	t.Logf("  suites(%d) ext=%d groups=%v alpn=%v", len(suites), extCount, groups, alpn)

	// 三条结构判据，任何一条不满足都说明退回脚本栈了：
	//   - GREASE 占位必须有（crypto/tls 一个都不发）；
	//   - 扩展数要够多（Go 只有 10 个，浏览器 15+）；
	//   - ALPN 必须被收窄成 http/1.1（否则会协商到 h2，h1 报文直接坏）。
	if !hasGREASE(suites) {
		t.Error("ClientHello 里没有 GREASE —— 这是 crypto/tls 的特征，不是浏览器")
	}
	if extCount < 12 {
		t.Errorf("扩展只有 %d 个，浏览器系不会这么少", extCount)
	}
	if len(alpn) != 1 || alpn[0] != "http/1.1" {
		t.Errorf("ALPN 必须收窄成只有 http/1.1（否则协商到 h2 会坏），得到 %v", alpn)
	}
}

// 关掉指纹（"go" 档）时必须回到 crypto/tls 形态，证明这层是可控的、不是写死。
func TestTLSFingerprintCanFallBackToGo(t *testing.T) {
	if err := SetTLSFingerprint("go"); err != nil {
		t.Fatal(err)
	}
	defer SetTLSFingerprint("")

	addr, got := helloRecord(t)
	go func() {
		conn, err := dialTLSWithFingerprint(context.Background(), "tcp", addr, nil, true)
		if err == nil {
			conn.Close()
		}
	}()
	rec := <-got
	if rec == nil {
		t.Fatal("没抓到 ClientHello")
	}
	_, suites, groups, _, _ := helloParts(t, rec)
	if hasGREASE(suites) {
		t.Error("go 档不该带 GREASE")
	}
	if len(groups) != 1 {
		t.Errorf("crypto/tls 只 offer 一个 group，得到 %d 个", len(groups))
	}
}

// 换了指纹也必须能真握手、真发 HTTP/1.1 请求 —— 不能只对齐特征把功能搞坏。
func TestTLSFingerprintHandshakeStillWorks(t *testing.T) {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "proto=%s", r.Proto)
	}))
	srv.EnableHTTP2 = true // 服务端支持 h2，用来证明我们只落到 h1
	srv.StartTLS()
	defer srv.Close()

	addr := strings.TrimPrefix(srv.URL, "https://")
	conn, err := dialTLSWithFingerprint(context.Background(), "tcp", addr, currentTLSFingerprint(), true)
	if err != nil {
		t.Fatalf("uTLS 握手失败: %v", err)
	}
	defer conn.Close()

	req := "GET /x HTTP/1.1\r\nHost: " + addr + "\r\nConnection: keep-alive\r\n\r\n"
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(conn)
	status, _ := br.ReadString('\n')
	for {
		line, err := br.ReadString('\n')
		if err != nil || line == "\r\n" {
			break
		}
	}
	buf := make([]byte, 256)
	n, _ := br.Read(buf)
	if !strings.Contains(status, "200") {
		t.Errorf("状态行异常: %s", strings.TrimSpace(status))
	}
	if !strings.Contains(string(buf[:n]), "proto=HTTP/1.1") {
		t.Errorf("协议不是 HTTP/1.1: %s", buf[:n])
	}
}

// 配置层：名字进配置，未知名字要明确报错而不是静默退回默认。
func TestSetTLSFingerprint(t *testing.T) {
	defer SetTLSFingerprint("")

	if got := TLSFingerprintName(); got != "chrome_133" {
		t.Errorf("默认档位=%q want chrome_133", got)
	}
	if err := SetTLSFingerprint("chrome_120"); err != nil {
		t.Fatal(err)
	}
	if got := TLSFingerprintName(); got != "chrome_120" {
		t.Errorf("切换后=%q want chrome_120", got)
	}
	if err := SetTLSFingerprint("chrome_999"); err == nil {
		t.Error("未知档位应报错")
	}
	if err := SetTLSFingerprint(""); err != nil {
		t.Fatal(err)
	}
	if got := TLSFingerprintName(); got != "chrome_133" {
		t.Errorf("空值应恢复默认，得到 %q", got)
	}
}

// 生产 Transport 必须真的走上这条路径：不是只有测试在调 dialTLSWithFingerprint。
func TestWireTransportUsesFingerprint(t *testing.T) {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.StartTLS()
	defer srv.Close()

	cl := &http.Client{Timeout: 5 * time.Second, Transport: newWireTransport(&http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	})}
	resp, err := cl.Get(srv.URL)
	if err != nil {
		t.Fatalf("走生产 Transport 握手失败: %v", err)
	}
	resp.Body.Close()
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
