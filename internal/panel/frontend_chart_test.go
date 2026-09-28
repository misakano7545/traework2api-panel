package panel

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// extractJSFunc 从 app.js 源码里抠出顶层函数体（收尾大括号顶格那一行）。
// 只给本文件用：图表的三个函数是纯函数，抠出来喂 node 就能断言。
func extractJSFunc(t *testing.T, src, name string) string {
	t.Helper()
	i := strings.Index(src, "function "+name+"(")
	if i < 0 {
		t.Fatalf("app.js 里找不到 function %s", name)
	}
	j := strings.Index(src[i:], "\n}\n")
	if j < 0 {
		t.Fatalf("%s 的收尾大括号没找到", name)
	}
	return src[i : i+j+3]
}

// TestUsageChartSVG 时序图的断言：三个函数都只吃参数、只返字符串，所以能在 node 里直接跑。
// 抓的是**算错会图不画**的那几件事：空态、坏时间点丢弃、堆叠柱数量、网格/刻度、y 轴取最大值。
// 没有 node 的环境跳过——这条是给改图的人兜底，不是 CI 硬依赖。
func TestUsageChartSVG(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("无 node，跳过图表断言")
	}
	raw, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)
	driver := `
const assert = require('assert');
` + extractJSFunc(t, src, "usagePointTime") + "\n" +
		extractJSFunc(t, src, "fmtShortTok") + "\n" +
		extractJSFunc(t, src, "usageChartSVG") + `
assert.strictEqual(fmtShortTok(1234), '1.2k');
assert.strictEqual(fmtShortTok(1000000), '1.0M');
assert.ok(usageChartSVG([]).includes('us-empty'), '空数据要出空态');
assert.ok(usageChartSVG([{ t: 'bad' }]).includes('us-empty'), '时间解析不出来的点必须被丢掉，不能画出 NaN 图');
assert.strictEqual(usagePointTime('2026-09-25T15').label, '09-25 15:00');
assert.strictEqual(usagePointTime('2026-09-25').label, '09-25');
assert.strictEqual(usagePointTime('nope'), null);
const svg = usageChartSVG([
  { t: '2026-09-25T15', prompt_tokens: 100, completion_tokens: 50, total_tokens: 150 },
  { t: '2026-09-25T16', prompt_tokens: 200, completion_tokens: 80, total_tokens: 280 },
]);
assert.strictEqual((svg.match(/<rect/g) || []).length, 4, '两个时间片 → 4 根柱（输入+输出各一）');
assert.strictEqual((svg.match(/class="ax"/g) || []).length, 4, '3 条网格 + 1 条基线');
assert.ok(svg.includes('>09-25 15:00<') && svg.includes('>09-25 16:00<'), '首尾要有时间刻度');
assert.ok(svg.includes('>280<'), 'y 轴上限取最大时间片');
assert.ok(!svg.includes('NaN') && !svg.includes('undefined'), '图里不许出现 NaN/undefined');
// 柱子必须夹在绘图区内（PL=46 / 右边距 PR=12）：首尾柱半个柱宽探出去会压住 y 轴刻度。
for (const m of svg.matchAll(/<rect[^>]*x="([\d.]+)"[^>]*width="([\d.]+)"/g)) {
  const x = Number(m[1]), w = Number(m[2]);
  assert.ok(x >= 46, '柱子左沿探进 y 轴刻度区: x=' + x);
  assert.ok(x + w <= 748, '柱子右沿探出绘图区: x+w=' + (x + w));
}
// 缓存命中率：上游没报 → 不画线；报了 → 100% 贴顶、0% 贴底、空档断开成多段。
const noCache = usageChartSVG([{ t: '2026-09-25T15', prompt_tokens: 10, completion_tokens: 1, total_tokens: 11 }]);
assert.ok(!noCache.includes('hitline') && !noCache.includes('hitdot'),
  '上游没报缓存字段时不该出现命中率线/点（不能拿 0 当 0%）');
const half = usageChartSVG([
  { t: '2026-09-25T15', prompt_tokens: 100, completion_tokens: 1, total_tokens: 101, cache_hit_tokens: 100, cache_samples: 1 },
  { t: '2026-09-25T16', prompt_tokens: 100, completion_tokens: 1, total_tokens: 101, cache_hit_tokens: 0, cache_samples: 1 },
]);
assert.strictEqual((half.match(/<polyline class="hitline"/g) || []).length, 1, '两个有数据的点成一段线');
assert.ok(half.includes('cy="12.0"'), '100% 命中贴顶线 (PT=12): ' + half.match(/cy="[\d.]+"/g));
assert.ok(half.includes('cy="152.0"'), '0% 命中贴基线 (PT+ih=152)');
assert.ok(half.includes('缓存命中率 100%') && half.includes('缓存命中率 0%'), '圆点要带命中率提示');
const broken = usageChartSVG([
  { t: '2026-09-25T15', prompt_tokens: 10, completion_tokens: 1, total_tokens: 11, cache_hit_tokens: 5, cache_samples: 1 },
  { t: '2026-09-25T16', prompt_tokens: 10, completion_tokens: 1, total_tokens: 11 },
  { t: '2026-09-25T17', prompt_tokens: 10, completion_tokens: 1, total_tokens: 11, cache_hit_tokens: 5, cache_samples: 1 },
]);
assert.strictEqual((broken.match(/<polyline class="hitline"/g) || []).length, 0, '被空档隔开的点不成线');
assert.strictEqual((broken.match(/hitdot/g) || []).length, 2, '但每个有数据的点各留一个圆点');
`
	out, err := exec.Command(node, "-e", driver).CombinedOutput()
	if err != nil {
		t.Fatalf("node 断言失败: %v\n%s", err, out)
	}
}
