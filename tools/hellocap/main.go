package main

import (
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8443", "监听地址")
	out := flag.String("out", "hellocap-out", "输出目录")
	timeout := flag.Duration("timeout", 30*time.Second, "等 ClientHello 的最长时间")
	once := flag.Bool("once", true, "抓到一条（且通过 -sni 过滤）就退出")
	genSpecFlag := flag.Bool("gen-spec", true, "同时生成可直接编译的 utls ClientHelloSpec 代码")
	sni := flag.String("sni", "", "只收 ServerName 含这些子串的连接，逗号分隔（空 = 全收）")
	flag.Parse()

	var want []string
	for _, s := range strings.Split(*sni, ",") {
		if s = strings.TrimSpace(s); s != "" {
			want = append(want, strings.ToLower(s))
		}
	}
	// 空过滤列表 = 全收；非空时任何一个子串命中即可。
	match := func(name string) bool {
		if len(want) == 0 {
			return true
		}
		n := strings.ToLower(name)
		for _, w := range want {
			if strings.Contains(n, w) {
				return true
			}
		}
		return false
	}

	if err := os.MkdirAll(*out, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "建输出目录失败:", err)
		os.Exit(1)
	}

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "监听失败:", err)
		os.Exit(1)
	}
	defer ln.Close()

	fmt.Printf("等待 ClientHello —— 监听 %s，输出 %s\n", *addr, *out)
	if len(want) > 0 {
		fmt.Printf("只收 ServerName 含 %v 的连接\n", want)
	}
	fmt.Printf("用法：把系统/客户端代理指到 %s，触发一次 HTTPS 请求即可。\n", *addr)
	fmt.Println("（抓 ClientHello 不需要证书、不需要解密：握手第一步就断开。）")

	// 保存成功的信号。once 模式由独立 goroutine 消费，避免主循环被一次「不匹配」卡死。
	saved := make(chan string, 8)
	if *once {
		go func() {
			tag := <-saved
			fmt.Printf("\n已抓到 hello-%s，退出。\n", tag)
			os.Exit(0)
		}()
	}

	n := 0
	for {
		conn, err := ln.Accept()
		if err != nil {
			fmt.Fprintln(os.Stderr, "accept:", err)
			break
		}
		go func() {
			defer conn.Close()
			n++
			tag := strconv.Itoa(n)
			raw, ch, err := capture(conn, *timeout)
			if err != nil {
				fmt.Printf("[%s] 没抓到：%v\n", tag, err)
				return
			}
			if ch == nil {
				fmt.Printf("[%s] 不是 ClientHello\n", tag)
				return
			}
			if !match(ch.ServerName) {
				fmt.Printf("[%s] 跳过（SNI=%q 不在过滤列表）\n", tag, ch.ServerName)
				return
			}
			if err := save(*out, tag, raw, ch); err != nil {
				fmt.Printf("[%s] 落盘失败：%v\n", tag, err)
				return
			}
			fmt.Print(renderReport(ch))
			if *genSpecFlag {
				code := genSpec(ch)
				path := filepath.Join(*out, "spec-"+tag+".go.txt")
				if err := os.WriteFile(path, []byte(code), 0o644); err != nil {
					fmt.Printf("[%s] 生成 spec 失败：%v\n", tag, err)
				} else {
					fmt.Printf("\n已生成 spec：%s\n%s\n", path, code)
				}
			}
			saved <- tag
		}()
	}
}

// capture 从一个连接上截下首条 ClientHello。
//
// 两种入口都要处理：
//   - 走 HTTP 代理：先来 "CONNECT host:443 HTTP/1.1"，要先回 200 再读 TLS；
//   - 直连/透明代理：第一个字节就是 0x16（handshake）。
func capture(conn net.Conn, timeout time.Duration) ([]byte, *ClientHello, error) {
	_ = conn.SetDeadline(time.Now().Add(timeout))
	buf := make([]byte, 0, 8192)
	tmp := make([]byte, 4096)

	// 先读到足以判断是不是 CONNECT。
	for len(buf) < 5 {
		n, err := conn.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
		}
		if err != nil {
			if len(buf) > 0 && buf[0] == 0x16 {
				break
			}
			return nil, nil, fmt.Errorf("读首块失败: %w", err)
		}
	}

	if len(buf) >= 8 && strings.HasPrefix(strings.ToUpper(string(buf[:8])), "CONNECT ") {
		// 读到 header 结束，回 200，之后就是纯 TLS。
		for !strings.Contains(string(buf), "\r\n\r\n") {
			n, err := conn.Read(tmp)
			if n > 0 {
				buf = append(buf, tmp[:n]...)
			}
			if err != nil {
				return nil, nil, fmt.Errorf("读 CONNECT 头失败: %w", err)
			}
			if len(buf) > 8192 {
				return nil, nil, fmt.Errorf("CONNECT 头过长")
			}
		}
		idx := strings.Index(string(buf), "\r\n\r\n")
		buf = append([]byte(nil), buf[idx+4:]...)
		if _, err := conn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
			return nil, nil, fmt.Errorf("回 200 失败: %w", err)
		}
	}

	// 攒到能切出一条完整 ClientHello 为止。
	for {
		consumed, recs, err := splitRecords(buf)
		if err != nil {
			return nil, nil, err
		}
		hs := handshakeBytes(recs)
		if len(hs) >= 4 {
			need := 4 + int(hs[1])<<16>>8 + int(hs[2])<<8 + int(hs[3])
			_ = need
			bodyLen := int(hs[1])<<16 | int(hs[2])<<8 | int(hs[3])
			if len(hs) >= 4+bodyLen {
				raw := append([]byte(nil), buf[:consumed]...)
				ch, perr := ParseClientHello(hs)
				if perr != nil {
					return raw, nil, perr
				}
				return raw, ch, nil
			}
		}
		n, err := conn.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
		}
		if err != nil {
			if len(buf) > 0 {
				return append([]byte(nil), buf...), nil, fmt.Errorf("连接提前结束，只拿到 %d 字节", len(buf))
			}
			return nil, nil, err
		}
		if len(buf) > 1<<20 {
			return nil, nil, fmt.Errorf("数据过多，不像 ClientHello")
		}
	}
}

