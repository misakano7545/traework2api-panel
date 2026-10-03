package panel

import (
	"os"
	"os/exec"
	"testing"
)

// storedKey/saveKey/clearKey 是这次改动的全部新逻辑，纯 I/O 三行——但「存储被禁用时抛
// SecurityError」那条路径此前没有任何东西覆盖，而它恰恰是最容易写错（漏 try/catch 就
// 变成「每次请求都炸」）。抠出来在 node 里真跑一遍：正常读写、登出清两个 store、
// 存储抛异常时不炸。
func TestKeyStorageHelpersRuntime(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("无 node，跳过存储助手运行时断言")
	}
	raw, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)
	// extractJSFunc 从 "function api(" 起切，`async ` 前缀会丢——await 在非 async 函数里是
	// 语法错误，所以这里补回去（别改共享助手，其他用例靠它的切片行为）。
	apiSrc := "async " + extractJSFunc(t, src, "api")
	driver := `
const assert = require('assert');
const LS_KEY = 'tw2a_key';
// 假存储：可正常读写，也可切换成「访问即抛」（浏览器禁用存储的形态）
let throwOnAccess = false;
const mk = () => ({
  _m: new Map(),
  getItem(k) { if (throwOnAccess) throw new Error('SecurityError'); return this._m.has(k) ? this._m.get(k) : null; },
  setItem(k, v) { if (throwOnAccess) throw new Error('SecurityError'); this._m.set(k, String(v)); },
  removeItem(k) { if (throwOnAccess) throw new Error('SecurityError'); this._m.delete(k); },
});
globalThis.localStorage = mk();
globalThis.sessionStorage = mk();
// api() 的依赖：抓取实际发出的请求头 + 记录钥匙门是否被拉起
let lastHeaders = null, gateOpened = 0, nextStatus = 200;
globalThis.fetch = async (url, opts) => {
  lastHeaders = opts.headers;
  return { status: nextStatus, ok: nextStatus < 400, json: async () => ({ ok: true }) };
};
globalThis.openKey = () => { gateOpened++; };
` + extractJSFunc(t, src, "storedKey") + "\n" +
		extractJSFunc(t, src, "saveKey") + "\n" +
		extractJSFunc(t, src, "clearKey") + "\n" +
		apiSrc + `
// 空存储 → 空串（不是 null/undefined：调用方直接拿去拼 Bearer）
assert.strictEqual(storedKey(), '', '空存储应回空串');
// 写后读：这就是「输一次就记住」的最小验证
saveKey('sk-abc');
assert.strictEqual(storedKey(), 'sk-abc');
// 覆盖写
saveKey('sk-def');
assert.strictEqual(storedKey(), 'sk-def', '换密钥要覆盖旧值');
// 登出：两个 store 都要清干净
globalThis.sessionStorage.setItem(LS_KEY, 'legacy');
clearKey();
assert.strictEqual(storedKey(), '', '登出后读不到密钥');
assert.strictEqual(globalThis.sessionStorage.getItem(LS_KEY), null, '遗留的 sessionStorage 也要清');
// 存储被禁用（抛异常）时：读回空串、写/清静默失败，都不许把异常抛给调用方
throwOnAccess = true;
assert.strictEqual(storedKey(), '', '存储抛异常时 storedKey 必须回空串');
saveKey('x');
clearKey();
throwOnAccess = false;
// api() 必须把存储里的密钥带在请求上 —— 这段是「改造后忘了发头」的唯一拦截点
(async () => {
  saveKey('sk-live');
  await api('overview');
  assert.strictEqual(lastHeaders['Authorization'], 'Bearer sk-live', 'api() 必须带上 Bearer 头');
  assert.strictEqual(lastHeaders['Content-Type'], undefined, 'GET 不该带 Content-Type');
  clearKey();
  await api('overview');
  assert.strictEqual(lastHeaders['Authorization'], undefined, '没密钥时不发 Authorization');
  assert.strictEqual(gateOpened, 0, '正常响应不该拉钥匙门');
  // 401 且密钥没变过 → 拉钥匙门
  saveKey('sk-bad'); nextStatus = 401;
  await api('overview').catch(() => {});
  assert.strictEqual(gateOpened, 1, '401 应拉钥匙门');
  // 401 但发出后密钥已被改成别的 → 不再盖弹窗（原竞态判断要保住）
  nextStatus = 401;
  const inflight = api('overview').catch(() => {});
  saveKey('sk-newer');
  await inflight;
  assert.strictEqual(gateOpened, 1, '密钥已更新时不该再弹钥匙门');
  console.log('ok');
})().catch(e => { console.error(e); process.exit(1); });
`
	out, err := exec.Command(node, "-e", driver).CombinedOutput()
	if err != nil {
		t.Fatalf("node 断言失败: %v\n%s", err, out)
	}
}
