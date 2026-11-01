package main

import (
	"crypto/tls"
	"fmt"
	"net"
	"testing"
	"time"

	utls "github.com/refraction-networking/utls"
)

// buildSpec 把抓到的 ClientHello 变成 utls 能直接用的 spec 对象。
//
// 与 genSpec 是同一套语义的两个产物：genSpec 给人看/贴进代码，buildSpec 给测试
// 真握手用。分开写是有意的 —— 生成字符串没法在测试里编译，但对象可以，这样
// 「生成的 spec 到底能不能握手」就有了可执行的验证，而不是靠肉眼。
func buildSpec(ch *ClientHello) *utls.ClientHelloSpec {
	spec := &utls.ClientHelloSpec{
		CompressionMethods: ch.CompressionMethods,
	}
	for _, v := range ch.CipherSuites {
		if isGREASE(v) {
			continue
		}
		spec.CipherSuites = append(spec.CipherSuites, v)
	}
	for _, e := range ch.Extensions {
		if ext := buildExtension(e); ext != nil {
			spec.Extensions = append(spec.Extensions, ext)
		}
	}
	return spec
}

func buildExtension(e Extension) utls.TLSExtension {
	switch e.Type {
	case extServerName:
		return &utls.SNIExtension{}
	case extSupportedGroups:
		var cs []utls.CurveID
		for _, g := range extractU16List(e.Data) {
			if !isGREASE(g) {
				cs = append(cs, utls.CurveID(g))
			}
		}
		return &utls.SupportedCurvesExtension{Curves: cs}
	case extSignatureAlgorithms:
		var ss []utls.SignatureScheme
		for _, s := range extractU16List(e.Data) {
			if !isGREASE(s) {
				ss = append(ss, utls.SignatureScheme(s))
			}
		}
		return &utls.SignatureAlgorithmsExtension{SupportedSignatureAlgorithms: ss}
	case extALPN:
		return &utls.ALPNExtension{AlpnProtocols: extractALPN(e.Data)}
	case extSupportedVersions:
		var vs []uint16
		if len(e.Data) >= 1 {
			end := 1 + int(e.Data[0])
			if end > len(e.Data) {
				end = len(e.Data)
			}
			for q := 1; q+2 <= end; q += 2 {
				v := uint16(e.Data[q])<<8 | uint16(e.Data[q+1])
				if !isGREASE(v) {
					vs = append(vs, v)
				}
			}
		}
		return &utls.SupportedVersionsExtension{Versions: vs}
	case extKeyShare:
		var ks []utls.KeyShare
		for _, k := range extractKeyShares(e.Data) {
			if !isGREASE(k.Group) {
				ks = append(ks, utls.KeyShare{Group: utls.CurveID(k.Group)})
			}
		}
		return &utls.KeyShareExtension{KeyShares: ks}
	}
	if isGREASE(e.Type) {
		return &utls.UtlsGREASEExtension{}
	}
	return &utls.GenericExtension{Id: e.Type, Data: e.Data}
}

// 生成的 spec 必须能真握手 —— 这是整套工具唯一有意义的验收标准。
func TestBuiltSpecStillHandshakes(t *testing.T) {
	ch, err := chrome133Hello()
	if err != nil {
		t.Fatalf("取参考 ClientHello 失败: %v", err)
	}
	spec := buildSpec(ch)

	srv := httptestTLSServer(t)
	defer srv.Close()
	addr := srv.Listener.Addr().String()

	raw, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()

	u := utls.UClient(raw, &utls.Config{ServerName: "127.0.0.1", InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}, utls.HelloCustom)
	if err := u.ApplyPreset(spec); err != nil {
		t.Fatalf("ApplyPreset 失败: %v", err)
	}
	if err := u.Handshake(); err != nil {
		t.Fatalf("用生成的 spec 握手失败: %v", err)
	}
	if got := u.ConnectionState().Version; got != tls.VersionTLS13 {
		t.Errorf("协商版本=0x%04x，期望 TLS1.3", got)
	}
}

