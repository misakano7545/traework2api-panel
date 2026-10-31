package panel

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestThemePreheat：首帧主题靠 theme.js 在 <style> 之前同步改 data-theme。
// 少了它或挪到 <style> 之后，浅色用户刷新会先闪一帧深色。
func TestThemePreheat(t *testing.T) {
	html := string(indexHTMLRaw)
	i := strings.Index(html, `src="theme.js"`)
	if i < 0 {
		t.Fatal("index.html 没有引用 theme.js")
	}
	if s := strings.Index(html, "<style>"); s < 0 || i > s {
		t.Error("theme.js 必须在 <style> 之前同步加载")
	}
	if strings.Contains(html, "<script>") {
		t.Error("页面不能有内联脚本：CSP script-src 'self' 会拦掉，主题就永远停在深色")
	}
	if a := strings.Index(html, `src="app.js"`); a < 0 || a > strings.Index(html, "</body>") {
		t.Error("app.js 应放在 </body> 之前，别让它阻塞首帧")
	}
	// 键名与判定规则必须与 app.js 同源，否则预热一套、app.js 再翻一套 = 二次闪烁。
	app, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`LS_THEME\s*=\s*'([^']+)'`).FindSubmatch(app)
	if m == nil {
		t.Fatal("app.js 里找不到 LS_THEME")
	}
	if !strings.Contains(string(themeJS), "'"+string(m[1])+"'") {
		t.Errorf("theme.js 读的 localStorage 键与 app.js 的 LS_THEME(%q) 不一致", m[1])
	}
}

// TestAssetURLCarriesFingerprint：脚本 URL 带内容指纹，缓存回放旧 app.js 时 URL 必然不同。
func TestAssetURLCarriesFingerprint(t *testing.T) {
	html := string(indexHTML)
	for _, want := range []string{"src=\"app.js?v=" + fingerprint(appJS) + "\"", "src=\"theme.js?v=" + fingerprint(themeJS) + "\""} {
		if !strings.Contains(html, want) {
			t.Errorf("注入后的页面缺少 %s", want)
		}
	}
}
