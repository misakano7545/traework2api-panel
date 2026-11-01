// Package main 是 ClientHello 抓取桩：把真实客户端（Trae/Electron）握手第一步的
// 原始字节落盘并解析，用于复刻它的 TLS 指纹。
//
// 为什么需要它：本仓的 TLS 指纹目前用 utls 的 HelloChrome_133 顶替，而客户端抓包时
// UA 是 Chrome/142.0.7444.235。133 与 142 的 ClientHello 不是一回事（PQ key share 的
// 排布改过），但 2026-10-30 那次 MITM 终止了 TLS，只留了明文 HTTP，没留握手字节 ——
// 所以这层一直是「像浏览器」而不是「像这个客户端」。
//
// 抓 ClientHello 不需要解密、不需要证书：握手第一步就能断开。
package main

import (
	"encoding/binary"
	"fmt"
)

// 常用扩展 ID。
const (
	extServerName           = 0
	extSupportedGroups      = 10
	extPointFormats         = 11
	extSignatureAlgorithms  = 13
	extALPN                 = 16
	extPadding              = 21
	extExtendedMasterSecret = 23
	extCompressCertificate  = 27
	extSessionTicket        = 35
	extSupportedVersions    = 43
	extPSKKeyExchangeModes  = 45
	extKeyShare             = 51
	extRenegotiationInfo    = 65281
)

// Extension 一条原始扩展（类型 + 未解析的数据）。
type Extension struct {
	Type uint16
	Data []byte
}

// KeyShare 一组 key share 条目。
type KeyShare struct {
	Group uint16
	Data  []byte
}

// ClientHello 解析后的握手首包。
type ClientHello struct {
	LegacyVersion      uint16
	Random             []byte
	SessionID          []byte
	CipherSuites       []uint16
	CompressionMethods []byte
	Extensions         []Extension

	// 从扩展里解出来的常用字段（解析失败保持零值，不影响原始数据落盘）。
	ServerName        string
	ALPN              []string
	Groups            []uint16
	SignatureAlgs     []uint16
	KeyShares         []KeyShare
	SupportedVersions []uint16
}

// tlsRecord 从字节流里切出 TLS record。
type tlsRecord struct {
	Type    uint8
	Version uint16
	Data    []byte
}

// splitRecords 把裸字节流切成 TLS record 列表。
//
// 返回已消费的字节数与记录列表：客户端可能一次写出多条 record
// （ClientHello + 早期数据），也可能只写了一半，所以调用方要能判断「是否读够」。
func splitRecords(b []byte) (int, []tlsRecord, error) {
	var out []tlsRecord
	off := 0
	for off+5 <= len(b) {
		typ := b[off]
		ver := binary.BigEndian.Uint16(b[off+1:])
		n := int(binary.BigEndian.Uint16(b[off+3:]))
		if n > 1<<14+2048 {
			return off, out, fmt.Errorf("record 长度异常: %d", n)
		}
		if off+5+n > len(b) {
			break // 还没读全，等下一批
		}
		out = append(out, tlsRecord{Type: typ, Version: ver, Data: b[off+5 : off+5+n]})
		off += 5 + n
	}
	return off, out, nil
}

// handshakeBytes 把若干 handshake record 的载荷拼回连续的 handshake 流。
func handshakeBytes(recs []tlsRecord) []byte {
	var out []byte
	for _, r := range recs {
		if r.Type == 22 { // handshake
			out = append(out, r.Data...)
		}
	}
	return out
}

