package panel

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestConfigLoggingCardWiring 配置页「日志」卡的接线：卡片、字段映射、复选框读写三处缺一
// 就静默失效（卡片在但映射没加 = 勾了不落盘；映射在但 collectConfig 没 checkbox 分支 =
// 永远存成 false；loadConfig 没分支 = 打开页面开关状态是假的）。这三处都不进 id 配对检查
// （表单字段用 name 不用 id），所以单独钉住。
func TestConfigLoggingCardWiring(t *testing.T) {
	app, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	html, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	src, page := string(app), string(html)

	if !strings.Contains(page, `name="request_client_info"`) {
		t.Error("配置页缺少 request_client_info 开关（日志卡没搬过来）")
	}
	if !strings.Contains(page, "<h3>日志</h3>") {
		t.Error("配置页缺少「日志」卡")
	}
	if !regexp.MustCompile(`request_client_info:\s*\['logging',\s*'request_client_info'\]`).MatchString(src) {
		t.Error("CFG_MAP 缺 request_client_info → logging.request_client_info 映射：勾了也不会落盘")
	}
	// collectConfig 必须在读 el.value 之前分叉 checkbox，否则 value 恒为 "on" → 存成 false。
	collect := extractJSFunc(t, src, "collectConfig")
	if !strings.Contains(collect, "el.type === 'checkbox'") {
		t.Fatal("collectConfig 缺 checkbox 分支：复选框会永远提交 false")
	}
	if i, j := strings.Index(collect, "el.type === 'checkbox'"), strings.Index(collect, "el.value.trim()"); i < 0 || j < 0 || i > j {
		t.Error("collectConfig 的 checkbox 分支必须在读 el.value 之前")
	}
	load := extractJSFunc(t, src, "loadConfig")
	if !strings.Contains(load, "el.type === 'checkbox'") || !strings.Contains(load, "el.checked") {
		t.Error("loadConfig 缺 checkbox 回填：打开配置页时开关状态与配置无关")
	}
}
