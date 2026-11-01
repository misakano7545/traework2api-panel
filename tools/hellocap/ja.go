package main

import (
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
)

// isGREASE 判断是否为 GREASE 占位值（0x?a?a，高低字节相同且低半字节是 a）。
func isGREASE(v uint16) bool {
	hi, lo := byte(v>>8), byte(v)
	return hi == lo && lo&0x0f == 0x0a
}

// JA3 是老的指纹：version,ciphers,extensions,groups,point_formats 的 md5。
// 保留它是因为大部分报告和数据还在用它，而且好算好核对。
func (ch *ClientHello) JA3() (string, string) {
	var cs, ex, gr, pf []string
	for _, v := range ch.CipherSuites {
		cs = append(cs, strconv.Itoa(int(v)))
	}
	for _, e := range ch.Extensions {
		ex = append(ex, strconv.Itoa(int(e.Type)))
	}
	for _, g := range ch.Groups {
		gr = append(gr, strconv.Itoa(int(g)))
	}
	for _, e := range ch.Extensions {
		if e.Type != extPointFormats || len(e.Data) < 1 {
			continue
		}
		n := int(e.Data[0])
		for i := 0; i < n && 1+i < len(e.Data); i++ {
			pf = append(pf, strconv.Itoa(int(e.Data[1+i])))
		}
	}
	raw := strings.Join([]string{
		strconv.Itoa(int(ch.LegacyVersion)),
		strings.Join(cs, "-"),
		strings.Join(ex, "-"),
		strings.Join(gr, "-"),
		strings.Join(pf, "-"),
	}, ",")
	sum := md5.Sum([]byte(raw))
	return hex.EncodeToString(sum[:]), raw
}

// ja4Version 把 ClientHello 的最高 supported_version 映射成 JA4 的两字符版本。
//
// 必须跳过 GREASE：Chrome 系会在 supported_versions 里塞 0xbaba 这种占位值，
// 不过滤的话最大值会变成 47770，版本直接算成 "00"（第一版就踩了这个）。
func (ch *ClientHello) ja4Version() string {
	best := ch.LegacyVersion
	if isGREASE(best) {
		best = 0
	}
	for _, v := range ch.SupportedVersions {
		if isGREASE(v) {
			continue
		}
		if v > best {
			best = v
		}
	}
	switch best {
	case 0x0304:
		return "13"
	case 0x0303:
		return "12"
	case 0x0302:
		return "11"
	case 0x0301:
		return "10"
	case 0x0300:
		return "s3"
	case 0x0002:
		return "s2"
	case 0x0001:
		return "s1"
	}
	return "00"
}

// JA4 按 foxio 的规范算（TCP/TLS 分支）。
//
//	a = t + 版本 + SNI 有无 + 套件数 + 扩展数 + ALPN 首末字符
//	b = sha256(去 GREASE 的套件，4 位 hex 逗号串)[:12]
//	c = sha256(去 GREASE/SNI 的扩展 + "_" + 签名算法)[:12]
//
// 计数都按「去 GREASE 之后」算，这是 JA4 与 JA3 最容易算错的地方。
func (ch *ClientHello) JA4() string {
	// 套件
	var ciphers []string
	for _, v := range ch.CipherSuites {
		if isGREASE(v) {
			continue
		}
		ciphers = append(ciphers, fmt.Sprintf("%04x", v))
	}
	// 扩展：JA4 规定 SNI 不计入扩展数
	var exts []string
	for _, e := range ch.Extensions {
		if e.Type == extServerName || isGREASE(e.Type) {
			continue
		}
		exts = append(exts, fmt.Sprintf("%04x", e.Type))
	}
	// 签名算法
	var sigalgs []string
	for _, s := range ch.SignatureAlgs {
		if isGREASE(s) {
			continue
		}
		sigalgs = append(sigalgs, fmt.Sprintf("%04x", s))
	}

	sni := "i"
	if ch.ServerName != "" {
		sni = "d"
	}
	alpn := "00"
	if len(ch.ALPN) > 0 {
		a := ch.ALPN[0]
		alpn = string(a[0]) + string(a[len(a)-1])
	}

	h := func(parts ...string) string {
		sum := sha256.Sum256([]byte(strings.Join(parts, ",")))
		return hex.EncodeToString(sum[:])[:12]
	}
	b := h(ciphers...)
	c := h(strings.Join(exts, ","), strings.Join(sigalgs, ","))

	return fmt.Sprintf("t%s%s%02d%02d%s_%s_%s",
		ch.ja4Version(), sni, len(ciphers), len(exts), alpn, b, c)
}

// extName 扩展 ID 的可读名，报告里用。
func extName(t uint16) string {
	switch t {
	case 0:
		return "server_name"
	case 5:
		return "status_request"
	case 10:
		return "supported_groups"
	case 11:
		return "ec_point_formats"
	case 13:
		return "signature_algorithms"
	case 16:
		return "ALPN"
	case 18:
		return "signed_certificate_timestamp"
	case 21:
		return "padding"
	case 22:
		return "encrypt_then_mac"
	case 23:
		return "extended_master_secret"
	case 27:
		return "compress_certificate"
	case 28:
		return "record_size_limit"
	case 34:
		return "delegated_credentials"
	case 35:
		return "session_ticket"
	case 41:
		return "pre_shared_key"
	case 43:
		return "supported_versions"
	case 44:
		return "cookie"
	case 45:
		return "psk_key_exchange_modes"
	case 49:
		return "post_handshake_auth"
	case 50:
		return "signature_algorithms_cert"
	case 51:
		return "key_share"
	case 65281:
		return "renegotiation_info"
	case 65037:
		return "encrypted_client_hello"
	case 17613:
		return "application_settings(ALPS)"
	}
	if isGREASE(t) {
		return fmt.Sprintf("GREASE(0x%04x)", t)
	}
	return fmt.Sprintf("0x%04x", t)
}