// ParseClientHello 解析一条完整的 ClientHello（输入是 record 拼接后的字节流）。
func ParseClientHello(stream []byte) (*ClientHello, error) {
	if len(stream) < 4 {
		return nil, fmt.Errorf("handshake 流太短: %d", len(stream))
	}
	if stream[0] != 1 {
		return nil, fmt.Errorf("不是 ClientHello（handshake type=%d）", stream[0])
	}
	bodyLen := int(stream[1])<<16 | int(stream[2])<<8 | int(stream[3])
	if 4+bodyLen > len(stream) {
		return nil, fmt.Errorf("ClientHello 不完整: 需要 %d，实际 %d", 4+bodyLen, len(stream))
	}
	b := stream[4 : 4+bodyLen]

	ch := &ClientHello{}
	p := 0

	readU16 := func() (uint16, bool) {
		if p+2 > len(b) {
			return 0, false
		}
		v := binary.BigEndian.Uint16(b[p:])
		p += 2
		return v, true
	}
	readBytes := func(n int) ([]byte, bool) {
		if n < 0 || p+n > len(b) {
			return nil, false
		}
		v := b[p : p+n]
		p += n
		return v, true
	}

	v, ok := readU16()
	if !ok {
		return nil, fmt.Errorf("读 legacy_version 失败")
	}
	ch.LegacyVersion = v

	random, ok := readBytes(32)
	if !ok {
		return nil, fmt.Errorf("读 random 失败")
	}
	ch.Random = append([]byte(nil), random...)

	sidLen := 0
	if p < len(b) {
		sidLen = int(b[p])
	}
	if _, ok := readBytes(1 + sidLen); !ok {
		return nil, fmt.Errorf("读 session_id 失败")
	}
	ch.SessionID = append([]byte(nil), b[p-sidLen:p]...)

	if p+2 > len(b) {
		return nil, fmt.Errorf("读 cipher_suites 长度失败")
	}
	csLen := int(binary.BigEndian.Uint16(b[p:]))
	p += 2
	cs, ok := readBytes(csLen)
	if !ok {
		return nil, fmt.Errorf("读 cipher_suites 失败")
	}
	for i := 0; i+1 < len(cs); i += 2 {
		ch.CipherSuites = append(ch.CipherSuites, binary.BigEndian.Uint16(cs[i:]))
	}

	compLen := 0
	if p < len(b) {
		compLen = int(b[p])
	}
	comp, ok := readBytes(1 + compLen)
	if !ok {
		return nil, fmt.Errorf("读 compression_methods 失败")
	}
	ch.CompressionMethods = append([]byte(nil), comp[1:]...)

	// 扩展段可能不存在（极老的客户端）。
	if p+2 > len(b) {
		return ch, nil
	}
	extLen := int(binary.BigEndian.Uint16(b[p:]))
	p += 2
	extEnd := p + extLen
	if extEnd > len(b) {
		extEnd = len(b)
	}
	for p+4 <= extEnd {
		typ := binary.BigEndian.Uint16(b[p:])
		n := int(binary.BigEndian.Uint16(b[p+2:]))
		p += 4
		if p+n > len(b) {
			break
		}
		data := append([]byte(nil), b[p:p+n]...)
		ch.Extensions = append(ch.Extensions, Extension{Type: typ, Data: data})
		ch.parseKnownExtension(typ, data)
		p += n
	}
	return ch, nil
}

// parseKnownExtension 解出复刻时要用的那几项。解析失败就跳过，不影响原始落盘。
func (ch *ClientHello) parseKnownExtension(typ uint16, d []byte) {
	switch typ {
	case extServerName:
		// list_len(2) + type(1) + name_len(2) + name
		if len(d) < 5 || d[2] != 0 {
			return
		}
		n := int(binary.BigEndian.Uint16(d[3:]))
		if 5+n <= len(d) {
			ch.ServerName = string(d[5 : 5+n])
		}
	case extALPN:
		if len(d) < 2 {
			return
		}
		end := 2 + int(binary.BigEndian.Uint16(d))
		if end > len(d) {
			end = len(d)
		}
		for q := 2; q < end; {
			l := int(d[q])
			if q+1+l > len(d) {
				break
			}
			ch.ALPN = append(ch.ALPN, string(d[q+1:q+1+l]))
			q += 1 + l
		}
	case extSupportedGroups:
		if len(d) < 2 {
			return
		}
		end := 2 + int(binary.BigEndian.Uint16(d))
		if end > len(d) {
			end = len(d)
		}
		for q := 2; q+2 <= end; q += 2 {
			ch.Groups = append(ch.Groups, binary.BigEndian.Uint16(d[q:]))
		}
	case extSignatureAlgorithms:
		if len(d) < 2 {
			return
		}
		end := 2 + int(binary.BigEndian.Uint16(d))
		if end > len(d) {
			end = len(d)
		}
		for q := 2; q+2 <= end; q += 2 {
			ch.SignatureAlgs = append(ch.SignatureAlgs, binary.BigEndian.Uint16(d[q:]))
		}
	case extSupportedVersions:
		// ClientHello 里是 1 字节长度 + 若干 2 字节版本。
		if len(d) < 1 {
			return
		}
		n := int(d[0])
		for q := 1; q+2 <= 1+n && q+2 <= len(d); q += 2 {
			ch.SupportedVersions = append(ch.SupportedVersions, binary.BigEndian.Uint16(d[q:]))
		}
	case extKeyShare:
		if len(d) < 2 {
			return
		}
		end := 2 + int(binary.BigEndian.Uint16(d))
		if end > len(d) {
			end = len(d)
		}
		for q := 2; q+4 <= end; {
			g := binary.BigEndian.Uint16(d[q:])
			kl := int(binary.BigEndian.Uint16(d[q+2:]))
			if q+4+kl > len(d) {
				break
			}
			ch.KeyShares = append(ch.KeyShares, KeyShare{Group: g, Data: append([]byte(nil), d[q+4:q+4+kl]...)})
			q += 4 + kl
		}
	}
}
