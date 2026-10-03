package panel

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// api_key 的存储形状钉在 **localStorage + 无模块级缓存**（照 WB 仓）：
//
//  1. 存 localStorage，**不是** sessionStorage——后者是「每标签页一份、关掉就没」，
//     退回去的症状是「每次重开面板都要重输一次」，且没有任何运行时报错。
//  2. 唯一事实源就是存储本身：不设模块级 `key` 缓存，用到就现读（storedKey()）。
//     缓存变量一旦回来就得在多个写点手动同步，漏一个就是「面板显示已换密钥、
//     请求还在用旧的」这类只在特定顺序下出现的 bug。
//
// 两条都是**静默**回归（不报错、跑起来一切正常），只能靠静态断言拦。
func TestAPIKeyStorageShape(t *testing.T) {
	raw, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)

	for _, want := range []string{
		`localStorage.setItem(LS_KEY, v)`, // saveKey：唯一写点
		`localStorage.getItem(LS_KEY)`,    // storedKey：唯一读点
		`localStorage.removeItem(LS_KEY)`, // clearKey：登出
		`const sent = storedKey();`,       // 请求发出时的快照（401 竞态判断用）
	} {
		if !strings.Contains(src, want) {
			t.Errorf("app.js 里缺 %q —— 密钥的存储形状被改回去了", want)
		}
	}

	// 读只走 storedKey()、写只走 saveKey()：散落的直接存取会绕开 try/catch 与单一事实源。
	if n := strings.Count(src, "localStorage.getItem(LS_KEY)"); n != 1 {
		t.Errorf("localStorage.getItem(LS_KEY) 应只出现在 storedKey() 里，实得 %d 处", n)
	}
	if n := strings.Count(src, "localStorage.setItem(LS_KEY"); n != 1 {
		t.Errorf("localStorage.setItem(LS_KEY) 应只出现在 saveKey() 里，实得 %d 处", n)
	}
	// 密钥仍读写 sessionStorage = 退回「关掉标签页就丢」。
	if n := strings.Count(src, "sessionStorage.setItem(LS_KEY") + strings.Count(src, "sessionStorage.getItem(LS_KEY"); n != 0 {
		t.Errorf("密钥仍在读写 sessionStorage（%d 处）", n)
	}
	// 模块级缓存变量不许回来。
	if regexp.MustCompile(`(?m)^let key\b`).MatchString(src) {
		t.Error("模块级 key 缓存回来了——应以存储为唯一事实源，用到就 storedKey()")
	}
	// 登出必须真能清掉密钥（否则「登出」是假的）。
	if !strings.Contains(src, "clearKey()") {
		t.Error("登出没有走 clearKey()")
	}
	// 两个写点：密钥门 + 配置页保存后。少一个就会出现「换了密钥但浏览器还留着旧的」。
	if n := strings.Count(src, "saveKey("); n < 3 { // 定义 1 + 调用 2
		t.Errorf("saveKey( 应出现 ≥3 次（定义 + 密钥门 + 配置保存），实得 %d", n)
	}
}
