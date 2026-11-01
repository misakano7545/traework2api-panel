// tls.go 出站 TLS 指纹：把 Go crypto/tls 的 ClientHello 换成浏览器系。
//
// 为什么需要这层：请求头是 HTTP 层的，TLS 握手在 HTTP 之前就完成了。用
// crypto/tls 发包，ClientHello 是「13 个套件 / 单 supported_group(x25519) /
// 无 GREASE / 10 个扩展」的结构；浏览器系（BoringSSL）是「多套件 / 多 group /
// 带 GREASE / 十几个扩展含 ALPS·ECH」。做过 JA4 的系统先看握手、再看头，
// 光对齐 header 在这一层是白给。
//
// 实测（本文件同目录的 TestTLSFingerprintDiffersFromGo 钉住）：
//
//	crypto/tls        JA3 = 771,49195-49199-…,0-11-65281-23-18-5-10-13-50-43-51,4587,0
//	utls Chrome_133   JA3 = 771,43690-4865-…,10794-35-23-13-65037-5-43-27-16-17613-51-45-11-18-65281-0-10-27242,4588,0
//
// 目标版本的现实：客户端抓包时 UA 是 Chrome/142.0.7444.235，而 utls v1.8.2 的
// 最高档是 HelloChrome_133。中间 9 个大版本的 ClientHello 差异主要在 PQ
// key share（X25519MLKEM768）的排布，133 已经带 PQ。**这不是逐字节一致，
// 只是从「脚本栈」变成「浏览器栈」** —— 想真正一致得自己按 142 抓包构造 spec，
// 而现在没有 142 的 ClientHello 样本（当年 MITM 终止了 TLS，没留握手字节）。
package upstream

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	utls "github.com/refraction-networking/utls"
)

// Fingerprint 出站 TLS 指纹档位。
type tlsFingerprint struct {
	ID   utls.ClientHelloID
	Name string
}

var (
	tlsMu sync.RWMutex
	tlsFP = chromeFingerprint(utls.HelloChrome_133) // 默认：utls 支持的最高 Chrome 档
)

func chromeFingerprint(id utls.ClientHelloID) *tlsFingerprint {
	return &tlsFingerprint{ID: id, Name: "chrome_" + id.Version}
}

// tlsFingerprints 可选的档位。名字进配置，值只在这里维护。
var tlsFingerprints = map[string]func() *tlsFingerprint{
	"chrome_133": func() *tlsFingerprint { return chromeFingerprint(utls.HelloChrome_133) },
	"chrome_131": func() *tlsFingerprint { return chromeFingerprint(utls.HelloChrome_131) },
	"chrome_120": func() *tlsFingerprint { return chromeFingerprint(utls.HelloChrome_120) },
	// "go" 保留原生握手，用于「疑似被 TLS 层盯上时回退」和对照实验。
	"go": func() *tlsFingerprint { return nil },
}

// TLSFingerprintName 当前生效的指纹名（面板展示用）。
func TLSFingerprintName() string {
	tlsMu.RLock()
	defer tlsMu.RUnlock()
	if tlsFP == nil {
		return "go"
	}
	return tlsFP.Name
}

// SetTLSFingerprint 切换出站 TLS 指纹。空字符串 = 恢复默认档。
func SetTLSFingerprint(name string) error {
	name = strings.ToLower(strings.TrimSpace(name))
	tlsMu.Lock()
	defer tlsMu.Unlock()
	if name == "" {
		tlsFP = chromeFingerprint(utls.HelloChrome_133)
		return nil
	}
	mk, ok := tlsFingerprints[name]
	if !ok {
		return fmt.Errorf("未知的 TLS 指纹 %q（可选：chrome_133 / chrome_131 / chrome_120 / go）", name)
	}
	tlsFP = mk()
	return nil
}

// currentTLSFingerprint 取当前档位快照（请求路径并发读）。
func currentTLSFingerprint() *tlsFingerprint {
	tlsMu.RLock()
	defer tlsMu.RUnlock()
	return tlsFP
}

// alpnForWire 出站 ALPN。
//
// 客户端是 HTTP/1.1（抓包里带 Connection: keep-alive），而本仓关掉了 h2
// （见 newWireTransport）。浏览器默认 offer 的是 h2 + http/1.1，真协商到 h2
// 就会用 h1 报文去说 h2，直接坏掉 —— 所以必须收窄成只有 http/1.1。
//
// 代价：JA4 的 ALPN 段与真浏览器不同（Chrome 是 h2,http/1.1）。这是「能跑」
// 与「完全一致」之间必须选一个，功能优先。
func alpnForWire() []string { return []string{"http/1.1"} }

// dialTLSWithFingerprint 用指定指纹建一条已握手的 TLS 连接。
//
// fp 为 nil 时走 Go 原生 crypto/tls（此时由 http.Transport 自己的
// DialTLSContext 逻辑处理更合适，这条分支只给测试与回退兜底用）。
func dialTLSWithFingerprint(ctx context.Context, network, addr string, fp *tlsFingerprint, insecure bool) (net.Conn, error) {
	raw, err := (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext(ctx, network, addr)
	if err != nil {
		return nil, err
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	if fp == nil {
		c := tls.Client(raw, &tls.Config{ServerName: host, InsecureSkipVerify: insecure, MinVersion: tls.VersionTLS12})
		if err := c.HandshakeContext(ctx); err != nil {
			raw.Close()
			return nil, err
		}
		return c, nil
	}
	spec, err := specWithALPN(fp.ID, alpnForWire())
	if err != nil {
		raw.Close()
		return nil, err
	}
	cfg := &utls.Config{
		ServerName:         host,
		InsecureSkipVerify: insecure,
		MinVersion:         tls.VersionTLS12,
	}
	u := utls.UClient(raw, cfg, utls.HelloCustom)
	if err := u.ApplyPreset(spec); err != nil {
		raw.Close()
		return nil, err
	}
	if err := u.HandshakeContext(ctx); err != nil {
		raw.Close()
		return nil, err
	}
	return u, nil
}

// specWithALPN 取预置 spec 并把 ALPN 换成指定列表。
func specWithALPN(id utls.ClientHelloID, alpn []string) (*utls.ClientHelloSpec, error) {
	spec, err := utls.UTLSIdToSpec(id)
	if err != nil {
		return nil, err
	}
	found := false
	for i := range spec.Extensions {
		if a, ok := spec.Extensions[i].(*utls.ALPNExtension); ok {
			a.AlpnProtocols = alpn
			found = true
		}
	}
	if !found {
		spec.Extensions = append(spec.Extensions, &utls.ALPNExtension{AlpnProtocols: alpn})
	}
	return &spec, nil
}
