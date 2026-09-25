// 思考列的渲染自检：直接从 app.js 里抠出 thinkCell/switchLabel 跑断言（不复制一份代码，
// 否则测的是副本）。`node internal/panel/thinkcell_check.mjs`；Go 侧 TestThinkCellRendering 会调它。
//
// 由来：上游 get_detail_param 只给 Thinking.Type 与 reasoning_effort_config 两个信号，
// 实测没有 low/medium/high 档位列表。这里钉住的是「有什么显示什么、没给就明说、将来多出的键别吞掉」。
import { readFileSync } from 'node:fs';
import assert from 'node:assert';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

const appJs = join(dirname(fileURLToPath(import.meta.url)), 'app.js');
const src = readFileSync(appJs, 'utf8');

// 按大括号配平抠函数体，比正则可靠（函数里有正则字面量）。
function grab(name) {
  const i = src.indexOf('function ' + name + '(');
  if (i < 0) throw new Error('app.js 里找不到 ' + name);
  let depth = 0;
  for (let k = src.indexOf('{', i); k < src.length; k++) {
    if (src[k] === '{') depth++;
    else if (src[k] === '}' && --depth === 0) return src.slice(i, k + 1);
  }
  throw new Error(name + ' 括号不配平');
}

const esc = s => String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;')
  .replace(/>/g, '&gt;').replace(/"/g, '&quot;');
const { thinkCell, switchLabel } = new Function(
  'esc', grab('switchLabel') + '\n' + grab('thinkCell') + '\nreturn {thinkCell, switchLabel};')(esc);

// 上游实测的两种形态
assert.match(switchLabel('{"support_thinking":false}'), /^不支持（support_thinking=false）$/);
assert.match(switchLabel('{"support_thinking":true}'), /^支持（support_thinking=true）$/);
// 形状不认识 → 原样，不猜
assert.equal(switchLabel('{"other":1}'), '{"other":1}');
// 上游将来在这段 JSON 里加档位列表 → 必须原样带出来，不能被标签吞掉
const withLevels = switchLabel('{"support_thinking":true,"levels":["low","medium","high"]}');
assert.ok(withLevels.includes('low') && withLevels.includes('medium') && withLevels.includes('high'), withLevels);

assert.ok(thinkCell({}).includes('上游未给'));
assert.ok(!thinkCell({}).includes('思考：'), '没有字段时不该编出思考结论');
assert.ok(thinkCell({ thinking: 'enabled' }).includes('思考：开'));
const d = thinkCell({ reasoning_effort_config: '{"support_thinking":false}' });
assert.ok(d.includes('不支持') && d.includes('title="上游原值：'), d);

console.log('thinkCell/switchLabel 断言全部通过');
