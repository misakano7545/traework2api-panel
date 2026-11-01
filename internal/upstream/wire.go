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
	"compress/flate"
	"compress/gzip"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
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
	resp, err := t.base.RoundTrip(r2)
	if err != nil {
		return nil, err
	}
	return decodeBody(resp)
}

// decodeBody 按 Content-Encoding 包一层解压 reader。
//
// 为什么要自己做：一旦请求侧显式写了 Accept-Encoding，net/http 就不再自动
// 加 gzip、也不再透明解压（transport.go 里 requestedGzip 只在 header 为空时置真）。
// 而客户端发的是多值，不显式写就少一层伪装。
//
// 四种编码都支持：gzip / deflate 走标准库，br 走 andybalholm/brotli，
// zstd 走 klauspost/compress/zstd —— 这两个是 utls 的传递依赖，本来就在树里，
// 所以补齐四值不再额外增重。
func decodeBody(resp *http.Response) (*http.Response, error) {
	enc := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding")))
	if enc == "" || enc == "identity" {
		return resp, nil
	}
	var rc io.ReadCloser
	switch enc {
	case "gzip":
		zr, err := gzip.NewReader(resp.Body)
		if err != nil {
			resp.Body.Close()
			return nil, fmt.Errorf("gzip 响应解压失败: %w", err)
		}
		rc = zr
	case "deflate":
		rc = flate.NewReader(resp.Body)
	case "br":
		rc = io.NopCloser(brotli.NewReader(resp.Body))
	case "zstd":
		zr, err := zstd.NewReader(resp.Body)
		if err != nil {
			resp.Body.Close()
			return nil, fmt.Errorf("zstd 响应解压失败: %w", err)
		}
		rc = zstdCloser{zr}
	default:
		resp.Body.Close()
		return nil, fmt.Errorf("不支持的 Content-Encoding %q", enc)
	}
	resp.Body = &decodedBody{Reader: rc, raw: resp.Body}
	// 解压后长度与编码都不再成立，留着会让上层读错。
	resp.Header.Del("Content-Encoding")
	resp.Header.Del("Content-Length")
	resp.ContentLength = -1
	resp.Uncompressed = true
	return resp, nil
}

// zstdCloser 让 *zstd.Decoder 满足 io.ReadCloser。
type zstdCloser struct{ d *zstd.Decoder }

func (z zstdCloser) Read(p []byte) (int, error) { return z.d.Read(p) }
func (z zstdCloser) Close() error              { z.d.Close(); return nil }

// decodedBody 关掉解压器时一并关掉底层连接，避免 SSE 长流泄漏。
type decodedBody struct {
	io.Reader
	raw io.ReadCloser
}

func (b *decodedBody) Close() error {
	if c, ok := b.Reader.(io.Closer); ok {
		_ = c.Close()
	}
	return b.raw.Close()
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
// 两件事在这里一起定：
//   - 关掉 HTTP/2（客户端抓包里是 HTTP/1.1 + Connection: keep-alive，而 http2
//     明确拒绝 Connection 头）。TLSNextProto 给一个非 nil 的空 map 就禁用 h2。
//   - TLS 握手改走 utls 的浏览器指纹（见 tls.go）。用 DialTLSContext 接管后，
//     http.Transport 不再自己做 TLS，返回的必须是已握手的连接。
func newWireTransport(base *http.Transport) *headerCaseTransport {
	base.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	base.DialTLSContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		// 接管握手后 Transport 不再看 TLSClientConfig，这里必须自己继承它的
		// InsecureSkipVerify，否则测试里挂自签证书会直接验签失败。
		insecure := base.TLSClientConfig != nil && base.TLSClientConfig.InsecureSkipVerify
		return dialTLSWithFingerprint(ctx, network, addr, currentTLSFingerprint(), insecure)
	}
	return &headerCaseTransport{base: base}
}
