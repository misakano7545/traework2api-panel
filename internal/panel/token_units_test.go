package panel

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestUsageTokenUnits token 列必须换算单位。
//
// 抓的是「看一眼读不出量级」这件事：5,302,202 这种裸千分位数在窄列里没有意义，
// 要写成 5.30M / 50.02k（与请求记录卡的 Token 列同一个助手 fmtTok）。
// 账号池的用量芯片、用量明细/按账号/按模型/按小时行、用量总览四张卡共用这条口径。
func TestUsageTokenUnits(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("无 node，跳过单位换算断言")
	}
	raw, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)
	driver := `
const assert = require('assert');
let usageHours = '72';
` + extractJSFunc(t, src, "fmtTok") + "\n" +
		extractJSFunc(t, src, "fmtNum") + "\n" +
		extractJSFunc(t, src, "fmtMs") + "\n" +
		extractJSFunc(t, src, "fmtRate") + "\n" +
		extractJSFunc(t, src, "fmtHit") + "\n" +
		extractJSFunc(t, src, "fmtCredits") + "\n" +
		extractJSFunc(t, src, "usMixBar") + "\n" +
		extractJSFunc(t, src, "esc") + "\n" +
		extractJSFunc(t, src, "usageCell") + "\n" +
		extractJSFunc(t, src, "usRow") + `
// 账号池「用量」列：token 要带单位，不能是裸千分位数
const cell = usageCell({ requests: 528, total_tokens: 84300000, avg_latency_ms: 74700, avg_tokens_per_second: 53.9 });
assert.ok(cell.includes('84.30M'), '用量列 token 未换算单位: ' + cell);
assert.ok(cell.includes('74.70s'), '用量列延迟未换算成秒: ' + cell);
assert.ok(!cell.includes('84,300,000'), '用量列不该出现裸千分位数: ' + cell);
// 小值不加单位（<1000 原样）
assert.ok(usageCell({ requests: 3, total_tokens: 98 }).includes('>98<'), '小 token 值应原样显示');
// 用量明细行：四列（请求/Prompt/Completion/合计）都要换算
const row = usRow('cn:glm-5.2', '', {
  requests: 5302, errors: 0,
  prompt_tokens: 5302202, completion_tokens: 50022, total_tokens: 5352224,
  cache_samples: 63, cache_hit_tokens: 5122304, credits: 24.98,
}, true);
assert.ok(row.includes('5.30M'), '明细行 Prompt/合计 未换算: ' + row);
assert.ok(row.includes('50.0k'), '明细行 Completion 未换算: ' + row);
// 只看可见单元格：title 里的悬停提示**故意**保留精确值（5,122,304 这种看得越多越好）。
const visible = row.replace(/title="[^"]*"/g, '');
assert.ok(!visible.includes('5,302,202') && !visible.includes('5,352,224'),
  '明细行可见列不该出现裸千分位数: ' + visible);
`
	out, err := exec.Command(node, "-e", driver).CombinedOutput()
	if err != nil {
		t.Fatalf("node 断言失败: %v\n%s", err, out)
	}
}

// TestUsageKpiTokenUnits 用量总览卡片的 token 也要换算（与明细行同口径，别一处一种写法）。
func TestUsageKpiTokenUnits(t *testing.T) {
	raw, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)
	for _, bad := range []string{"usKpi(fmtNum(total), '总 token'", "usKpi(fmtNum(pt), 'prompt'", "usKpi(fmtNum(ct), 'completion'", "usKpi(fmtNum(reqs), '请求数'"} {
		if strings.Contains(src, bad) {
			t.Errorf("用量总览卡仍是裸数字：%s", bad)
		}
	}
}
