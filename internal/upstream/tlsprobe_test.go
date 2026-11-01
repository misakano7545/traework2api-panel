//go:build tlsprobe

// 真实上游 TLS 指纹实测桩：对四个生产主机分别用 crypto/tls 与 utls 握手，
// 打印协商结果与线上 ClientHello 形态。
//
//	go test -tags tlsprobe ./internal/upstream -run TestLiveTLSProbe -v
//
// 只读、不发业务请求、不吃额度（401 也是有效证据）。
package upstream

import (
	"context"
	"crypto/md5"
	"crypto/tls"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	utls "github.com/refraction-networking/utls"
)

var probeHosts = []string{
	"api5-normal.mchost.guru",        // CN agent
	"api.trae.cn",                    // CN ug
	"api5-normal-alisg.mchost.guru", // INTL agent
	"growsg-normal.trae.ai",         // INTL ug
}

// dialAndPeek 建连后读一条 ClientHello，再真握手。
func peekAndDial(t *testing.T, addr string, fp *tlsFingerprint) (ja3 string, suites, groups []uint16, ver uint16, cipher uint16, alpn string, err error) {
	raw, err := (&net.Dialer{Timeout: 10 * time.Second}).DialContext(context.Background(), "tcp", addr)
	if err != nil {
		return
	}
	defer raw.Close()

	// 先抓自己发出去的 ClientHello：开一个 io.TeeReader 截获前若干字节。
	host, _, _ := net.SplitHostPort(addr)
	if fp == nil {
		// Go 原生：TeeConn 包在**底层** raw 上，这样 ClientHello 的明文写出能被截获。
		tee := &teeConn{Conn: raw, buf: &limitedBuf{max: 8192}}
		tc := tls.Client(tee, &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12})
		_ = tc.HandshakeContext(context.Background())
		ja3, suites, groups = parseHello(tee.buf.Bytes())
		st := tc.ConnectionState()
		ver, cipher, alpn = st.Version, st.CipherSuite, st.NegotiatedProtocol
		return
	}
	cfg := &utls.Config{ServerName: host, MinVersion: tls.VersionTLS12}
	spec, e := specWithALPN(fp, alpnForWire())
	if e != nil {
		err = e
		return
	}
	u := utls.UClient(raw, cfg, utls.HelloCustom)
	if err = u.ApplyPreset(spec); err != nil {
		return
	}
	if err = u.HandshakeContext(context.Background()); err != nil {
		return
	}
	st := u.ConnectionState()
	ver, cipher, alpn = st.Version, st.CipherSuite, st.NegotiatedProtocol
	return
}

type teeConn struct {
	net.Conn
	buf *limitedBuf
}

func (c *teeConn) Write(p []byte) (int, error) {
	c.buf.Write(p)
	return c.Conn.Write(p)
}

type limitedBuf struct {
	b   []byte
	max int
}

func (l *limitedBuf) Write(p []byte) {
	if len(l.b) >= l.max {
		return
	}
	n := l.max - len(l.b)
	if n > len(p) {
		n = len(p)
	}
	l.b = append(l.b, p[:n]...)
}

func (l *limitedBuf) Bytes() []byte { return l.b }

// parseHello 从可拼接的多条 TLS 记录里找出 ClientHello 并解出关键字段。
func parseHello(b []byte) (ja3 string, suites, groups []uint16) {
	var hello []byte
	for p := 0; p+5 <= len(b); {
		typ := b[p]
		n := int(binary.BigEndian.Uint16(b[p+3:]))
		if p+5+n > len(b) {
			break
		}
		if typ == 22 {
			hello = append(hello, b[p:p+5+n]...)
		}
		p += 5 + n
	}
	if len(hello) < 43 {
		return
	}
	p := 9 + 2 + 32
	sid := int(hello[p])
	p += 1 + sid
	cs := int(binary.BigEndian.Uint16(hello[p:]))
	p += 2
	for i := 0; i < cs; i += 2 {
		suites = append(suites, binary.BigEndian.Uint16(hello[p+i:]))
	}
	p += cs
	comp := int(hello[p])
	p += 1 + comp
	extLen := int(binary.BigEndian.Uint16(hello[p:]))
	p += 2
	end := p + extLen
	for p+4 <= end && p+4 <= len(hello) {
		typ := int(binary.BigEndian.Uint16(hello[p:]))
		l := int(binary.BigEndian.Uint16(hello[p+2:]))
		p += 4
		if typ == 10 && p+2 <= len(hello) {
			q := p + 2
			n := int(binary.BigEndian.Uint16(hello[p:]))
			for q+4 <= p+2+n && q+4 <= len(hello) {
				gl := int(binary.BigEndian.Uint16(hello[q:]))
				groups = append(groups, binary.BigEndian.Uint16(hello[q+2:]))
				q += 2 + gl
			}
		}
		p += l
	}
	return ja3FromHello(hello), suites, groups
}

