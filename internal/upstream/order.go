// order.go 出站报文的**头顺序**：在连接层重写报文头，让顺序与官方客户端一致。
//
// 为什么不在 RoundTrip 层做：net/http 写请求头走 Header.writeSubset，它按 key 的
// 字母序排序（sortedKeyValues），这个顺序不受调用方控制。要改就只能绕开它自己拼报文 ——
// 那等于重造连接池、超时、chunked 与并发安全。
//
// 折中做法：把一个 net.Conn 包一层，在 Write 时判断「这次写的是不是一整段报文头」，
// 是就重排后再写出。Transport 完全不知情，连接池 / 超时 / chunked / SSE 流都还是
// 标准库在管。
//
// 顺带把头名大小写也在这里解决（clientHeaderCase）。
package upstream

import (
	"bytes"
	"net"
	"sort"
	"strings"

	"traework2api/internal/auth"
)

// clientHeaderOrder 官方客户端 chat 链路的头顺序（2026-10-30 抓包，CN 口径）。
//
// 客户端的顺序按 SDK 内部字段声明排，不是字母序 —— 同一个 aha 网络栈每次请求都是
// 这个序。表里没有的头按原相对顺序排在最后。
var clientHeaderOrder = []string{
	"host",
	"connection",
	"content-length",
	"accept-encoding",
	"content-type",
	"sec-fetch-dest",
	"sec-fetch-mode",
	"sec-fetch-site",
	"user-agent",
	"app-version",
	"x-ahanet-timeout",
	"x-custom-trace-id",
	"authorization",
	"traceparent",
	"x-agent-host-type",
	"x-app-id",
	"x-app-version",
	"x-app-version-code",
	"x-bridge-transport",
	"x-device-brand",
	"x-device-cpu",
	"x-device-id",
	"x-device-type",
	"x-flow-traceparent",
	"x-harness",
	"x-ide-token",
	"x-ide-version",
	"x-ide-version-code",
	"x-ide-version-type",
	"x-lgw-req-sdk-type",
	"x-machine-id",
	"x-os-version",
	"package-type",
	"x-request-id",
	"x-user-region",
	"x-lscbd-aid",
	"x-lscbd-platform",
	"accept",
	"x-ss-dp",
	"x-tt-trace-id",
	"referer",
	"x-helios",
	"x-medusa",
	"x-neptune",
}

// headerRank 头名 → 排位。表外的排到表尾之后，保持原相对顺序。
var headerRank = func() map[string]int {
	m := make(map[string]int, len(clientHeaderOrder))
	for i, k := range clientHeaderOrder {
		m[k] = i
	}
	return m
}()

// reorderHead 把整段 HTTP 请求头按客户端顺序重排，并把头名换成客户端的写法。
//
// 输入含请求行、不含结尾空行；解析不出头时原样返回 —— 绝不因为「想排个序」把请求搞坏。
func reorderHead(head []byte, realm string) []byte {
	text := strings.ReplaceAll(string(head), "\r\n", "\n")
	lines := strings.Split(text, "\n")
	if len(lines) == 0 || lines[0] == "" {
		return head
	}

	type hdr struct {
		name  string
		value string
		rank  int
		seq   int
	}
	var hs []hdr
	for i, ln := range lines[1:] {
		if ln == "" {
			continue
		}
		colon := strings.IndexByte(ln, ':')
		if colon <= 0 {
			continue
		}
		name := strings.ToLower(strings.TrimSpace(ln[:colon]))
		val := strings.TrimSpace(ln[colon+1:])
		r, ok := headerRank[name]
		if !ok {
			r = len(clientHeaderOrder) + 1
		}
		hs = append(hs, hdr{name: name, value: val, rank: r, seq: i})
	}
	if len(hs) == 0 {
		return head
	}

	sort.SliceStable(hs, func(i, j int) bool {
		if hs[i].rank != hs[j].rank {
			return hs[i].rank < hs[j].rank
		}
		return hs[i].seq < hs[j].seq
	})

	var b bytes.Buffer
	b.Grow(len(head) + 32)
	b.WriteString(lines[0])
	b.WriteString("\r\n")
	for _, h := range hs {
		b.WriteString(clientHeaderCase(h.name))
		b.WriteString(": ")
		b.WriteString(h.value)
		b.WriteString("\r\n")
	}
	return b.Bytes()
}

// headConn 在写报文头时重排，其余透传。
//
// 只处理「这一次 Write 里就有完整头」的情况。不做跨 Write 缓冲：连接会被复用，
// 缓冲状态得跟着请求边界重置，判断错一次就会把上个请求的 body 拼进新头里
// （实测第二次请求直接 400）。头被拆开时就退回标准库的字母序，宁可少对齐一层，
// 也不能发坏包。
type headConn struct {
	net.Conn
	realm string
}

func newHeadConn(c net.Conn, realm string) *headConn {
	return &headConn{Conn: c, realm: realm}
}

func (c *headConn) Write(p []byte) (int, error) {
	if !looksLikeRequestHead(p) {
		return c.Conn.Write(p)
	}
	idx := bytes.Index(p, []byte("\r\n\r\n"))
	if idx < 0 {
		return c.Conn.Write(p)
	}
	out := append(reorderHead(p[:idx], c.realm), "\r\n"...)
	out = append(out, p[idx+4:]...)
	if _, err := c.Conn.Write(out); err != nil {
		return 0, err
	}
	return len(p), nil
}

// httpMethods 判断一次 Write 是不是以 HTTP 方法开头（即报文头）。
var httpMethods = [][]byte{
	[]byte("GET "), []byte("POST "), []byte("PUT "), []byte("HEAD "),
	[]byte("DELETE "), []byte("OPTIONS "), []byte("PATCH "), []byte("CONNECT "),
}

func looksLikeRequestHead(p []byte) bool {
	for _, m := range httpMethods {
		if bytes.HasPrefix(p, m) {
			return true
		}
	}
	return false
}

// realmForHost 按目标主机判断该用哪套头顺序（目前两区顺序一致，留口子）。
func realmForHost(host string) string {
	if strings.Contains(host, "alisg") || strings.Contains(host, ".trae.ai") {
		return auth.RealmIntl
	}
	return auth.RealmCN
}
