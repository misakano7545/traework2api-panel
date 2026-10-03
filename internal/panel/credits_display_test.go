package panel

import (
	"os"
	"os/exec"
	"testing"
)

// fmtCredits 是纯函数（只吃数字、只返字符串），抠出来喂 node 断言。
// 抓的是「显示层会让人读错」的那几件事：0/缺值不许显示成 0（那是「没计价」不是「没花钱」）、
// 小值不许被砍成 0、坏输入不许渲染出 NaN/undefined。
func TestFmtCredits(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("无 node，跳过纯函数断言")
	}
	raw, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)
	driver := `
const assert = require('assert');
` + extractJSFunc(t, src, "fmtCredits") + `
const strip = s => s.replace(/<[^>]+>/g, '');
assert.strictEqual(strip(fmtCredits(0)), '—', '0 = 没计价，不能显示成 0');
assert.strictEqual(strip(fmtCredits(undefined)), '—');
assert.strictEqual(strip(fmtCredits(null)), '—');
assert.strictEqual(strip(fmtCredits('abc')), '—');
assert.strictEqual(strip(fmtCredits(40.6215)), '40.62');
assert.strictEqual(strip(fmtCredits(29)), '29', '整数不该拖小数点');
assert.strictEqual(strip(fmtCredits(0.9214)), '0.921', '一次小请求也要看得见');
assert.strictEqual(strip(fmtCredits(0.0043)), '0.0043', '更小的值不许被砍成 0');
for (const v of [0, 0.0043, 0.9214, 29, 40.6215, 1234.5, -3]) {
  const s = strip(fmtCredits(v));
  assert.ok(!/NaN|undefined|Infinity/.test(s), '坏渲染: ' + v + ' → ' + s);
}
`
	out, err := exec.Command(node, "-e", driver).CombinedOutput()
	if err != nil {
		t.Fatalf("node 断言失败: %v\n%s", err, out)
	}
}