// save 落三样东西：原始字节（hex，方便肉眼核对）、解析后的 JSON、可读报告。
func save(dir, tag string, raw []byte, ch *ClientHello) error {
	if err := os.WriteFile(filepath.Join(dir, "hello-"+tag+".hex"), []byte(hex.EncodeToString(raw)), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "hello-"+tag+".bin"), raw, 0o644); err != nil {
		return err
	}
	type dump struct {
		JA3        string   `json:"ja3"`
		JA3Raw     string   `json:"ja3_raw"`
		JA4        string   `json:"ja4"`
		Version    string   `json:"legacy_version"`
		ServerName string   `json:"server_name"`
		ALPN       []string `json:"alpn"`
		Ciphers    []string `json:"cipher_suites"`
		Extensions []string `json:"extensions"`
		Groups     []string `json:"supported_groups"`
		SigAlgs    []string `json:"signature_algorithms"`
		KeyShares  []string `json:"key_shares"`
		Versions   []string `json:"supported_versions"`
	}
	d := dump{Version: fmt.Sprintf("0x%04x", ch.LegacyVersion), ServerName: ch.ServerName, ALPN: ch.ALPN}
	d.JA3, d.JA3Raw = ch.JA3()
	d.JA4 = ch.JA4()
	for _, v := range ch.CipherSuites {
		d.Ciphers = append(d.Ciphers, fmt.Sprintf("0x%04x", v))
	}
	for _, e := range ch.Extensions {
		d.Extensions = append(d.Extensions, fmt.Sprintf("%d:%s", e.Type, extName(e.Type)))
	}
	for _, g := range ch.Groups {
		d.Groups = append(d.Groups, fmt.Sprintf("%d", g))
	}
	for _, s := range ch.SignatureAlgs {
		d.SigAlgs = append(d.SigAlgs, fmt.Sprintf("%d", s))
	}
	for _, k := range ch.KeyShares {
		d.KeyShares = append(d.KeyShares, fmt.Sprintf("%d(%dB)", k.Group, len(k.Data)))
	}
	for _, v := range ch.SupportedVersions {
		d.Versions = append(d.Versions, fmt.Sprintf("0x%04x", v))
	}
	b, _ := json.MarshalIndent(d, "", "  ")
	return os.WriteFile(filepath.Join(dir, "hello-"+tag+".json"), b, 0o644)
}

// renderReport 打一份能直接贴进 README 的报告。
func renderReport(ch *ClientHello) string {
	var sb strings.Builder
	ja3, raw := ch.JA3()
	fmt.Fprintf(&sb, "\n=========== ClientHello ===========\n")
	fmt.Fprintf(&sb, "JA3: %s\n", ja3)
	fmt.Fprintf(&sb, "JA4: %s\n", ch.JA4())
	fmt.Fprintf(&sb, "SNI: %q  ALPN: %v\n", ch.ServerName, ch.ALPN)
	fmt.Fprintf(&sb, "legacy_version: 0x%04x  supported_versions: ", ch.LegacyVersion)
	for _, v := range ch.SupportedVersions {
		fmt.Fprintf(&sb, "0x%04x ", v)
	}
	fmt.Fprintf(&sb, "\n\n套件（%d，含 GREASE）：\n  ", len(ch.CipherSuites))
	for _, v := range ch.CipherSuites {
		fmt.Fprintf(&sb, "0x%04x ", v)
	}
	fmt.Fprintf(&sb, "\n\n扩展（%d，按线上顺序）：\n", len(ch.Extensions))
	for i, e := range ch.Extensions {
		fmt.Fprintf(&sb, "  %2d. %-32s (%dB)\n", i+1, extName(e.Type), len(e.Data))
	}
	fmt.Fprintf(&sb, "\nsupported_groups（%d）：", len(ch.Groups))
	for _, g := range ch.Groups {
		fmt.Fprintf(&sb, "%d ", g)
	}
	fmt.Fprintf(&sb, "\nkey_share（%d）：", len(ch.KeyShares))
	for _, k := range ch.KeyShares {
		fmt.Fprintf(&sb, "%d(%dB) ", k.Group, len(k.Data))
	}
	fmt.Fprintf(&sb, "\nsignature_algorithms（%d）：", len(ch.SignatureAlgs))
	for _, s := range ch.SignatureAlgs {
		fmt.Fprintf(&sb, "%d ", s)
	}
	fmt.Fprintf(&sb, "\n\nJA3 原文: %s\n", raw)
	fmt.Fprintf(&sb, "\n%s\n", ch.compareWithChrome133())
	return sb.String()
}

var _ = io.Discard
