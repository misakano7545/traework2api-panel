// 面板静态资源与安全响应头。go:embed 打进二进制，无外部构建。
package panel

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"net/http"
	"strings"
)

//go:embed index.html
var indexHTMLRaw []byte

//go:embed theme.js
var themeJS []byte

//go:embed app.js
var appJS []byte

// fingerprint 内容指纹（sha256 前 8 字节）。脚本 URL 因此绑到内容上：反代/扩展/离线缓存
// 回放旧 app.js 时，新页面要的 URL 必然不同，不会出现「新 HTML 配旧 JS」的半截页面。
func fingerprint(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

// indexHTML 运行时把无查询串的 script src 换成带指纹的 URL（源码里保持可读、diff 干净）。
var indexHTML = func() []byte {
	s := strings.ReplaceAll(string(indexHTMLRaw), `src="theme.js"`, `src="theme.js?v=`+fingerprint(themeJS)+`"`)
	s = strings.ReplaceAll(s, `src="app.js"`, `src="app.js?v=`+fingerprint(appJS)+`"`)
	return []byte(s)
}()

// 与参考面板同级：脚本只允许同源，样式允许内联（色条宽度），禁止被嵌套。
const csp = "default-src 'none'; script-src 'self'; style-src 'self' 'unsafe-inline'; " +
	"connect-src 'self'; img-src 'self' data:; form-action 'none'; " +
	"frame-ancestors 'none'; base-uri 'none'"

func setSecurityHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Security-Policy", csp)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Cross-Origin-Opener-Policy", "same-origin")
	w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
}

func (p *Panel) index(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(indexHTML)
}

func (p *Panel) appScript(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(appJS)
}

func (p *Panel) themeScript(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(themeJS)
}
