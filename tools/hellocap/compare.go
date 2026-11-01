package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	utls "github.com/refraction-networking/utls"
)

// chrome133Hello 让 utls 真发一次 ClientHello，抓下来解析。
//
// 不用 UTLSIdToSpec 直接读扩展列表：spec 里的扩展是 utls 的类型对象，
// 拿不到「线上到底是什么字节」。自己发一次、走同一条解析路径，diff 才可信。
func chrome133Hello() (*ClientHello, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	defer ln.Close()

	type res struct {
		ch  *ClientHello
		err error
	}
	out := make(chan res, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			out <- res{nil, err}
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		buf := make([]byte, 0, 4096)
		tmp := make([]byte, 4096)
		for {
			consumed, recs, _ := splitRecords(buf)
			hs := handshakeBytes(recs)
			if len(hs) >= 4 {
				bodyLen := int(hs[1])<<16 | int(hs[2])<<8 | int(hs[3])
				if len(hs) >= 4+bodyLen {
					_ = consumed
					ch, perr := ParseClientHello(hs)
					out <- res{ch, perr}
					return
				}
			}
			n, err := conn.Read(tmp)
			if n > 0 {
				buf = append(buf, tmp[:n]...)
			}
			if err != nil {
				out <- res{nil, err}
				return
			}
		}
	}()

	go func() {
		raw, err := (&net.Dialer{Timeout: 3 * time.Second}).DialContext(context.Background(), "tcp", ln.Addr().String())
		if err != nil {
			return
		}
		defer raw.Close()
		u := utls.UClient(raw, &utls.Config{ServerName: "probe.local", InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}, utls.HelloChrome_133)
		_ = u.Handshake()
	}()

	select {
	case r := <-out:
		return r.ch, r.err
	case <-time.After(6 * time.Second):
		return nil, fmt.Errorf("等 utls ClientHello 超时")
	}
}

// compareWithChrome133 输出「抓到的那条」与 utls HelloChrome_133 的结构差异。
//
// 判据只看结构，不比随机值：cipher 顺序、扩展顺序、扩展集合、group、sigalg。
// key_share 的公钥每次都不一样，只比「有几个、分别是哪个 group」。
func (ch *ClientHello) compareWithChrome133() string {
	ref, err := chrome133Hello()
	if err != nil {
		return fmt.Sprintf("（与 chrome_133 对比失败：%v）", err)
	}

	var sb strings.Builder
	sb.WriteString("----------- 与 utls HelloChrome_133 的结构差异 -----------\n")

	// 套件
	if diffs := diffStringSlice(toHex16(ch.CipherSuites), toHex16(ref.CipherSuites)); len(diffs) > 0 {
		sb.WriteString("套件差异：\n")
		for _, d := range diffs {
			sb.WriteString("  " + d + "\n")
		}
	} else {
		sb.WriteString("套件：完全一致\n")
	}

	// 扩展顺序与集合
	exp := extIDs(ch)
	rexp := extIDs(ref)
	if strings.Join(exp, ",") == strings.Join(rexp, ",") {
		sb.WriteString("扩展：类型与顺序完全一致\n")
	} else {
		sb.WriteString("扩展顺序差异：\n")
		sb.WriteString("  抓包: " + strings.Join(exp, " ") + "\n")
		sb.WriteString("  133 : " + strings.Join(rexp, " ") + "\n")
		if only := missingIn(exp, rexp); len(only) > 0 {
			sb.WriteString("  抓包有、133 没有: " + strings.Join(only, " ") + "\n")
		}
		if only := missingIn(rexp, exp); len(only) > 0 {
			sb.WriteString("  133 有、抓包没有: " + strings.Join(only, " ") + "\n")
		}
	}

	// groups / sigalgs
	if a, b := toDec16(ch.Groups), toDec16(ref.Groups); strings.Join(a, ",") != strings.Join(b, ",") {
		sb.WriteString("supported_groups 差异：\n  抓包: " + strings.Join(a, " ") + "\n  133 : " + strings.Join(b, " ") + "\n")
	} else {
		sb.WriteString("supported_groups：一致\n")
	}
	if a, b := toDec16(ch.SignatureAlgs), toDec16(ref.SignatureAlgs); strings.Join(a, ",") != strings.Join(b, ",") {
		sb.WriteString("signature_algorithms 差异：\n  抓包: " + strings.Join(a, " ") + "\n  133 : " + strings.Join(b, " ") + "\n")
	} else {
		sb.WriteString("signature_algorithms：一致\n")
	}

	// key share 只比 group 集合
	var kg, rkg []string
	for _, k := range ch.KeyShares {
		kg = append(kg, fmt.Sprintf("%d", k.Group))
	}
	for _, k := range ref.KeyShares {
		rkg = append(rkg, fmt.Sprintf("%d", k.Group))
	}
	if strings.Join(kg, ",") != strings.Join(rkg, ",") {
		sb.WriteString("key_share 组差异：\n  抓包: " + strings.Join(kg, " ") + "\n  133 : " + strings.Join(rkg, " ") + "\n")
	} else {
		sb.WriteString("key_share 组：一致\n")
	}

	// ALPN
	if strings.Join(ch.ALPN, ",") != strings.Join(ref.ALPN, ",") {
		sb.WriteString(fmt.Sprintf("ALPN 差异：抓包 %v / 133 %v\n", ch.ALPN, ref.ALPN))
	} else {
		sb.WriteString(fmt.Sprintf("ALPN：一致 %v\n", ch.ALPN))
	}

	sb.WriteString("\nJA3 抓包: " + mustJA3(ch) + "\n")
	sb.WriteString("JA3 133 : " + mustJA3(ref) + "\n")
	sb.WriteString("JA4 抓包: " + ch.JA4() + "\n")
	sb.WriteString("JA4 133 : " + ref.JA4() + "\n")
	return sb.String()
}

func mustJA3(ch *ClientHello) string {
	j, _ := ch.JA3()
	return j
}

func extIDs(ch *ClientHello) []string {
	var out []string
	for _, e := range ch.Extensions {
		out = append(out, fmt.Sprintf("%d", e.Type))
	}
	return out
}

func toHex16(v []uint16) []string {
	out := make([]string, 0, len(v))
	for _, x := range v {
		out = append(out, fmt.Sprintf("%04x", x))
	}
	return out
}

func toDec16(v []uint16) []string {
	out := make([]string, 0, len(v))
	for _, x := range v {
		out = append(out, fmt.Sprintf("%d", x))
	}
	return out
}

// missingIn 返回 a 里有、b 里没有的项（按 a 的顺序）。
func missingIn(a, b []string) []string {
	set := map[string]bool{}
	for _, x := range b {
		set[x] = true
	}
	var out []string
	for _, x := range a {
		if !set[x] {
			out = append(out, x)
		}
	}
	return out
}

// diffStringSlice 逐位比较，返回人类可读的差异行。
func diffStringSlice(a, b []string) []string {
	if len(a) != len(b) {
		return []string{fmt.Sprintf("长度不同：抓包 %d / 133 %d\n    抓包: %s\n    133 : %s",
			len(a), len(b), strings.Join(a, " "), strings.Join(b, " "))}
	}
	for i := range a {
		if a[i] != b[i] {
			return []string{fmt.Sprintf("第 %d 位起不同：\n    抓包: %s\n    133 : %s",
				i, strings.Join(a, " "), strings.Join(b, " "))}
		}
	}
	return nil
}

var _ = io.Discard
