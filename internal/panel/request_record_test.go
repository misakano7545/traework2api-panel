package panel

import (
	"os"
	"os/exec"
	"testing"
)

// TestRequestRecordPure 请求记录卡里的纯函数：筛选与单元格格式。
//
// 抓的是「算错会让面板说谎」那几件事：
//   - 筛选是空格分词的 AND（多关键词反过来当 OR 会把不匹配的请求显示出来）；
//   - token 只认 total，缺 total 才回落 prompt+completion；两者都没有必须回「—」
//     而不是 0（0 与「上游没报」是两件事）；
//   - 积分只在 credit_known 时显示，未收录单价的模型不能编一个数出来。
func TestRequestRecordPure(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("无 node，跳过请求记录断言")
	}
	raw, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)
	driver := `
const assert = require('assert');
` + extractJSFunc(t, src, "fmtTok") + "\n" +
		extractJSFunc(t, src, "trimFixed") + "\n" +
		extractJSFunc(t, src, "reqMatch") + "\n" +
		extractJSFunc(t, src, "reqTokenCell") + "\n" +
		extractJSFunc(t, src, "reqCreditCell") + `
const e = { client_ip: '1.1.1.1', user_agent: 'curl/8.4.0', model: 'cn:glm-5.2', account: 'u1(xxxxxxxx)', request_id: 'req-abcdef', outcome: 'success' };
// 空条件全通过
assert.ok(reqMatch(e, {}), '空条件必须全通过');
// 关键词：命中 IP / UA / 模型 / 账号 / 请求 ID
assert.ok(reqMatch(e, { q: '1.1.1.1' }));
assert.ok(reqMatch(e, { q: 'curl' }));
assert.ok(reqMatch(e, { q: 'glm-5.2' }));
assert.ok(reqMatch(e, { q: 'u1(' }));
assert.ok(reqMatch(e, { q: 'req-abcdef' }));
// 多关键词是 AND：任一不命中即淘汰
assert.ok(reqMatch(e, { q: 'glm curl 1.1.1.1' }), '多关键词命中应通过');
assert.ok(!reqMatch(e, { q: 'glm nope' }), '多关键词是 AND，not a OR');
// outcome 精确匹配（不是包含）
assert.ok(reqMatch(e, { outcome: 'success' }));
assert.ok(!reqMatch(e, { outcome: 'http_error' }));
assert.ok(!reqMatch({ ...e, outcome: 'success_extra' }, { outcome: 'success' }), 'outcome 必须精确匹配');
// 大小写不敏感
assert.ok(reqMatch(e, { q: 'CURL' }));
// token 单元格：total 优先，回落 pt+ct，全缺回 —
assert.strictEqual(reqTokenCell({ total_tokens: 1200 }), '1.2k');
assert.strictEqual(reqTokenCell({ prompt_tokens: 900, completion_tokens: 100 }), '1.0k');
assert.strictEqual(reqTokenCell({}), '—');
assert.strictEqual(reqTokenCell(null), '—');
// 积分单元格：只有 credit_known 才显示，且去掉多余的 0
assert.ok(/muted/.test(reqCreditCell({ credit: 1.5, credit_known: false })), '未记录积分应显示占位');
assert.strictEqual(reqCreditCell({ credit: 1.5, credit_known: true }), '1.5');
assert.strictEqual(reqCreditCell({ credit: 2, credit_known: true }), '2');
assert.ok(/muted/.test(reqCreditCell({})), '无积分字段应显示占位');
`
	out, err := exec.Command(node, "-e", driver).CombinedOutput()
	if err != nil {
		t.Fatalf("node 断言失败: %v\n%s", err, out)
	}
}
