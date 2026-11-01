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
//
// ID 与 Spec 二选一：ID 是 utls 预置档（HelloChrome_*），Spec 是按抓包复刻的自定义
// 形状（aha）。两者都会经过 specWithALPN 把 ALPN 收窄成 http/1.1。
type tlsFingerprint struct {
	ID   *utls.ClientHelloID
	Spec *utls.ClientHelloSpec
	Name string
}

var (
	tlsMu sync.RWMutex
	// 默认用 aha：它是官方客户端 aha/TTNet 网络栈的真实形状（抓包复刻），
	// 比 chrome_133 更贴近「这个客户端」。四个生产主机（含 api.trae.cn）实测全通。
	tlsFP = ahaFingerprint()
)

func chromeFingerprint(id utls.ClientHelloID) *tlsFingerprint {
	return &tlsFingerprint{ID: &id, Name: "chrome_" + id.Version}
}

// ahaFingerprint 官方客户端（Trae）自己的 ClientHello 形状。
//
// 比 chrome_133 更该做默认：它才是「这个客户端发的东西」。chrome_133 多出 PQ key share、
// ECH 与 ALPS 私有 id，在 JA3/JA4 上与真客户端对不上（对比见 tls_aha.go 的表）。
func ahaFingerprint() *tlsFingerprint {
	return &tlsFingerprint{Spec: ahaSpec, Name: "aha"}
}

// tlsFingerprints 可选的档位。名字进配置，值只在这里维护。
var tlsFingerprints = map[string]func() *tlsFingerprint{
	"aha":        ahaFingerprint,
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
	spec, err := specWithALPN(fp, alpnForWire())
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

// specWithALPN 取指定档位的 spec 并把 ALPN 换成指定列表。
//
// 每次调用都返回**新拷贝**：spec 会被 utls 在握手中改写（GREASE 值、key share 长度），
// 全局共享同一份会串味。extension 对象本身无状态，可以共用。
func specWithALPN(fp *tlsFingerprint, alpn []string) (*utls.ClientHelloSpec, error) {
	if fp == nil {
		return nil, fmt.Errorf("nil fingerprint")
	}
	var spec *utls.ClientHelloSpec
	if fp.Spec != nil {
		cp := *fp.Spec
		cp.Extensions = append([]utls.TLSExtension(nil), fp.Spec.Extensions...)
		cp.CipherSuites = append([]uint16(nil), fp.Spec.CipherSuites...)
		cp.CompressionMethods = append([]byte(nil), fp.Spec.CompressionMethods...)
		spec = &cp
	} else if fp.ID != nil {
		s, err := utls.UTLSIdToSpec(*fp.ID)
		if err != nil {
			return nil, err
		}
		spec = &s
	} else {
		return nil, fmt.Errorf("档位 %q 既没有 Spec 也没有 ID", fp.Name)
	}
	// ALPN 一律替换成新的扩展对象，避免共享 AlpnProtocols 切片。
	found := false
	for i := range spec.Extensions {
		if _, ok := spec.Extensions[i].(*utls.ALPNExtension); ok {
			spec.Extensions[i] = &utls.ALPNExtension{AlpnProtocols: append([]string(nil), alpn...)}
			found = true
		}
	}
	if !found && len(alpn) > 0 {
		spec.Extensions = append(spec.Extensions, &utls.ALPNExtension{AlpnProtocols: append([]string(nil), alpn...)})
	}
	// 有状态的扩展必须重建，不能复用同一对象：UtlsGREASEExtension 在握手时写入
	// 自己抽到的 value，UtlsPaddingExtension 会缓存算出来的长度 —— 多连接共用一个
	// 实例会串味，症状是随机的 `tls: internal error`。
	//
	// 只对自定义 Spec 做：ID 档位每次 UTLSIdToSpec 出来的本来就是新对象。
	if fp.Spec != nil {
		for i, e := range spec.Extensions {
			switch v := e.(type) {
			case *utls.UtlsGREASEExtension:
				spec.Extensions[i] = &utls.UtlsGREASEExtension{}
			case *utls.UtlsPaddingExtension:
				spec.Extensions[i] = &utls.UtlsPaddingExtension{GetPaddingLen: v.GetPaddingLen}
			case *utls.KeyShareExtension:
				shares := make([]utls.KeyShare, len(v.KeyShares))
				for j, ks := range v.KeyShares {
					shares[j] = utls.KeyShare{Group: ks.Group}
				}
				spec.Extensions[i] = &utls.KeyShareExtension{KeyShares: shares}
			}
		}
	}
	return spec, nil
}
