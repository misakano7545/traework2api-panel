package main

import (
	"fmt"
	"strings"

	utls "github.com/refraction-networking/utls"
)

// genSpec 把抓到的 ClientHello 变成一段可直接编译的 utls ClientHelloSpec。
//
// 只对「内部含随机值」的扩展用专用类型，其余原样搬字节：
//   - key_share(51)：专用 —— 原样搬会把公钥钉死，每次都发同一把，等于自曝；
//   - supported_versions(43) / sni(0) / alpn(16) / groups(10) / sigalgs(13)：
//     专用 —— 它们是结构化的，用类型表达更清楚，也便于改；
//   - 其余一律 GenericExtension，保留原始字节。
//
// ECH(65037) 是特例：它内部带 HPKE 公钥与配置，原样搬等于复用同一把公钥。
// 生成时会标注出来，需要人工确认要不要保留。
func genSpec(ch *ClientHello) string {
	var sb strings.Builder

	sb.WriteString("// 由 tools/hellocap 生成。用它替掉 HelloChrome_133 之前，先跑一次\n")
	sb.WriteString("// TestSpecStillHandshakes 确认能真握手，再在真实上游过一遍。\n")
	sb.WriteString("var capturedSpec = &utls.ClientHelloSpec{\n")

	// 套件
	sb.WriteString("	CipherSuites: []uint16{\n		")
	first := true
	for _, v := range ch.CipherSuites {
		if isGREASE(v) {
			continue // GREASE 由 utls 自己插，写死值会打乱它的协议
		}
		if !first {
			sb.WriteString(" ")
		}
		first = false
		fmt.Fprintf(&sb, "0x%04x,", v)
	}
	sb.WriteString("\n\t},\n")

	// 压缩方法
	sb.WriteString("\tCompressionMethods: []byte{")
	for i, c := range ch.CompressionMethods {
		if i > 0 {
			sb.WriteString(", ")
		}
		fmt.Fprintf(&sb, "0x%02x", c)
	}
	sb.WriteString("},\n")

	// 扩展
	sb.WriteString("\tExtensions: []utls.TLSExtension{\n")
	for _, e := range ch.Extensions {
		if line, note := genExtension(e); line != "" {
			sb.WriteString("\t\t" + line + "\n")
			if note != "" {
				sb.WriteString("\t\t// " + note + "\n")
			}
		}
	}
	sb.WriteString("\t},\n}\n")

	return sb.String()
}

