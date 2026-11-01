//go:build tlsprobe

// api.trae.cn 拒绝 aha 形状的二分定位。
//
// 背景：aha 档在 api5-normal(.alisg).mchost.guru 与 growsg-normal.trae.ai 上都握手
// 成功，唯独 api.trae.cn（阿里 Tengine）回 handshake failure。而 ug 族（签到/积分/
// 额度）全走那台，所以定位清楚之前 aha 不能做默认档。
//
//	go test -tags tlsprobe ./internal/upstream -run TestBisectTraeCN -v
package upstream

import (
	"context"
	"crypto/tls"
	"net"
	"testing"
	"time"

	utls "github.com/refraction-networking/utls"
)

// variantSpec 复制 ahaSpec 并按 mut 改一份，返回可直接用于握手的档位。
func variantSpec(name string, mut func(*utls.ClientHelloSpec)) *tlsFingerprint {
	cp := *ahaSpec
	cp.CipherSuites = append([]uint16(nil), ahaSpec.CipherSuites...)
	cp.Extensions = append([]utls.TLSExtension(nil), ahaSpec.Extensions...)
	if mut != nil {
		mut(&cp)
	}
	return &tlsFingerprint{Spec: &cp, Name: name}
}

// dropExt 从 spec 里删掉指定 id 的扩展。
func dropExt(spec *utls.ClientHelloSpec, id uint16) {
	var kept []utls.TLSExtension
	for _, e := range spec.Extensions {
		if g, ok := e.(*utls.GenericExtension); ok && g.Id == id {
			continue
		}
		kept = append(kept, e)
	}
	spec.Extensions = kept
}

// handshakeOnce 对 host 握一次手，返回错误（nil = 成功）。fp 为 nil 走 Go 原生。
func handshakeOnce(host string, fp *tlsFingerprint) error {
	addr := host + ":443"
	if fp == nil {
		c, err := tls.DialWithDialer(&net.Dialer{Timeout: 8 * time.Second}, "tcp", addr,
			&tls.Config{ServerName: host, MinVersion: tls.VersionTLS12})
		if err != nil {
			return err
		}
		return c.Close()
	}
	raw, err := (&net.Dialer{Timeout: 8 * time.Second}).DialContext(context.Background(), "tcp", addr)
	if err != nil {
		return err
	}
	defer raw.Close()

	spec, err := specWithALPN(fp, alpnForWire())
	if err != nil {
		return err
	}
	cfg := &utls.Config{ServerName: host, MinVersion: 0x0303}
	conn := utls.UClient(raw, cfg, utls.HelloCustom)
	if err := conn.ApplyPreset(spec); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return conn.HandshakeContext(ctx)
}

// TestAhaStabilityTraeCN 对 api.trae.cn 连打多次，统计 aha 档的成功率。
//
// 单次结果不可信：这台网关（阿里 Tengine）在连续握手时会抖，同一条 spec 前一秒
// 成功、后一秒 handshake failure。要判断「某条扩展是不是被拒」，必须看成功率，
// 不能看单次。
func TestAhaStabilityTraeCN(t *testing.T) {
	const host = "api.trae.cn"
	const rounds = 6

	cases := []struct {
		name string
		fp   *tlsFingerprint
	}{
		{"aha(带 padding)", ahaFingerprint()},
		{"aha(去 padding)", variantSpec("no-pad", func(s *utls.ClientHelloSpec) {
			var kept []utls.TLSExtension
			for _, e := range s.Extensions {
				if g, ok := e.(*utls.GenericExtension); ok && g.Id == 21 {
					continue
				}
				kept = append(kept, e)
			}
			s.Extensions = kept
		})},
		{"chrome_133", chromeFingerprint(utls.HelloChrome_133)},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ok, fail := 0, 0
			var lastErr error
			for i := 0; i < rounds; i++ {
				if err := handshakeOnce(host, c.fp); err != nil {
					fail++
					lastErr = err
				} else {
					ok++
				}
			}
			t.Logf("成功 %d/%d，最后一次错误: %v", ok, rounds, lastErr)
		})
	}
}
func TestBisectTraeCN(t *testing.T) {
	const host = "api.trae.cn"

	variants := []struct {
		name string
		fp   *tlsFingerprint
	}{
		{"baseline(aha)", ahaFingerprint()},
		{"去 17513(ALPS)", variantSpec("no-alps", func(s *utls.ClientHelloSpec) { dropExt(s, 17513) })},
		{"去 27(compress_cert)", variantSpec("no-cc", func(s *utls.ClientHelloSpec) { dropExt(s, 27) })},
		{"去 5(status_request)", variantSpec("no-sr", func(s *utls.ClientHelloSpec) { dropExt(s, 5) })},
		{"去 18(SCT)", variantSpec("no-sct", func(s *utls.ClientHelloSpec) { dropExt(s, 18) })},
		{"去 11(point_formats)", variantSpec("no-pf", func(s *utls.ClientHelloSpec) { dropExt(s, 11) })},
		{"groups 加 4588", variantSpec("with-pq", func(s *utls.ClientHelloSpec) {
			for _, e := range s.Extensions {
				if c, ok := e.(*utls.SupportedCurvesExtension); ok {
					c.Curves = append([]utls.CurveID{utls.CurveID(0x11ec)}, c.Curves...)
				}
			}
		})},
		{"sigalgs 补到 12 个", variantSpec("more-sig", func(s *utls.ClientHelloSpec) {
			for _, e := range s.Extensions {
				if sa, ok := e.(*utls.SignatureAlgorithmsExtension); ok {
					sa.SupportedSignatureAlgorithms = []utls.SignatureScheme{
						0x0a0a, 0x0904, 0x0905, 0x0906, 0x0403, 0x0804, 0x0401,
						0x0503, 0x0805, 0x0501, 0x0806, 0x0601,
					}
				}
			}
		})},
		{"去 GREASE 扩展", variantSpec("no-grease", func(s *utls.ClientHelloSpec) {
			var kept []utls.TLSExtension
			for _, e := range s.Extensions {
				if _, ok := e.(*utls.UtlsGREASEExtension); ok {
					continue
				}
				kept = append(kept, e)
			}
			s.Extensions = kept
		})},
		{"去首个 cipher GREASE", variantSpec("no-cipher-grease", func(s *utls.ClientHelloSpec) {
			if len(s.CipherSuites) > 0 && isGREASE(s.CipherSuites[0]) {
				s.CipherSuites = s.CipherSuites[1:]
			}
		})},
		{"对照 chrome_133", chromeFingerprint(utls.HelloChrome_133)},
		{"对照 crypto/tls", nil},
		// 抓包里 padding 是 220 字节固定零，而 utls 的 BoringPaddingStyle 会按包长
		// 补到 512 的倍数。两者长度不同 —— 试固定 220 能不能让 Tengine 接受。
		{"padding 固定 220B", variantSpec("pad220", func(s *utls.ClientHelloSpec) {
			s.Extensions = append(s.Extensions, &utls.GenericExtension{Id: 21, Data: make([]byte, 220)})
		})},
		{"padding utls 自动", variantSpec("pad-auto", func(s *utls.ClientHelloSpec) {
			s.Extensions = append(s.Extensions, &utls.UtlsPaddingExtension{GetPaddingLen: utls.BoringPaddingStyle})
		})},
	}

	for _, v := range variants {
		t.Run(v.name, func(t *testing.T) {
			start := time.Now()
			err := handshakeOnce(host, v.fp)
			dur := time.Since(start)
			if err != nil {
				t.Logf("失败(%v): %v", dur, err)
				return
			}
			t.Logf("握手 OK %v", dur)
		})
	}
}