// ja3FromHello 只算 JA3，不依赖 testing.T（线上抓到的字节不经过 t）。
func ja3FromHello(hello []byte) string {
	if len(hello) < 43 {
		return ""
	}
	p := 9 + 2 + 32
	sid := int(hello[p])
	p += 1 + sid
	cs := int(binary.BigEndian.Uint16(hello[p:]))
	p += 2
	var csStr []string
	for i := 0; i < cs && p+i+1 < len(hello); i += 2 {
		csStr = append(csStr, fmt.Sprintf("%d", binary.BigEndian.Uint16(hello[p+i:])))
	}
	p += cs
	comp := int(hello[p])
	p += 1 + comp
	extLen := int(binary.BigEndian.Uint16(hello[p:]))
	p += 2
	end := p + extLen
	var extIDs, grpStr, pts []string
	for p+4 <= end && p+4 <= len(hello) {
		typ := int(binary.BigEndian.Uint16(hello[p:]))
		l := int(binary.BigEndian.Uint16(hello[p+2:]))
		p += 4
		extIDs = append(extIDs, fmt.Sprintf("%d", typ))
		switch typ {
		case 10:
			q := p + 2
			n := int(binary.BigEndian.Uint16(hello[p:]))
			for q+4 <= p+2+n && q+4 <= len(hello) {
				gl := int(binary.BigEndian.Uint16(hello[q:]))
				grpStr = append(grpStr, fmt.Sprintf("%d", binary.BigEndian.Uint16(hello[q+2:])))
				q += 2 + gl
			}
		case 11:
			n := int(hello[p])
			for i := 0; i < n && p+1+i < len(hello); i++ {
				pts = append(pts, fmt.Sprintf("%d", hello[p+1+i]))
			}
		}
		p += l
	}
	raw := strings.Join([]string{
		fmt.Sprintf("%d", binary.BigEndian.Uint16(hello[9:])),
		strings.Join(csStr, "-"),
		strings.Join(extIDs, "-"),
		strings.Join(grpStr, "-"),
		strings.Join(pts, "-"),
	}, ",")
	sum := md5.Sum([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// TestLiveTLSProbe 四个生产主机 × 两种指纹。
func TestLiveTLSProbe(t *testing.T) {
	for _, h := range probeHosts {
		addr := h + ":443"
		for _, mode := range []struct {
			name string
			fp   *tlsFingerprint
		}{
			{"crypto/tls", nil},
			{"utls aha（官方客户端形状）", ahaFingerprint()},
			{"utls chrome_133", chromeFingerprint(utls.HelloChrome_133)},
		} {
			t.Run(h+"/"+mode.name, func(t *testing.T) {
				start := time.Now()
				ja3, suites, groups, ver, cipher, alpn, err := peekAndDial(t, addr, mode.fp)
				dur := time.Since(start)
				if err != nil {
					t.Logf("握手失败(%v): %v", dur, err)
					return
				}
				t.Logf("握手 OK %v  TLS=0x%04x cipher=0x%04x alpn=%q", dur, ver, cipher, alpn)
				if mode.fp == nil {
					t.Logf("  [Go]   JA3=%s suites=%d groups=%v", ja3, len(suites), groups)
				} else {
					t.Logf("  [utls] suites=%d groups=%v", len(suites), groups)
				}
			})
		}
	}
}

// TestLiveHTTPProbe 带真实出站头发一次请求：401 即为链路通。
func TestLiveHTTPProbe(t *testing.T) {
	for _, h := range probeHosts {
		t.Run(h, func(t *testing.T) {
			cl := &http.Client{Timeout: 20 * time.Second, Transport: newWireTransport(&http.Transport{})}
			req, err := http.NewRequest(http.MethodPost, "https://"+h+"/api/agent/v3/llm_utils_chat", strings.NewReader("{}"))
			if err != nil {
				t.Fatal(err)
			}
			SOLOHeaders(req, nil, false)
			req.Header.Set("Authorization", "Cloud-IDE-JWT probe.invalid")
			resp, err := cl.Do(req)
			if err != nil {
				t.Fatalf("请求失败: %v", err)
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
			t.Logf("HTTP %s proto=%s body=%s", resp.Status, resp.Proto, strings.TrimSpace(string(body)))
		})
	}
}

var _ = fmt.Sprint