// genSpec 的文本要包含关键片段，且不能出现读错的版本号。
func TestGenSpecText(t *testing.T) {
	ch, err := chrome133Hello()
	if err != nil {
		t.Fatal(err)
	}
	code := genSpec(ch)
	for _, want := range []string{
		"var capturedSpec = &utls.ClientHelloSpec{",
		"CipherSuites:",
		"&utls.KeyShareExtension{",
		"&utls.SupportedVersionsExtension{",
	} {
		if !contains(code, want) {
			t.Errorf("生成的 spec 缺少 %q", want)
		}
	}
	// supported_versions 那一行必须含 0x0304。注意不能全文件搜 "0x0403" 判错：
	// 0x0403 是合法的签名算法（ecdsa_secp256r1_sha256），头一版就误伤过。
	var svLine string
	for _, line := range splitLines(code) {
		if contains(line, "SupportedVersionsExtension") {
			svLine = line
		}
	}
	if svLine == "" {
		t.Fatal("生成的 spec 里没有 SupportedVersionsExtension")
	}
	if !contains(svLine, "0x0304") {
		t.Errorf("supported_versions 字节序读反了：%s", svLine)
	}
	if contains(svLine, "0x0403") {
		t.Errorf("supported_versions 里出现了 0x0403（0x0304 的高低位读反）：%s", svLine)
	}
}

func splitLines(s string) []string {
	var out []string
	cur := ""
	for _, r := range s {
		if r == '\n' {
			out = append(out, cur)
			cur = ""
			continue
		}
		cur += string(r)
	}
	out = append(out, cur)
	return out
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}

func httptestTLSServer(t *testing.T) *tlsTestServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cert, err := selfSignedCert()
	if err != nil {
		t.Fatal(err)
	}
	s := &tlsTestServer{Listener: ln, cert: cert}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				tc := tls.Server(c, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
				_ = tc.Handshake()
			}(c)
		}
	}()
	return s
}

type tlsTestServer struct {
	net.Listener
	cert tls.Certificate
}

func (s *tlsTestServer) Close() error { return s.Listener.Close() }

// selfSignedCert 生成一张临时自签证书，只为让握手能走完。
func selfSignedCert() (tls.Certificate, error) {
	return tls.X509KeyPair(localCertPEM, localKeyPEM)
}

var localCertPEM = []byte(`-----BEGIN CERTIFICATE-----
MIIBhTCCASugAwIBAgIQIRi6zePL6mKjOipn+dNuaTAKBggqhkjOPQQDAjASMRAw
DgYDVQQKEwdBY21lIENvMB4XDTE3MTAyMDE5NDMwNloXDTE4MTAyMDE5NDMwNlow
EjEQMA4GA1UEChMHQWNtZSBDbzBZMBMGByqGSM49AgEGCCqGSM49AwEHA0IABD0d
7VNhbWvZLWPuj/RtHFjvtJBEwOkhbN/BnnE8rnZR8+sbwnc/KhCk3FhnpHZnQz7B
5aETbbIgmuvewdjvSBSjYzBhMA4GA1UdDwEB/wQEAwICpDATBgNVHSUEDDAKBggr
BgEFBQcDATAPBgNVHRMBAf8EBTADAQH/MCkGA1UdEQQiMCCCDmxvY2FsaG9zdDo1
NDUzgg4xMjcuMC4wLjE6NTQ1MzAKBggqhkjOPQQDAgNIADBFAiEA2zpJEPQyz6/l
Wf86aX6PepsntZv2GYlA5UpabfT2EZICICpJ5h/iI+i341gBmLiAFQOyTDT+/wQc
6MF9+Yw1Yy0t
-----END CERTIFICATE-----`)
var localKeyPEM = []byte(`-----BEGIN EC PRIVATE KEY-----
MHcCAQEEIIrYSSNQFaA2Hwf1duRSxKtLYX5CB04fSeQ6tF1aY/PuoAoGCCqGSM49
AwEHoUQDQgAEPR3tU2Fta9ktY+6P9G0cWO+0kETA6SFs38GecTyudlHz6xvCdz8q
EKTcWGekdmdDPsHloRNtsiCa697B2O9IFA==
-----END EC PRIVATE KEY-----`)

var _ = fmt.Sprint
