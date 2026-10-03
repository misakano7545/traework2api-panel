package panel

import (
	"os"
	"strings"
	"testing"
)

// api_key 必须存在 **localStorage**：sessionStorage 是「每标签页一份、关掉就没」，
// 用它会退化成「每次重开面板都要重输一次」（就是「前端无法本地存储 api_key」）。
// 这条纯静态断言的价值在于：改回 sessionStorage 不会有任何运行时症状，
// 只有真正关掉标签页重开的人才发现——所以用一条测试钉住它。
func TestAPIKeyPersistsInLocalStorage(t *testing.T) {
	raw, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)
	for _, want := range []string{
		`localStorage.setItem(LS_KEY, key)`, // 写入（密钥门）
		`localStorage.getItem(LS_KEY)`,      // 回读（boot）
		`localStorage.removeItem(LS_KEY)`,   // 登出清干净
	} {
		if !strings.Contains(src, want) {
			t.Errorf("app.js 里缺 %q —— 密钥会退回「关掉标签页就丢」", want)
		}
	}
	// 只剩登出时那一处 sessionStorage 清理；任何别处再拿 sessionStorage 当 key 存储都是回归。
	if n := strings.Count(src, "sessionStorage.setItem(LS_KEY") + strings.Count(src, "sessionStorage.getItem(LS_KEY"); n != 0 {
		t.Errorf("密钥仍在读写 sessionStorage（%d 处）", n)
	}
	// 配置页改密钥后也要落盘，否则刷新又变回旧值。
	if strings.Count(src, "localStorage.setItem(LS_KEY, key)") < 2 {
		t.Error("密钥落盘点应有两处：密钥门 + 配置页保存后")
	}
}
