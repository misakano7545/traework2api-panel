package panel

import (
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

// TestAppJSIdsExistInHTML 静态配对：app.js 里 $('x') 引用的 id 必须在 index.html 存在。
// 排版被重写/合并时最容易丢容器——$() 返回 null → TypeError 被函数自己的 try/catch 吞掉，
// 同一次调用里排在后面的渲染整段跳过，表现为「整块空白但接口 200、console 无异常」。
// 这条静态检查先于浏览器，一秒级拦下这类静默空白。
func TestAppJSIdsExistInHTML(t *testing.T) {
	app, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	html, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, m := range regexp.MustCompile(`\$\('([A-Za-z0-9_-]+)'\)`).FindAllStringSubmatch(string(app), -1) {
		n++
		if !strings.Contains(string(html), `id="`+m[1]+`"`) {
			t.Errorf("app.js 引用的 id 在 index.html 里不存在: %s", m[1])
		}
	}
	if n == 0 {
		t.Fatal("没有从 app.js 里扫到任何 id 引用，正则可能失效")
	}
}

// TestModelFilterPure 模型筛选/排序是纯函数（只吃参数、只返值），抠出来喂 node 断言。
// 抓的是「算错会筛掉本该显示的模型」那几件事：空条件全通过、关键字 AND、域前缀、
// 排序不倒序。没有 node 就跳过——这是给改筛选逻辑的人兜底，不是 CI 硬依赖。
func TestModelFilterPure(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("无 node，跳过模型筛选断言")
	}
	raw, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)
	driver := `
const assert = require('assert');
` + extractJSFunc(t, src, "mdSearchText") + "\n" +
		extractJSFunc(t, src, "mdMatch") + "\n" +
		extractJSFunc(t, src, "mdSortList") + `
const a = { id: 'cn:glm-5.2', name: 'GLM-5.2', capability: 'reasoning_model', function: 'solo_work_lite', context_length: 200000, max_output_tokens: 32000 };
const b = { id: 'intl:gpt-5.4', name: 'GPT-5.4', capability: 'reasoning_model', function: 'solo_coder', context_length: 128000, max_output_tokens: 64000 };
const c = { id: 'cn:kimi-k3', name: '', capability: 'reasoning_model', function: 'solo_work_lite' };
// 空条件：全部通过
assert.ok(mdMatch(a, { q: '', realm: '', fn: '', sort: 'default' }));
// 关键字：ID 命中、展示名命中（大小写不敏感）、多词 AND
assert.ok(mdMatch(a, { q: 'glm' }));
assert.ok(mdMatch(a, { q: 'GLM-5.2' }));
assert.ok(!mdMatch(a, { q: 'cn glm-5.2 nope' }), '多关键词是 AND');
// 域前缀筛选：cn 只留 cn:，intl 只留 intl:
assert.ok(mdMatch(a, { realm: 'cn' }) && !mdMatch(b, { realm: 'cn' }));
assert.ok(mdMatch(b, { realm: 'intl' }));
// 通道筛选
assert.ok(mdMatch(a, { fn: 'solo_work_lite' }) && !mdMatch(b, { fn: 'solo_work_lite' }));
// 排序：上下文档 大→小；缺字段当 0 排最后，不冒充最大
const byCtx = mdSortList([c, a, b], { sort: 'context' });
assert.strictEqual(byCtx[0].id, 'cn:glm-5.2');
assert.strictEqual(byCtx[byCtx.length - 1].id, 'cn:kimi-k3');
const byOut = mdSortList([a, b], { sort: 'output' });
assert.strictEqual(byOut[0].id, 'intl:gpt-5.4');
const byName = mdSortList([b, a], { sort: 'name' });
assert.strictEqual(byName[0].id, 'cn:glm-5.2');
// mdSortList 不改动入参（上游原始顺序可回溯）
const orig = [c, a, b];
mdSortList(orig, { sort: 'name' });
assert.strictEqual(orig[0].id, 'cn:kimi-k3', 'mdSortList 不能改动入参');
`
	out, err := exec.Command(node, "-e", driver).CombinedOutput()
	if err != nil {
		t.Fatalf("node 断言失败: %v\n%s", err, out)
	}
}
