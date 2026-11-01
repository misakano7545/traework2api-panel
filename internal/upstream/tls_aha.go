// tls_aha.go 官方客户端（aha/TTNet 网络栈）的真实 ClientHello spec。
//
// 来源：2026-11-01 用 tools/hellocap 抓本机 Trae CN 客户端的线上握手
// （SNI api.trae.com.cn，517 字节）。它是 aha 原生网络栈发的，**不是 Chromium**，
// 和 utls 的 HelloChrome_* 有实质差别：
//
//	                 ciphers  groups              sigalgs  备注
//	aha/TTNet(本文件) 15       29 / 23 / 24        8        无 PQ、带 17513 与 padding
//	Chrome 133       15       56026 / 4588 / 29…  7        带 PQ、ECH、ALPS
//	真 Chromium 抓包  15       4588 / 29 / 23 / 24 12       带 PQ、ECH、ALPS、私有扩展
//
// 所以「用 Chrome 指纹冒充」在 JA3/JA4 上是**对不上**的：本仓此前的
// chrome_133 档位比客户端多出整个 PQ key share 与 ECH，groups 集合也不同。
// 这个 spec 才是 Trae 自己的形状。
//
// 生成参数：每条扩展都按抓包的原始字节搬，只有四个例外（见各条注释）。
// 换客户端版本后重新抓一次、重新生成，别手改。
package upstream

import (
	utls "github.com/refraction-networking/utls"
)

// ahaSpec 官方客户端 aha 网络栈的 ClientHello。
//
// 用 utls.HelloCustom + 这个 spec 建连，见 tls.go 的 dialTLSWithFingerprint。
var ahaSpec = &utls.ClientHelloSpec{
	CipherSuites: []uint16{
		// 首位是 GREASE 占位（抓包里是 0x7a7a）。utls 会把 0x?a?a 形态换成每次随机的值，
		// 所以这里写任一 GREASE 形态即可，别写抓到的那个具体值。
		0x0a0a,
		0x1301, 0x1302, 0x1303, 0xc02b, 0xc02f, 0xc02c, 0xc030,
		0xcca9, 0xcca8, 0xc013, 0xc014, 0x009c, 0x009d, 0x002f, 0x0035,
	},
	CompressionMethods: []byte{0x00},
	TLSVersMax:         utls.VersionTLS13,
	TLSVersMin:         utls.VersionTLS12,
	Extensions: []utls.TLSExtension{
		// 0xeaea / 0xcaca / 0xaaaa 之类 GREASE 由 utls 每次随机生成，
		// 抓到的具体值不复用（复用反而是稳定特征）。
		&utls.UtlsGREASEExtension{},
		&utls.SNIExtension{},
		&utls.GenericExtension{Id: 23, Data: []byte{}},      // extended_master_secret
		&utls.GenericExtension{Id: 65281, Data: []byte{0x00}}, // renegotiation_info
		// 三条曲线：x25519 / secp256r1 / secp384r1。没有 PQ 混合曲线，这是 aha 的特征。
		&utls.SupportedCurvesExtension{Curves: []utls.CurveID{
			utls.X25519, utls.CurveID(0x0017), utls.CurveID(0x0018),
		}},
		&utls.GenericExtension{Id: 11, Data: []byte{0x01, 0x00}}, // ec_point_formats: uncompressed
		&utls.GenericExtension{Id: 35, Data: []byte{}},            // session_ticket
		// ALPN 在抓包里是 h2 + http/1.1。本仓关了 h2（见 wire.go），
		// 所以运行时会被 specWithALPN 覆盖成只有 http/1.1 —— 这是唯一的有意偏差，
		// 理由见 tls.go 的 alpnForWire。
		&utls.ALPNExtension{AlpnProtocols: []string{"h2", "http/1.1"}},
		&utls.GenericExtension{Id: 5, Data: []byte{0x01, 0x00, 0x00, 0x00, 0x00}}, // status_request
		&utls.SignatureAlgorithmsExtension{SupportedSignatureAlgorithms: []utls.SignatureScheme{
			0x0403, 0x0804, 0x0401, 0x0503, 0x0805, 0x0501, 0x0806, 0x0601,
		}},
		&utls.GenericExtension{Id: 18, Data: []byte{}}, // signed_certificate_timestamp
		// 只带 x25519 的 key share。utls 每次现生成密钥，抓到的公钥不复用。
		&utls.KeyShareExtension{KeyShares: []utls.KeyShare{{Group: utls.X25519}}},
		&utls.GenericExtension{Id: 45, Data: []byte{0x01, 0x01}}, // psk_key_exchange_modes: psk_dhe_ke
		&utls.SupportedVersionsExtension{Versions: []uint16{utls.VersionTLS13, utls.VersionTLS12}},
		&utls.GenericExtension{Id: 27, Data: []byte{0x02, 0x00, 0x02}}, // compress_certificate: brotli
		// 17513 = application_settings（ALPS 的旧 id，Chromium 用 17613）。值是 "h2"。
		&utls.GenericExtension{Id: 17513, Data: []byte{0x00, 0x03, 0x02, 0x68, 0x32}},
		&utls.UtlsGREASEExtension{},
		// padding：抓包里是 220 字节固定零，而且是扩展列表的最后一条。
		//
		// 早先这里有段错误结论：加上 padding 后 api.trae.cn 回 handshake failure，
		// 一度被判成「Tengine 不认 padding」。后来的 TestBisectTraeCN 把每个变体单独
		// 跑一遍，padding 固定 220B 与 utls 自动档**都能握手成功** —— 真正的元凶是
		// specWithALPN 里对 ID 档位也做了 KeyShare 重建，把 spec 改坏了。修掉之后
		// padding 照抓包补回来。
		&utls.GenericExtension{Id: 21, Data: make([]byte, 220)},
	},
}