// genExtension 生成单条扩展的 Go 表达式，第二返回值是需要提醒的注释。
func genExtension(e Extension) (string, string) {
	switch e.Type {
	case extServerName:
		return `&utls.SNIExtension{},`, "运行时按目标域名填，不要写死"
	case extSupportedGroups:
		var ids []string
		for _, g := range extractU16List(e.Data) {
			if isGREASE(g) {
				continue
			}
			ids = append(ids, fmt.Sprintf("utls.CurveID(0x%04x)", g))
		}
		return fmt.Sprintf("&utls.SupportedCurvesExtension{Curves: []utls.CurveID{%s}},", strings.Join(ids, ", ")), ""
	case extSignatureAlgorithms:
		var ids []string
		for _, s := range extractU16List(e.Data) {
			if isGREASE(s) {
				continue
			}
			ids = append(ids, fmt.Sprintf("utls.SignatureScheme(0x%04x)", s))
		}
		return fmt.Sprintf("&utls.SignatureAlgorithmsExtension{SupportedSignatureAlgorithms: []utls.SignatureScheme{%s}},", strings.Join(ids, ", ")), ""
	case extALPN:
		var protos []string
		for _, p := range extractALPN(e.Data) {
			protos = append(protos, fmt.Sprintf("%q", p))
		}
		return fmt.Sprintf("&utls.ALPNExtension{AlpnProtocols: []string{%s}},", strings.Join(protos, ", ")), ""
	case extSupportedVersions:
		var vers []string
		// 注意：supported_versions 的前缀是 **1 字节**长度，不是 groups/sigalgs 那种 2 字节。
		// 用错会把 0x0304 读成 0x0403（第一版就踩了）。
		if len(e.Data) >= 1 {
			n := int(e.Data[0])
			end := 1 + n
			if end > len(e.Data) {
				end = len(e.Data)
			}
			for q := 1; q+2 <= end; q += 2 {
				v := uint16(e.Data[q])<<8 | uint16(e.Data[q+1])
				if isGREASE(v) {
					continue
				}
				vers = append(vers, fmt.Sprintf("0x%04x", v))
			}
		}
		return fmt.Sprintf("&utls.SupportedVersionsExtension{Versions: []uint16{%s}},", strings.Join(vers, ", ")), ""
	case extKeyShare:
		var ks []string
		for _, k := range extractKeyShares(e.Data) {
			if isGREASE(k.Group) {
				continue
			}
			// Data 留空：utls 会为每个 group 现生成密钥对。
			ks = append(ks, fmt.Sprintf("{Group: utls.CurveID(0x%04x)}", k.Group))
		}
		return fmt.Sprintf("&utls.KeyShareExtension{KeyShares: []utls.KeyShare{%s}},", strings.Join(ks, ", ")),
			"Data 故意留空 —— utls 每次现生成密钥，别把抓到的公钥钉进来"
	case 65037:
		// ECH 的 config 里带 HPKE 公钥。原样搬等于每次握手都发同一把公钥，
		// 比不发更糟（服务端/风控能看到固定值）。所以默认**注释掉**，要留得人工确认。
		return fmt.Sprintf("		// &utls.GenericExtension{Id: %d, Data: []byte{%s}},\n"+
				"		//   ↑ ECH：内含 HPKE 公钥（%d 字节），原样搬会每次发同一把。",
				e.Type, hexList(e.Data), len(e.Data)),
			"ECH 已注释：要保留需自行替换成 utls 的 ECH 构造或每次现生成"
	case 17613:
		var protos []string
		for _, p := range extractALPN(e.Data) {
			protos = append(protos, fmt.Sprintf("%q", p))
		}
		if len(protos) > 0 {
			return fmt.Sprintf("&utls.ApplicationSettingsExtension{SupportedProtocols: []string{%s}},", strings.Join(protos, ", ")), ""
		}
		return fmt.Sprintf("&utls.GenericExtension{Id: %d, Data: []byte{%s}},", e.Type, hexList(e.Data)), ""
	}
	if isGREASE(e.Type) {
		return "&utls.UtlsGREASEExtension{},", ""
	}
	return fmt.Sprintf("&utls.GenericExtension{Id: %d, Data: []byte{%s}},", e.Type, hexList(e.Data)), ""
}

// extractU16List 读「2 字节长度 + u16 数组」这种扩展体。
func extractU16List(d []byte) []uint16 {
	if len(d) < 2 {
		return nil
	}
	n := int(d[0])<<8 | int(d[1])
	end := 2 + n
	if end > len(d) {
		end = len(d)
	}
	var out []uint16
	for q := 2; q+2 <= end; q += 2 {
		out = append(out, uint16(d[q])<<8|uint16(d[q+1]))
	}
	return out
}

// extractALPN 读 ALPN 列表（2 字节长度 + 若干 1 字节长度字符串）。
func extractALPN(d []byte) []string {
	if len(d) < 2 {
		return nil
	}
	n := int(d[0])<<8 | int(d[1])
	end := 2 + n
	if end > len(d) {
		end = len(d)
	}
	var out []string
	for q := 2; q < end; {
		l := int(d[q])
		if q+1+l > len(d) {
			break
		}
		out = append(out, string(d[q+1:q+1+l]))
		q += 1 + l
	}
	return out
}

// extractKeyShares 读 key_share（2 字节长度 + 若干 {group, len, data}）。
func extractKeyShares(d []byte) []KeyShare {
	if len(d) < 2 {
		return nil
	}
	end := 2 + int(d[0])<<8 + int(d[1])
	if end > len(d) {
		end = len(d)
	}
	var out []KeyShare
	for q := 2; q+4 <= end; {
		g := uint16(d[q])<<8 | uint16(d[q+1])
		kl := int(d[q+2])<<8 | int(d[q+3])
		if q+4+kl > len(d) {
			break
		}
		out = append(out, KeyShare{Group: g, Data: d[q+4 : q+4+kl]})
		q += 4 + kl
	}
	return out
}

// hexList 把字节切成 0x.. 列表，每行 12 个，便于阅读。
func hexList(d []byte) string {
	var sb strings.Builder
	for i, b := range d {
		if i > 0 {
			sb.WriteString(",")
			if i%12 == 0 {
				sb.WriteString("\n\t\t\t")
			} else {
				sb.WriteString(" ")
			}
		}
		fmt.Fprintf(&sb, "0x%02x", b)
	}
	return sb.String()
}

var _ = utls.CurveID(0)
