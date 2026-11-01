// wire.go 出站报文的「线上形态」：头名大小写与 HTTP 版本。
//
// 为什么要在 Transport 层做，而不是在 headers.go 里直接写 http.Header 的 map：
// net/http 的 Header.Set/Get 都走 textproto.CanonicalMIMEHeaderKey，一旦把 key
// 改成原生客户端的小写写法，仓库里所有 Header.Get("X-App-Id") 就全部读不到了。
// 在 RoundTrip 里对**副本**改 key，调用方手里的 req 不受影响，读语义保持标准。
//
// 只改 key 的拼写，不改值的语义；头顺序仍然是 Go 的字母序（要改顺序得整条自写
// 报文，成本远高于收益，见 README）。
package upstream

import (
	"crypto/tls"
	"net/http"
	"strings"
	"time"
)

// clientUpperHeaders 是 2026-10-30 抓包里**首字母大写**的那几条。
//
// 客户端其余头一律小写（accept / user-agent / x-app-id / x-os-version …），
// Go 默认会把它们写成 Title-Case，这是最容易被「精确比对」类规则命中的差异。
var clientUpperHeaders = map[string]string{
	"host":           "Host",
	"connection":     "Connection",
	"content-length": "Content-Length",
	"referer":        "Referer",
	"x-ss-dp":        "X-SS-DP",
	"x-helios":       "X-Helios",
	"x-medusa":       "X-Medusa",
	"x-neptune":      "X-Neptune",
}

// clientHeaderCase 把 Go 规范化过的头名还原成客户端线上的写法。
func clientHeaderCase(name string) string {
	lower := strings.ToLower(name)
	if up, ok := clientUpperHeaders[lower]; ok {
		return up
	}
	return lower
}

// headerCaseTransport 在真正写出前把请求头名改回客户端写法。
//
// 它不改调用方持有的 *http.Request：Clone 出来的是副本，Header 已深拷贝。
type headerCaseTransport struct {
	base *http.Transport
}

func (t *headerCaseTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r2 := r.Clone(r.Context())
	h := make(http.Header, len(r2.Header))
	for k, v := range r2.Header {
		h[clientHeaderCase(k)] = v
	}
	r2.Header = h
	return t.base.RoundTrip(r2)
}

// SetResponseHeaderTimeout 让热改超时能穿透这层包装。
//
// setHeaderTimeout 原先靠 `hc.Transport.(*http.Transport)` 断言直接改字段；
// 包一层之后那个断言会失败，配置热改就会静默失效。走接口，两种形态都能命中。
func (t *headerCaseTransport) SetResponseHeaderTimeout(d time.Duration) {
	t.base.ResponseHeaderTimeout = d
}

// newWireTransport 建一个「报文形态贴近官方客户端」的 Transport。
//
// 关掉 HTTP/2 是刻意的：客户端抓包里是 HTTP/1.1（带 Connection: keep-alive），
// 而 http2 明确拒绝 Connection 头。TLSNextProto 给一个非 nil 的空 map 就禁用 h2。
func newWireTransport(base *http.Transport) *headerCaseTransport {
	base.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	return &headerCaseTransport{base: base}
}
