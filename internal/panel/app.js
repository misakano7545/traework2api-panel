'use strict';
/* ── 状态 ─────────────────────────────────────────────────────────── */
/* 密钥放 localStorage，**不是** sessionStorage：sessionStorage 是「每标签页一份、关掉就没」，
   于是每次重开面板都要重输一次——这正是「前端无法本地存储 api_key」的根因。
   WB 仓同处也是 localStorage（LS_KEY），这里对齐。
   注意副作用：面板若经公网隧道访问，这串密钥会长期留在本机浏览器里——「登出」负责清干净。 */
const LS_KEY = 'tw2a_key', LS_THEME = 'tw2a_theme';
/* 密钥的唯一事实源 = localStorage（照 WB 仓：不设模块级缓存，用到就现读）。
   两个存取器保留 try/catch：浏览器禁用存储时 getItem/setItem 会抛 SecurityError，
   不能让它把每次请求带崩（原来散在各处的 try/catch 集中到这里）。
   ponytail: 存储介质必须是 localStorage 不是 sessionStorage，见 LS_KEY 处注释。 */
function storedKey() {
  try { return localStorage.getItem(LS_KEY) || ''; } catch (e) { return ''; }
}
function saveKey(v) {
  try { localStorage.setItem(LS_KEY, v); } catch (e) {}
}
function clearKey() {
  // 两个 store 都清：修复前写进 sessionStorage 的那份遗留值也要带走，登出不留痕。
  try { localStorage.removeItem(LS_KEY); } catch (e) {}
  try { sessionStorage.removeItem(LS_KEY); } catch (e) {}
}
let theme = localStorage.getItem(LS_THEME) || 'auto';   // auto | light | dark
let view = 'accounts';
let cfgLoaded = null;
let logPin = true, logCh = 'all', loginID = '', refTimer = null;
let usageHours = 72;                   // 账号池用量列窗口，由 overview 的 usage_hours 同步
let ready = false;                       // 认证探测通过后才允许拉数据，避免把 401 当加载失败刷 toast

const $ = id => document.getElementById(id);

/* ── 主题 ─────────────────────────────────────────────────────────── */
/* 两态翻转（浅/深），首次访问跟随系统偏好；点击总是切换可见外观，符合直觉。 */
function effTheme() {
  return theme === 'auto' ? (matchMedia('(prefers-color-scheme: light)').matches ? 'light' : 'dark') : theme;
}
function applyTheme() {
  const eff = effTheme();
  document.documentElement.dataset.theme = eff;
  $('icoTheme').innerHTML = eff === 'light'
    ? '<circle cx="8" cy="8" r="3"/><path d="M8 1v2M8 13v2M1 8h2M13 8h2M3.2 3.2l1.4 1.4M11.4 11.4l1.4 1.4M12.8 3.2l-1.4 1.4M4.6 11.4l-1.4 1.4"/>'
    : '<path d="M13.2 9.6A5.6 5.6 0 0 1 6.4 2.8a5.6 5.6 0 1 0 6.8 6.8z"/>';
  $('btnTheme').title = eff === 'light' ? '切换到深色' : '切换到浅色';
}

/* ── 手机抽屉导航（≤760px）─────────────────────────────────────────
   本脚本在 <head> 加载（DOM 未就绪），故：元素惰性查询 + 点击事件委托。 */
function navSet(open) {
  const navEl = document.querySelector('.nav'), scrim = document.getElementById('navScrim');
  if (!navEl || !scrim) return;
  navEl.classList.toggle('open', open);
  scrim.classList.toggle('on', open);
  document.body.classList.toggle('nav-open', open);
  const btn = document.getElementById('btnNav');
  if (btn) btn.setAttribute('aria-expanded', String(open));
}
document.addEventListener('click', e => {
  const t = e.target;
  if (!(t && t.closest)) return;
  if (t.closest('#btnNav')) {
    const n = document.querySelector('.nav');
    navSet(!!(n && !n.classList.contains('open')));
  } else if (t.closest('#navScrim')) {
    navSet(false);
  }
});
addEventListener('keydown', e => { if (e.key === 'Escape') navSet(false); });

/* ── 请求 ─────────────────────────────────────────────────────────── */
async function api(path, opts = {}) {
  const h = Object.assign({}, opts.headers || {});
  const sent = storedKey();               // 本次实际发出的密钥（快照）
  if (sent) h['Authorization'] = 'Bearer ' + sent;
  if (opts.body) h['Content-Type'] = 'application/json';
  const r = await fetch('/panel/api/' + path, Object.assign({}, opts, { headers: h }));
  if (r.status === 401) {
    // 后台轮询发出时还没密钥，这次 401 回来时密钥已填好——不能把弹窗再盖上去。
    if (sent === storedKey()) openKey();
    throw new Error('密钥无效或未填写');
  }
  const d = await r.json().catch(() => ({}));
  if (!r.ok) throw new Error(d.error || ('HTTP ' + r.status));
  return d;
}
function toast(msg, cls) {
  const el = document.createElement('div');
  el.className = 'tst ' + (cls || '');
  el.textContent = msg;
  $('toasts').appendChild(el);
  setTimeout(() => el.remove(), 3600);
}
// esc 文本/属性双安全转义。不能只用 div.innerHTML（它转义 <>& 但不转义引号），
// 否则字符串拼进 HTML 属性（如 title="..."）时引号可闭合属性并注入事件处理器。
// 显式替换 5 个字符：& < > " '（& 必须最先，避免二次转义）。
function esc(s) {
  return String(s == null ? '' : s)
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;')
    .replace(/'/g, '&#39;');
}
function tag(cls, text, title) {
  return '<span class="tag ' + cls + '"' + (title ? ' title="' + esc(title) + '"' : '') + '>' + esc(text) + '</span>';
}

/* ── 密钥门 ───────────────────────────────────────────────────────── */
function openKey() {
  const v = $('keyVeil');
  if (v.classList.contains('on')) return;
  $('keyErr').hidden = true;
  v.classList.add('on');
  setTimeout(() => $('keyInput').focus(), 60);
}
async function submitKey() {
  const v = $('keyInput').value.trim();
  if (!v) return;
  saveKey(v);
  try {
    await api('overview');                // 认证探测成功才关弹窗
    $('keyErr').hidden = true;
    $('keyVeil').classList.remove('on');
    start();
  } catch (e) {
    $('keyErr').hidden = false;
  }
}

/* ── 路由 ─────────────────────────────────────────────────────────── */
const TITLES = { accounts: '账号池', models: '模型', usage: '用量', packages: '积分构成', config: '配置', logs: '运行日志' };
function go(v) {
  navSet(false);
  view = v;
  document.querySelectorAll('.view').forEach(s => s.hidden = s.id !== 'view-' + v);
  document.querySelectorAll('.nav a').forEach(a => a.classList.toggle('on', a.dataset.view === v));
  $('ttl').textContent = TITLES[v];
  if (!ready) return;                    // 认证探测通过后 start() 会再调一次
  if (v === 'accounts') loadOverview(true);
  if (v === 'models' && !$('mdBody').children.length) loadModels();
  if (v === 'usage') loadUsage(true);
  if (v === 'packages') loadPackages();
  if (v === 'config') loadConfig();
  if (v === 'logs') loadLogs();
}

/* ── 账号池 ───────────────────────────────────────────────────────── */
// credTitle 积分列提示：解释「0」是什么。
// 国际版免费号（Free plan，credits_limit=0、按美元计费）恒显示 0——不说明清楚会被当成
// 「接口坏了/没查到」，而这其实是上游的真实状态（付费国际号会带面额）。
function credTitle(a) {
  if (a.credits_total) return '剩余 / 总额（上游积分包）';
  if (a.realm === 'intl') return '国际版该账号无积分套餐（Free plan，按量计费）：0 是上游返回的真实值，不是未查询';
  return '剩余 / 总额（上游积分包）';
}
// 到期列：0 = 上游没给到期信息，不猜。临近/已过期才上标签，正常不动声色。
function expCell(unix) {
  if (!unix) return '<span style="color:var(--ink-3)">—</span>';
  const d = new Date(unix * 1000);
  const days = Math.ceil((d - Date.now()) / 86400000);
  const md = (d.getMonth() + 1) + '-' + String(d.getDate()).padStart(2, '0');
  let s = '<span class="num">' + md + '</span>';
  if (days < 0) s += ' ' + tag('bad', '已过期');
  else if (days <= 3) s += ' ' + tag('warn', '剩 ' + days + ' 天');
  return s;
}

/* 用量列：与用量视图同源（近 usageHours 小时的台账汇总）。无调用记录时不显示假零——
   「0 次」会被读成「调过但失败了，这个号有问题」。 */
function usageCell(u) {
  const none = '<td class="num usage-cell" title="近 ' + usageHours + ' 小时没有调用记录">' +
    '<span style="color:var(--ink-3)">—</span></td>';
  if (!u || !u.requests) return none;
  const tok = u.total_tokens ? fmtNum(u.total_tokens) : '—';
  const title = '近 ' + usageHours + ' 小时：' + fmtNum(u.requests) + ' 次调用' +
    (u.errors ? '（失败 ' + fmtNum(u.errors) + '）' : '') +
    ' / ' + tok + ' tok / 平均延迟 ' + fmtLat(u.avg_latency_ms) +
    ' / ' + fmtRate(u.avg_tokens_per_second);
  const t = esc(title);
  return '<td class="num usage-cell" title="' + t + '">' +
    '<span class="usage-line" aria-label="' + t + '">' +
    '<span class="usage-item usage-count"><b>' + fmtNum(u.requests) + '</b><em>次</em></span>' +
    '<span class="usage-item usage-total"><b>' + tok + '</b><em>tok</em></span>' +
    '<span class="usage-item usage-latency"><b>' + fmtLat(u.avg_latency_ms) + '</b></span>' +
    '<span class="usage-item usage-rate"><b>' + fmtRate(u.avg_tokens_per_second) + '</b></span>' +
    '</span></td>';
}

/* 相对时间（「最近成功」列）。Go 的零值时间会序列化成 0001-01-01，按"没记录"处理。 */
function ago(t) {
  if (!t) return '—';
  const d = new Date(t);
  if (isNaN(d) || d.getFullYear() < 2000) return '—';
  const s = Math.floor((Date.now() - d) / 1000);
  if (s < 60) return '刚刚';
  const m = Math.floor(s / 60);
  if (m < 60) return m + ' 分钟前';
  const h = Math.floor(m / 60);
  return h < 24 ? h + ' 小时前' : Math.floor(h / 24) + ' 天前';
}

function renderAccounts(list) {
  const tb = $('accBody');
  if (!list.length) {
    tb.innerHTML = '<tr><td colspan="10"><div class="empty"><div class="big">账号池是空的</div>点击右上角「添加账号」，用浏览器登录一个 Trae 账号</div></td></tr>';
    return;
  }
  tb.innerHTML = list.map(a => {
    const cls = a.disabled ? 'off' : (a.cooling ? 'cool' : '');
    const st = [];
    if (a.disabled) st.push(tag('bad', '已禁用'));
    else if (a.cooling) st.push(tag('warn', '冷却中', a.reason));
    else st.push(tag('ok', '可用'));
    // 签到域冷却与网关冷却分开显示：签到接口拥塞不影响对话选号，反之亦然。
    if (!a.disabled && a.checkin_cooling) {
      st.push(tag('warn', '签到冷却', a.checkin_reason || '签到失败，已暂停自动签到'));
    }
    // 国际版**没有签到**（EpCheckin* 在国际域全 404）：签到按钮置灰 + 提示。
    // 「刷新」不再置灰：积分/套餐在国际版走 v1 路径（EpEntUsageIntl），能取到到期时间；
    // 免费号返回的 0 积分是真实状态，不是接口不可用。
    const intlOff = a.realm === 'intl' ? ' disabled title="国际版无签到"' : '';
    const acts = ['<button class="xs" data-a="checkin" data-u="' + esc(a.uid) + '"' + intlOff + '>签到</button>',
      '<button class="xs" data-a="balance" data-u="' + esc(a.uid) + '">刷新</button>'];
    if (a.cooling) acts.push('<button class="xs" data-a="clear-cooldown" data-u="' + esc(a.uid) + '">解除冷却</button>');
    if (a.disabled) acts.push('<button class="xs" data-a="enable" data-u="' + esc(a.uid) + '">启用</button>');
    if (!a.disabled) acts.push('<button class="xs danger" data-a="disable" data-u="' + esc(a.uid) + '">禁用</button>');
    acts.push('<button class="xs danger" data-a="remove" data-u="' + esc(a.uid) + '">移除</button>');
    return '<tr class="' + cls + '"><td class="mark" aria-hidden="true"><i></i></td>' +
      '<td class="who"><div class="nm">' + (a.realm === 'intl' ? '<span class="tag mute">国际</span> ' : '') + esc(a.nickname || a.uid) + '</div><div class="id">' + esc(a.uid) + '</div></td>' +
      '<td>' + st.join(' ') + '</td>' +
      '<td class="cred" title="' + esc(credTitle(a)) + '"><div class="n">' + (a.credits || 0) +
        (a.credits_total ? '<span class="tot">/' + a.credits_total + '</span>' : '') + '</div></td>' +
      '<td>' + expCell(a.expire) + '</td>' +
      '<td class="num">' + (a.err_count || 0) + '</td>' +
      '<td class="num" title="累计成功 / 失败（自记录起）">' + (a.success_count || 0) +
        ' <span style="color:var(--ink-3)">/</span> <span style="color:var(--bad)">' + (a.err_total || 0) + '</span></td>' +
      '<td class="num">' + (a.in_flight || 0) + '</td>' +
      usageCell(a.usage) +
      '<td class="num" style="color:var(--ink-3)">' + ago(a.last_success) + '</td>' +
      '<td class="c-acts"><div class="acts">' + acts.join('') + '</div></td></tr>';
  }).join('');
}

async function loadOverview(quiet) {
  try {
    const d = await api('overview');
    const list = d.accounts || [];
    if (d.usage_hours) usageHours = d.usage_hours;
    $('sTotal').textContent = d.total;
    $('sHealthy').textContent = d.healthy;
    $('sCooling').textContent = d.cooling;
    $('sDisabled').textContent = d.disabled;
    const cSum = list.reduce((s, a) => s + (a.credits || 0), 0);
    const cTot = list.reduce((s, a) => s + (a.credits_total || 0), 0);
    $('sCredits').textContent = cSum + (cTot ? '/' + cTot : '');
    $('sCheckin').textContent = list.filter(a => a.checkin_cooling && !a.disabled).length;
    $('navSub').textContent = 'v' + d.version;
    $('navVer').textContent = 'v' + d.version;
    $('navState').textContent = d.healthy > 0 ? '服务正常' : (d.total ? '无可用账号' : '待添加账号');
    $('navPulse').className = 'pulse' + (d.healthy > 0 ? '' : (d.total ? ' warn' : ' bad'));
    $('accNote').textContent = d.auth_required ? '已启用密钥鉴权' : '本机免鉴权';
    const up = Math.floor(d.uptime_sec || 0);
    $('subMeta').textContent = '运行 ' + (up >= 86400 ? Math.floor(up / 86400) + ' 天 ' : '') +
      Math.floor(up % 86400 / 3600) + ' 时 ' + Math.floor(up % 3600 / 60) + ' 分';
    renderAccounts(list);
  } catch (e) { if (!quiet) toast(e.message, 'err'); }
}

/* ── 模型 ─────────────────────────────────────────────────────────── */
/* 思考列：上游这条接口**没有** low/medium/high 档位列表（2026-09 实测 15 个官方模型：
   kimi 三兄弟给 model_extra_config.Thinking.Type=enabled，Doubao 三条给 reasoning_effort_config={"support_thinking":false}，
   其余 12 条两个字段都没有；顶层再无其它 effort/thinking 键）。
   所以不编档位：有什么说什么，原值塞进 title，什么都没有就明说「上游未给」。 */
function thinkCell(m) {
  const raw = [];
  if (m.thinking) raw.push('thinking=' + m.thinking);
  if (m.reasoning_effort_config) raw.push(m.reasoning_effort_config);
  if (!raw.length) {
    return '<span class="tag mute" title="上游没给思考字段，也没有 low/medium/high 档位列表">上游未给</span>';
  }
  const parts = [];
  if (m.thinking) parts.push('思考：' + (m.thinking === 'enabled' ? '开' : m.thinking));
  if (m.reasoning_effort_config) parts.push('思考开关：' + switchLabel(m.reasoning_effort_config));
  return '<span class="tag mute" title="上游原值：' + esc(raw.join(' · ')) + '">' + esc(parts.join(' · ')) + '</span>';
}
/* support_thinking:true/false → 支持/不支持；形状不认识就原样显示，不猜。
   上游哪天在这段 JSON 里加了档位列表之类的其它键，就把它原样补在后面，别被这个标签吞掉。 */
function switchLabel(cfg) {
  const m = /"support_thinking"\s*:\s*(true|false)/.exec(cfg);
  if (!m) return cfg;
  const label = (m[1] === 'true' ? '支持' : '不支持') + '（support_thinking=' + m[1] + '）';
  const rest = cfg.replace(/"support_thinking"\s*:\s*(?:true|false)\s*,?/, '').replace(/^\{\s*|\s*\}$/g, '').trim();
  return rest ? label + ' ' + cfg : label;
}

/* ── 模型筛选/排序/搜索（抄自 WorkBuddy 面板，字段按 TRAE 载荷裁到实有项）─────
   模型目录一次拉全（十几条），筛选与排序全部在前端完成：改条件零延迟，也不会
   因为调一次筛选就打一次上游——/panel/api/models 是直连上游的实时查询，很贵。
   条件之间是 AND；每个条件为空即不参与判定。 */
let mdAll = [];
let mdFilter = { q: '', realm: '', fn: '', sort: 'default' };
// 通道（function）展示名：solo_work_lite = Work、solo_coder = Coder。
const FN_LABEL = { solo_work_lite: 'Work', solo_coder: 'Coder' };
function fnLabel(fn) { return FN_LABEL[fn] || fn || '—'; }
// mdSearchText 参与关键字搜索的字段（ID / 展示名 / 能力）。
function mdSearchText(m) {
  return [m.id, m.name, m.capability].filter(Boolean).join(' ').toLowerCase();
}
// mdMatch 单个模型是否满足全部筛选条件。
function mdMatch(m, f) {
  f = f || mdFilter;
  if (f.q) {
    const text = mdSearchText(m);
    // 空格分词后逐个匹配：多关键词是 AND，便于「cn work」这类组合查询。
    for (const kw of f.q.toLowerCase().split(/\s+/).filter(Boolean)) {
      if (!text.includes(kw)) return false;
    }
  }
  if (f.realm && !String(m.id || '').startsWith(f.realm + ':')) return false;
  if (f.fn && m.function !== f.fn) return false;
  return true;
}
// mdSortList 按当前排序条件返回新数组（不改动入参，保持上游原始顺序可回溯）。
function mdSortList(list, f) {
  f = f || mdFilter;
  const out = list.slice();
  const num = v => { const n = Number(v || 0); return Number.isFinite(n) ? n : 0; };
  if (f.sort === 'context') out.sort((a, b) => num(b.context_length) - num(a.context_length));
  else if (f.sort === 'output') out.sort((a, b) => num(b.max_output_tokens) - num(a.max_output_tokens));
  else if (f.sort === 'name') out.sort((a, b) => String(a.id || '').localeCompare(String(b.id || '')));
  return out;
}
// mdRowHtml 单个模型行（纯渲染，便于独立测试）。
function mdRowHtml(m) {
  const tip = m.name ? ' title="' + esc(m.name) + '"' : '';
  return '<tr><td class="mark" aria-hidden="true"><i></i></td>' +
    '<td class="who"' + tip + '><div class="nm">' + esc(m.id) + '</div>' +
    (m.name ? '<div class="id">' + esc(m.name) + '</div>' : '') + '</td>' +
    '<td>' + (m.capability ? tag('mute', m.capability) : '—') + '</td>' +
    '<td>' + thinkCell(m) + '</td>' +
    '<td>' + tag('mute', fnLabel(m.function)) + '</td>' +
    '<td class="num">' + (m.context_length ? Math.round(m.context_length / 1000) + 'K' : '—') + '</td>' +
    '<td class="num">' + (m.max_output_tokens ? Math.round(m.max_output_tokens / 1000) + 'K' : '—') + '</td></tr>';
}
function renderModels() {
  const tb = $('mdBody');
  const list = mdSortList(mdAll.filter(m => mdMatch(m)));
  if (!list.length) {
    tb.innerHTML = '<tr><td colspan="7"><div class="empty">' +
      (mdAll.length ? '没有符合当前筛选条件的模型' : '上游未返回模型') + '</div></td></tr>';
  } else {
    tb.innerHTML = list.map(m => mdRowHtml(m)).join('');
  }
  const filtered = list.length !== mdAll.length;
  $('mdCount').textContent = !mdAll.length ? ''
    : filtered ? '命中 ' + list.length + ' / ' + mdAll.length + ' 个模型'
      : mdAll.length + ' 个模型';
  $('mdCount').className = filtered ? 'note src-off' : 'note';
}
function resetModelFilter() {
  mdFilter = { q: '', realm: '', fn: '', sort: 'default' };
  $('mdQ').value = ''; $('mdRealm').value = ''; $('mdFn').value = ''; $('mdSort').value = 'default';
  renderModels();
}
async function loadModels() {
  const tb = $('mdBody');
  tb.innerHTML = '<tr><td colspan="7"><div class="empty">正在向上游查询…</div></td></tr>';
  try {
    const d = await api('models');
    mdAll = d.data || [];
    if (!mdAll.length) {
      $('mdCount').textContent = '';
      $('mdNote').textContent = '上游未返回模型';
      renderModels();
      return;
    }
    $('mdNote').textContent = '实时查询上游 · 路由层缓存 1 小时';
    renderModels();
  } catch (e) {
    mdAll = [];
    tb.innerHTML = '<tr><td colspan="7"><div class="empty">' + esc(e.message) + '</div></td></tr>';
    $('mdCount').textContent = '';
    $('mdNote').textContent = '查询失败';
  }
}

/* ── 时间范围控件 + 请求记录（抄自 WorkBuddy 面板，逐字对齐）───────────────────
   搬的是整块：请求记录卡（指标行 + 筛选栏 + 10 列表）+ 时间范围控件。两仓的 CSS 同源
   （WB 那 344 条选择器本仓全有），所以 HTML/类名可以原样用，不需要配样式。
   数据侧本仓已就绪：/panel/api/request_logs 返回 metrics+entries，筛选参数与归档读盘
   共用 reqlog.Filter（见 internal/panel/panel.go）。 */
/* ── 时间范围控件（用量 / 请求记录共用）────────────────────────────────
   预设项：今天 / 近 24 小时 / 近 3 天 / 近 7 天 / 近 30 天 / 全部历史 / 自定义。

   为什么区间一律由前端算好再发：
     - 「今天」必须是**浏览器本地时区**的 00:00 起。服务端时区未必与浏览器一致
       （容器常挂 TZ=Asia/Shanghai，而浏览器可能在任何时区），让服务端算"今天"
       会在跨时区时切错日子。
     - 「自定义」本来就是用户挑的具体时刻，没有任何服务端推导空间。

   滚动预设（近 N 小时/天）则保留 hours 参数：服务端按整点对齐的滚动窗口与旧
   行为逐位一致，前端自己减 N 小时会多算/少算一个边界桶。 */
const TRANGE_PRESETS = [
  ['today', '今天'],
  ['24', '近 24 小时'],
  ['72', '近 3 天'],
  ['168', '近 7 天'],
  ['720', '近 30 天'],
  ['0', '全部历史'],
  ['custom', '自定义…'],
];
const TRANGE_DEFAULT = '72';
const trangeStates = new Map(); // hostId → { preset, from: Date|null, to: Date|null }

// dtLocalValue / dtLocalParse 与 <input type=datetime-local> 的取值格式互转
// （YYYY-MM-DDTHH:mm，本地时区；ES 里"带时间的日期串"按本地解析，正是我们要的）。
function dtLocalValue(d) {
  const p = n => String(n).padStart(2, '0');
  return d.getFullYear() + '-' + p(d.getMonth() + 1) + '-' + p(d.getDate()) + 'T' +
    p(d.getHours()) + ':' + p(d.getMinutes());
}
function dtLocalParse(s) {
  if (!s) return null;
  const d = new Date(s);
  return isNaN(d.getTime()) ? null : d;
}

// trangeMidnight 今天 00:00（本地时区）。
function trangeMidnight() {
  const d = new Date();
  d.setHours(0, 0, 0, 0);
  return d;
}

function trangeState(id) {
  if (!trangeStates.has(id)) {
    // 「自定义」的初始值给一段有意义的默认：今天 00:00 → 现在。
    trangeStates.set(id, { preset: TRANGE_DEFAULT, from: trangeMidnight(), to: new Date() });
  }
  return trangeStates.get(id);
}

// trangeRender 画出控件骨架（幂等：重复调用会保留当前状态）。
function trangeRender(id) {
  const host = $(id);
  if (!host) return;
  const st = trangeState(id);
  const custom = st.preset === 'custom';
  host.innerHTML =
    '<select class="tr-preset" aria-label="时间范围">' +
    TRANGE_PRESETS.map(([v, label]) =>
      '<option value="' + v + '"' + (v === st.preset ? ' selected' : '') + '>' + esc(label) + '</option>').join('') +
    '</select>' +
    '<span class="tr-custom"' + (custom ? '' : ' hidden') + '>' +
    '<input type="datetime-local" class="tr-from" value="' + esc(st.from ? dtLocalValue(st.from) : '') + '" aria-label="起始时间">' +
    '<span class="tr-sep">→</span>' +
    '<input type="datetime-local" class="tr-to" value="' + esc(st.to ? dtLocalValue(st.to) : '') + '" aria-label="结束时间">' +
    '</span>';
  const preset = host.querySelector('.tr-preset');
  if (preset) preset.onchange = () => {
    st.preset = preset.value;
    // 从别的预设切到自定义时，把区间重置为"今天 00:00 → 现在"，
    // 免得用户上次留下的半年区间被无声沿用。
    if (st.preset === 'custom' && (!st.from || !st.to)) { st.from = trangeMidnight(); st.to = new Date(); }
    trangeRender(id);
    trangeEmit(id);
  };
  const fromEl = host.querySelector('.tr-from');
  const toEl = host.querySelector('.tr-to');
  const readCustom = () => {
    st.from = dtLocalParse(fromEl.value);
    st.to = dtLocalParse(toEl.value);
    // 起止颠倒就地标红（不静默纠正：用户可能正输到一半）。
    const bad = st.from && st.to && st.from > st.to;
    fromEl.classList.toggle('tr-bad', !!bad);
    toEl.classList.toggle('tr-bad', !!bad);
    if (bad) return;
    trangeEmit(id);
  };
  if (fromEl) fromEl.onchange = readCustom;
  if (toEl) toEl.onchange = readCustom;
}

const trangeHandlers = new Map();
// trangeBind 渲染控件并登记变化回调。**不**在绑定时触发回调：各视图的首次加载
// 由 go() 统一驱动，这里再触发一次会让打开页面时打两遍接口。
function trangeBind(id, onChange, preset) {
  trangeHandlers.set(id, onChange);
  if (preset) trangeState(id).preset = preset;
  trangeRender(id);
}
function trangeEmit(id) {
  const fn = trangeHandlers.get(id);
  if (fn) fn();
}

// trangeQuery 把当前选择翻译成查询参数。
//   rolling=true  → 滚动预设发 hours（服务端整点对齐），今天/自定义发 from/to
//   rolling=false → 一律发 from/to（归档是线性日志，前端算区间更直观）
// 「全部历史」两者都不发。
function trangeQuery(id, rolling) {
  const st = trangeState(id);
  const q = new URLSearchParams();
  const sec = d => Math.floor(d.getTime() / 1000);
  if (st.preset === 'custom') {
    if (st.from) q.set('from', sec(st.from));
    if (st.to) q.set('to', sec(st.to));
    return q;
  }
  if (st.preset === 'today') {
    q.set('from', sec(trangeMidnight()));
    return q;
  }
  if (st.preset === '0') return q;
  if (rolling) { q.set('hours', st.preset); return q; }
  q.set('from', sec(new Date(Date.now() - Number(st.preset) * 3600 * 1000)));
  return q;
}

// trangeLabel 人读口径，用于「用量总览」右上角这类需要回显区间的位置。
function trangeLabel(id) {
  const st = trangeState(id);
  const found = TRANGE_PRESETS.find(p => p[0] === st.preset);
  if (st.preset !== 'custom') return found ? found[1] : '';
  if (!st.from && !st.to) return '自定义';
  const f = d => d ? (d.getMonth() + 1) + '-' + String(d.getDate()).padStart(2, '0') + ' ' +
    String(d.getHours()).padStart(2, '0') + ':' + String(d.getMinutes()).padStart(2, '0') : '…';
  return f(st.from) + ' → ' + f(st.to);
}

function fmtMs(ms) {
  ms = Number(ms || 0);
  if (!ms) return '—';
  if (ms >= 1000) return (ms / 1000).toFixed(2) + 's';
  return Math.round(ms) + 'ms';
}

function fmtBytes(bytes) {
  const n = Number(bytes || 0);
  if (n < 1024) return n + ' B';
  if (n < 1024 * 1024) return (n / 1024).toFixed(1) + ' KB';
  return (n / 1024 / 1024).toFixed(1) + ' MB';
}

function trimFixed(s) {
  if (!String(s).includes('.')) return String(s);
  return String(s).replace(/0+$/, '').replace(/\.$/, '');
}

function cacheRateText(hit, miss) {
  const h = Number(hit || 0), m = Number(miss || 0), total = h + m;
  if (!total) return '—';
  return String(Math.round(h / total * 1000) / 10) + '%';
}

function usStat(v, k, cls) {
  return '<div class="stat ' + (cls || '') + '"><div class="v">' + esc(v) +
         '</div><div class="k">' + esc(k) + '</div></div>';
}

function renderRequestMetrics(m, entries) {
  m = m || {};
  const a = m.archive || {};
  $('reqStats').innerHTML =
    usStat(fmtTok(m.completed), '已完成') +
    usStat(m.success_rate == null ? '—' : Number(m.success_rate).toFixed(1) + '%', '完成成功率') +
    usStat(m.http_success_rate == null ? '—' : Number(m.http_success_rate).toFixed(1) + '%', 'HTTP 成功率') +
    usStat(fmtMs(m.avg_duration_ms), '平均耗时') +
    usStat(String(m.in_flight || 0), '进行中') +
    usStat(fmtTok(a.files), '归档文件');
  $('reqNote').textContent = a.enabled
    ? 'JSONL 归档 ' + fmtBytes(a.bytes) + (a.dropped_writes ? ' · 丢弃 ' + a.dropped_writes + ' 条' : '') +
      (a.last_error ? ' · 错误：' + a.last_error : '')
    : '仅内存指标，JSONL 归档已关闭';

  reqEntries = entries || [];
  renderRequestTable();
}

function reqMatch(e, f) {
  f = f || reqFilter;
  if (f.outcome && String(e && e.outcome || '') !== f.outcome) return false;
  if (f.q) {
    const text = [e && e.client_ip, e && e.user_agent, e && e.model, e && e.account, e && e.request_id]
      .filter(Boolean).join(' ').toLowerCase();
    for (const kw of f.q.toLowerCase().split(/\s+/).filter(Boolean)) {
      if (!text.includes(kw)) return false;
    }
  }
  return true;
}

function reqOutcomeTag(e) {
  const outcome = String(e && e.outcome || '');
  const label = { success: '成功', http_error: 'HTTP 错误', stream_error: '流错误', interrupted: '中断' }[outcome] || outcome || '—';
  const cls = outcome === 'success' ? 'ok'
    : outcome === 'interrupted' ? 'warn'
    : outcome ? 'bad' : 'mute';
  return '<span class="tag ' + cls + '">' + esc(String(e && e.status || '—') + ' ' + label) + '</span>';
}

function reqTokenCell(e) {
  const total = Number(e && e.total_tokens || 0) ||
    (Number(e && e.prompt_tokens || 0) + Number(e && e.completion_tokens || 0));
  return total ? fmtTok(total) : '—';
}

function reqCreditCell(e) {
  if (!e || !e.credit_known) return '<span class="muted">—</span>';
  const v = Number(e.credit);
  return Number.isFinite(v) ? trimFixed(v.toFixed(2)) : '<span class="muted">—</span>';
}

function renderRequestTable() {
  const list = reqEntries.filter(e => reqMatch(e));
  const tb = $('reqBody');
  if (!tb) return;
  tb.innerHTML = list.map(e => {
    const when = e && e.time ? new Date(e.time).toLocaleTimeString('zh-CN', { hour12: false }) : '—';
    const ip = e && e.client_ip ? e.client_ip : '';
    const ua = e && e.user_agent ? e.user_agent : '';
    const rid = e && e.request_id ? e.request_id : '';
    return '<tr title="' + esc(requestLogText(e)) + '">' +
      '<td class="num">' + esc(when) + '</td>' +
      '<td>' + reqOutcomeTag(e) + '</td>' +
      '<td>' + esc(e && e.model || '—') + '</td>' +
      '<td>' + esc(e && e.account || '—') + '</td>' +
      '<td>' + (ip ? '<span class="clip ip" title="' + esc(ip) + '">' + esc(ip) + '</span>' : '<span class="muted">—</span>') + '</td>' +
      '<td>' + (ua ? '<span class="clip" title="' + esc(ua) + '">' + esc(ua) + '</span>' : '<span class="muted">—</span>') + '</td>' +
      '<td class="num">' + fmtMs(e && e.duration_ms) + '</td>' +
      '<td class="num">' + reqTokenCell(e) + '</td>' +
      '<td class="num">' + reqCreditCell(e) + '</td>' +
      '<td>' + (rid ? '<span class="clip rid" title="' + esc(rid) + '">' + esc(rid) + '</span>' : '<span class="muted">—</span>') + '</td>' +
      '</tr>';
  }).join('') || '<tr><td colspan="10" class="empty">' +
      (reqEntries.length ? '没有符合当前筛选条件的请求记录' : '暂无请求记录') + '</td></tr>';

  const filtered = list.length !== reqEntries.length;
  // 归档里的旧条目没有来源字段（该功能上线前写入）：这时提示开关/历史原因，
  // 而不是让人以为筛选坏了。
  const hasSource = reqEntries.some(e => e && (e.client_ip || e.user_agent));
  $('reqCount').textContent = !reqEntries.length ? ''
    : (filtered ? '命中 ' + list.length + ' / ' + reqEntries.length + ' 条' : reqEntries.length + ' 条') +
      (hasSource ? '' : ' · 来源未记录');
  $('reqCount').className = (filtered || !hasSource) ? 'note src-off' : 'note';
}

function requestLogText(e) {
  const when = e && e.time ? new Date(e.time).toLocaleTimeString('zh-CN', { hour12: false }) : '—';
  const outcomeLabel = { success: '成功', http_error: 'HTTP 错误', stream_error: '流错误', interrupted: '中断' };
  const token = Number(e && e.total_tokens || 0) ||
    (Number(e && e.prompt_tokens || 0) + Number(e && e.completion_tokens || 0));
  let credit = 'credit —';
  if (e && e.credit_known) {
    const value = Number(e.credit);
    if (Number.isFinite(value)) credit = String(Number(value.toFixed(2))) + ' credit';
  }
  return [
    when,
    String(e && e.status || '—') + ' ' + (outcomeLabel[e && e.outcome] || (e && e.outcome) || '—'),
    e && e.model || '—',
    e && e.account || '—',
    e && e.client_ip || '—',
    e && e.user_agent || '—',
    fmtMs(e && e.duration_ms),
    fmtTok(token) + ' tok',
    credit,
    cacheRateText(e && e.cache_hit_tokens, e && e.cache_miss_tokens) === '—' ? '' : '命中 ' + cacheRateText(e && e.cache_hit_tokens, e && e.cache_miss_tokens),
    e && e.request_id || '—',
  ].filter(Boolean).join(' | ');
}

/* 请求记录筛选控件（与 WB 同款：搜索防抖 150ms——最多 1000 行重渲染，不必每键一次）。 */
let reqQTimer = null;
let reqFilter = { q: '', outcome: '' };
let reqEntries = [];
function bindRequestLogControls() {
  if ($('reqQ')) $('reqQ').oninput = () => {
    clearTimeout(reqQTimer);
    reqQTimer = setTimeout(() => { reqFilter.q = $('reqQ').value.trim(); renderRequestTable(); }, 150);
  };
  if ($('reqOutcome')) $('reqOutcome').onchange = () => {
    reqFilter.outcome = $('reqOutcome').value;
    renderRequestTable();
  };
  if ($('reqLimit')) $('reqLimit').onchange = loadLogs;
  if ($('btnReqReload')) $('btnReqReload').onclick = loadLogs;
  // 时间范围默认「全部历史」：请求记录的历史行为就是"取最近 N 条"，默认收窄会让
  // 打开页面时看到的条数凭空变少。
  if ($('reqRange')) trangeBind('reqRange', loadLogs, '0');
}

/* ── 日志（频道：全部/任务/对话/系统）─────────────────────────────── */
async function loadLogs() {
  const box = $('logBox');
  const atEnd = box.scrollTop + box.clientHeight >= box.scrollHeight - 24;
  const limit = ($('reqLimit') && $('reqLimit').value) || 100;
  // 时间范围由归档侧过滤（不是前端筛已拉取的条目）：区间落在更早的时间段时，
  // 「最近 N 条」里根本不会有那些记录，必须让服务端按时间取。
  const rq = trangeQuery('reqRange', false);
  rq.set('limit', limit);
  try {
    const [d, requestRows] = await Promise.all([
      api('logs'),
      api('request_logs?' + rq.toString()).catch(() => ({ metrics: {}, entries: [] })),
    ]);
    // 条目一律用服务端这一次返回的 entries：本仓 /panel/api/request_logs 在归档关闭时
    // 会**自己**回落到进程内最近事件并按同一套条件筛选（见 internal/panel/panel.go）。
    // 与 WB 面板唯一的差别就在这里——WB 的后端在归档关闭时不回落（回空），所以它必须
    // 自己改读 metrics.recent；本仓读了就会把筛选条件之外的请求显示出来。
    const metrics = requestRows.metrics || {};
    const recent = requestRows.entries || [];
    renderRequestMetrics(metrics, recent);
    const all = d.entries || [];
    const entries = all.filter(e => logCh === 'all' || e.ch === logCh);
    box.innerHTML = entries.length
      ? entries.map(e => {
        const lvl = /error|失败|错误/.test(e.text) ? ' e' : /warn|冷却|熔断/.test(e.text) ? ' w' : '';
        const ts = e.ts ? new Date(e.ts).toLocaleTimeString('zh-CN', { hour12: false }) : '';
        const ch = logCh === 'all'
          ? '<i class="lch c-' + esc(e.ch) + '">' + esc({ task: '任务', chat: '对话', sys: '系统' }[e.ch] || e.ch) + '</i>'
          : '';
        return '<span class="ln' + lvl + '">' + ch + esc(ts + ' ' + e.text) + '</span>';
      }).join('')
      : '<span style="color:var(--ink-3)">暂无日志</span>';
    if (logPin && atEnd) box.scrollTop = box.scrollHeight;
    const counts = {};
    for (const e of all) counts[e.ch] = (counts[e.ch] || 0) + 1;
    $('logNote').textContent = logCh === 'all'
      ? '任务 ' + (counts.task || 0) + ' · 对话 ' + (counts.chat || 0) + ' · 系统 ' + (counts.sys || 0)
      : ({ task: '任务', chat: '对话', sys: '系统' }[logCh] || logCh) + ' ' + entries.length + ' 行';
  } catch (e) { /* 概览已提示 */ }
}

/* ── 用量 ─────────────────────────────────────────────────────────── */
/* 口径：窗口（hours）是**唯一**筛选，卡片汇总/按模型/按账号/时序全部按它聚合，切窗口
   时所有数字一起变。失败尝试也计入请求数——重试放大正是靠这一列才在面板里看得见。
   这里只放网关自己记的调用用量；积分余额是上游查询值，不进这张表混算。 */
let usHours = '72';

function fmtNum(n) { return Number(n || 0).toLocaleString('en-US'); }
function fmtLat(v) { return v > 0 ? Math.round(v) + ' ms' : '—'; }
function fmtRate(v) { return v > 0 ? (v >= 100 ? Math.round(v) : v.toFixed(1)) + ' tok/s' : '—'; }

/* usPct 占比文案（0 值不显示 "0.0%"，直接 —，避免一行全是零）。 */
/* fmtCredits 估算积分：小值（一次小请求）保留到 4 位才看得见，大值砍到 2 位。
   0 / 缺值显示 —：0 积分与「这个模型没收录单价」在界面上不该混淆，后者带 title。 */
function fmtCredits(v) {
  const n = Number(v || 0);
  if (!Number.isFinite(n) || n === 0) return '<span style="color:var(--ink-3)" title="没有可计价的调用，或该模型未收录单价">—</span>';
  const s = n >= 10 ? n.toFixed(2) : n >= 0.01 ? n.toFixed(3) : n.toFixed(4);
  return s.replace(/\.?0+$/, '');
}

function usPct(part, total) {
  const t = Number(total || 0);
  if (!t) return '—';
  return (Number(part || 0) / t * 100).toFixed(1) + '%';
}

/* usKpi 用量页的指标卡。比账号池的 .stat 多两样：语义色轨（cls）与副标题（sub，
   放「占比 / 均速率」这类解释性数字）；bar 是卡片内的构成条 HTML，只有需要时才传。 */
function usKpi(v, k, cls, sub, bar) {
  return '<div class="kpi ' + (cls || '') + '">' +
    '<div class="k">' + esc(k) + '</div>' +
    '<div class="v">' + esc(v) + '</div>' +
    (bar || '') +
    (sub ? '<div class="s">' + esc(sub) + '</div>' : '') +
    '</div>';
}

/* usMixBar prompt/completion 占比条。宽度按百分比而不是固定像素：表列宽随窗口变化。 */
function usMixBar(prompt, completion, total) {
  const t = Number(total || 0);
  if (!t) return '';
  const pp = Math.max(0, Math.min(100, Number(prompt || 0) / t * 100));
  const pc = Math.max(0, Math.min(100, Number(completion || 0) / t * 100));
  return '<span class="us-mix" title="prompt ' + pp.toFixed(1) + '% · completion ' + pc.toFixed(1) + '%">' +
    '<i class="p" style="width:' + pp.toFixed(2) + '%"></i>' +
    '<i class="c" style="width:' + pc.toFixed(2) + '%"></i>' +
    '</span>';
}

/* fmtHit 缓存命中率单元格。口径与图表一致：hit / prompt，且**只在有样本的桶上算**。
   上游没报（cache_samples=0）时显示 —，不拿 0 当 0%——「没观测到」不是「全未命中」。 */
function fmtHit(a) {
  const s = Number((a && a.cache_samples) || 0);
  const pt = Number((a && a.prompt_tokens) || 0);
  if (!s || !pt) return '<span style="color:var(--ink-3)" title="上游没报缓存字段（无样本）">—</span>';
  const hit = Number((a && a.cache_hit_tokens) || 0);
  const r = Math.max(0, Math.min(1, hit / pt)) * 100;
  return '<span title="命中 ' + fmtNum(hit) + ' / prompt ' + fmtNum(pt) + '（' + s + ' 个样本）">' +
    r.toFixed(1) + '%</span>';
}

/* 用量明细：账号 / 模型两个维度共用一张表 + 页内切换（TRAE 只有这两维，没有 WB 的「按域」）。 */
const US_DIMS = {
  account: { key: 'by_account', title: '账号', withPerf: true },
  model: { key: 'by_model', title: '模型', withPerf: false },
};
const US_DIM_TABS = [['account', '按账号'], ['model', '按模型']];

// usSortRows 按当前排序字段降序（默认合计 Token，最大者最相关）。
function usSortRows(rows, sort) {
  const out = rows.slice();
  const num = v => { const n = Number(v || 0); return Number.isFinite(n) ? n : 0; };
  const val = a => sort === 'requests' ? num(a.requests)
    : sort === 'errors' ? num(a.errors)
      : sort === 'latency' ? num(a.avg_latency_ms)
        : num(a.total_tokens);
  out.sort((a, b) => val(b) - val(a));
  return out;
}

function usDimHead(dim) {
  const m = US_DIMS[dim] || US_DIMS.account;
  return '<tr><th class="mark" aria-hidden="true"></th><th>' + esc(m.title) + '</th>' +
    '<th class="num">请求</th><th class="num">失败</th>' +
    '<th class="num">Prompt</th><th class="num">Completion</th><th class="num">合计</th>' +
    '<th class="num">缓存命中率</th>' +
    '<th class="num" title="按官方计费公式推算：输入/输出/缓存命中 token × 各自单价（积分/百万）。' +
    '上游 token_usage 帧里没有积分字段，所以这是估算，不是回报值；未收录单价的模型不计入。">' +
    '积分≈</th>' +
    (m.withPerf ? '<th class="num">均延迟</th><th class="num">均速率</th>' : '') +
    '</tr>';
}

/* usTabsHtml 维度切换按钮（带条数徽标）。整段 innerHTML 重写而不是逐个改 class：
   容器的 click 监听是委托式的，换掉子节点不会丢事件。 */
function usTabsHtml(dims, active, counts) {
  return dims.map(([k, label]) => {
    const n = counts ? counts[k] : null;
    return '<button data-dim="' + k + '"' + (k === active ? ' class="on"' : '') + '>' +
      esc(label) + (n == null ? '' : '<span class="cnt">' + n + '</span>') + '</button>';
  }).join('');
}

/* usRow 一行。withPerf 控制延迟/速率两列；列开关显式传入，避免调用方改动后与表头错列。 */
function usRow(name, sub, a, withPerf) {
  return '<tr><td class="mark" aria-hidden="true"></td>' +
    '<td>' + esc(name) + (sub ? '<div class="note">' + esc(sub) + '</div>' : '') + '</td>' +
    '<td class="num">' + fmtNum(a.requests) + '</td>' +
    '<td class="num">' + (a.errors ? '<span style="color:var(--warn)">' + fmtNum(a.errors) + '</span>' : '—') + '</td>' +
    '<td class="num">' + fmtNum(a.prompt_tokens) + '</td>' +
    '<td class="num">' + fmtNum(a.completion_tokens) + '</td>' +
    '<td class="num">' + fmtNum(a.total_tokens) +
    usMixBar(a.prompt_tokens, a.completion_tokens, a.total_tokens) + '</td>' +
    '<td class="num">' + fmtHit(a) + '</td>' +
    '<td class="num">' + fmtCredits(a.credits) + '</td>' +
    (withPerf
      ? '<td class="num">' + fmtLat(a.avg_latency_ms) + '</td>' +
        '<td class="num">' + fmtRate(a.avg_tokens_per_second) + '</td>'
      : '') +
    '</tr>';
}
function usEmpty(tb, cols, msg) {
  tb.innerHTML = '<tr><td colspan="' + cols + '"><div class="empty">' + esc(msg) + '</div></td></tr>';
}

/* ── 用量时序图（抄自 WorkBuddy 面板那张 SVG，砍掉缓存命中率线）────────────
   纯 SVG 手写，不引图表库：面板通篇零第三方依赖。
   三个函数都保持**纯函数**（只吃参数、只返字符串），源码里有一条 node 断言测试盯着它们。 */

/* 解析时间片标签："2026-09-25T15"（小时桶）/ "2026-09-25"（日桶）。
   手写而不用 Date.parse：缺分钟的时刻（"…T15"）在部分引擎里直接 NaN，
   而 NaN 会传染整张图。返回 null 让调用方丢掉这个点。 */
function usagePointTime(t) {
  const m = /^(\d{4})-(\d{2})-(\d{2})(?:T(\d{2}))?$/.exec(String(t || ''));
  if (!m) return null;
  // 标签用匹配到的**字符串**（保住前导零），数值只在算 ms 时转换。
  const mo = m[2], d = m[3], h = m[4] || null;
  return {
    ms: Date.UTC(+m[1], +m[2] - 1, +m[3], h ? +h : 0), // 只用来算横向相对位置，不做时区换算
    label: h ? mo + '-' + d + ' ' + h + ':00' : mo + '-' + d,
  };
}

/* 坐标轴刻度用的紧凑数字：1234 → 1.2k、1200000 → 1.2M。 */
function fmtShortTok(v) {
  if (v >= 1e6) return (v / 1e6).toFixed(1) + 'M';
  if (v >= 1e3) return (v / 1e3).toFixed(v >= 1e4 ? 0 : 1) + 'k';
  return String(Math.round(v));
}

/* 堆叠柱：输入在下、输出在上；命中率折线（有缓存数据的片才画）；三条网格线 + 首/中/尾刻度。 */
function usageChartSVG(series) {
  const pts = [];
  for (const p of series || []) {
    const t = usagePointTime(p && p.t);
    if (!t) continue;
    const pt = Number(p.prompt_tokens || 0), ct = Number(p.completion_tokens || 0);
    const hit = Number(p.cache_hit_tokens || 0), samples = Number(p.cache_samples || 0);
    pts.push({
      ms: t.ms, label: t.label, pt, ct, tt: Number(p.total_tokens || 0) || pt + ct,
      // 命中率只在「确实观测到缓存字段」的片上算；上游没报就是 null，线在那里断开——
      // 0% 命中与「没观测到」是两件事（同 WB 面板的口径）。
      rate: samples > 0 && pt > 0 ? Math.min(1, hit / pt) : null,
    });
  }
  if (!pts.length) return '<div class="us-empty">这个窗口内还没有调用。</div>';

  const W = 760, H = 180, PL = 46, PR = 12, PT = 12, PB = 28;
  const iw = W - PL - PR, ih = H - PT - PB;
  const t0 = pts[0].ms, span = Math.max(1, pts[pts.length - 1].ms - t0);
  const max = Math.max(1, ...pts.map(p => p.tt));
  const x = ms => PL + (ms - t0) / span * iw;
  const y = v => PT + ih - (v / max) * ih;

  // 柱宽取「最小真实间隔」的 70%，夹在 2~26px：窗口拉到 30 天会变细但看得见。
  let gap = span;
  for (let i = 1; i < pts.length; i++) gap = Math.min(gap, pts[i].ms - pts[i - 1].ms);
  const bw = Math.max(2, Math.min(26, (gap / span) * iw * 0.7));

  let grid = '';
  for (let g = 0; g <= 2; g++) {
    const yy = y(max * g / 2);
    grid += '<line class="ax" x1="' + PL + '" x2="' + (W - PR) + '" y1="' + yy.toFixed(1) +
      '" y2="' + yy.toFixed(1) + '"/><text class="lbl" x="' + (PL - 6) + '" y="' + (yy + 3).toFixed(1) +
      '" text-anchor="end">' + fmtShortTok(max * g / 2) + '</text>';
  }

  let bars = '';
  for (const p of pts) {
    // 夹进绘图区：首尾柱子的半个柱宽会探出 PL / W-PR，压在 y 轴刻度或右边界上。
    const bx = Math.min(W - PR - bw, Math.max(PL, x(p.ms) - bw / 2)).toFixed(1);
    const hp = (p.pt / max) * ih, hc = (p.ct / max) * ih;
    if (hp > 0) {
      bars += '<rect class="bar-p" x="' + bx + '" y="' + y(p.pt).toFixed(1) + '" width="' + bw.toFixed(1) +
        '" height="' + hp.toFixed(1) + '" rx="1.5"/>';
    }
    if (hc > 0) {
      // 中间留 1px 缝：同色堆叠时分不出哪段是输入哪段是输出
      bars += '<rect class="bar-c" x="' + bx + '" y="' + (y(p.tt) + 1).toFixed(1) + '" width="' + bw.toFixed(1) +
        '" height="' + Math.max(0, hc - 1).toFixed(1) + '" rx="1.5"/>';
    }
  }

  let axis = '<line class="ax" x1="' + PL + '" x2="' + (W - PR) + '" y1="' + (PT + ih) + '" y2="' + (PT + ih) + '"/>';
  for (const m of [pts[0], pts[Math.floor(pts.length / 2)], pts[pts.length - 1]]) {
    axis += '<text class="lbl" x="' + x(m.ms).toFixed(1) + '" y="' + (PT + ih + 16) +
      '" text-anchor="middle">' + m.label + '</text>';
  }

  // 命中率折线：0% 贴基线、100% 贴顶线；空档处断开成多段。单点不成线，但仍给个圆点。
  let hit = '', seg = [];
  const flush = () => {
    if (seg.length > 1) hit += '<polyline class="hitline" points="' + seg.join(' ') + '"/>';
    seg = [];
  };
  for (const p of pts) {
    if (p.rate === null) {
      flush();
      continue;
    }
    const cx = x(p.ms).toFixed(1), cy = (PT + ih - p.rate * ih).toFixed(1);
    seg.push(cx + ',' + cy);
    hit += '<circle class="hitdot" cx="' + cx + '" cy="' + cy + '" r="2"><title>缓存命中率 ' +
      Math.round(p.rate * 100) + '%</title></circle>';
  }
  flush();

  return '<svg viewBox="0 0 ' + W + ' ' + H + '" role="img" aria-label="token 时序">' +
    grid + bars + hit + axis + '</svg>';
}

function renderUsageChart(series) {
  const host = $('usChart');
  if (!host) return;
  host.innerHTML = usageChartSVG(series);
  $('usChartNote').textContent = (series || []).length
    ? '堆叠柱：输入 + 输出，共 ' + series.length + ' 个时间片' : '';
}

/* renderUsageDim 用量明细（账号/模型维度切换 + 排序），数据一次拉全，切换零请求。 */
let usDim = 'account', usSort = 'total', usageData = null;
function renderUsageDim() {
  const d = usageData || {};
  const m = US_DIMS[usDim] || US_DIMS.account;
  const rows = usSortRows(d[m.key] || [], usSort);
  $('usDimTabs').innerHTML = usTabsHtml(US_DIM_TABS, usDim, {
    account: (d.by_account || []).length,
    model: (d.by_model || []).length,
  });
  $('usDimHead').innerHTML = usDimHead(usDim);
  $('usDimBody').innerHTML = rows.map(x =>
    usRow(usDim === 'account' ? (x.extra || String(x.key || '').slice(0, 8)) : x.key,
      usDim === 'account' ? String(x.key || '').slice(0, 8) : '',
      x, m.withPerf)
  ).join('') || '<tr><td colspan="' + (m.withPerf ? 11 : 9) + '"><div class="empty">这个窗口内还没有调用</div></td></tr>';
  $('usDimNote').textContent = rows.length + ' 行 · 请求数含失败尝试';
}

function renderUsage(d) {
  usageData = d || {};
  const t = usageData.totals || {};
  const total = Number(t.total_tokens || 0);
  const pt = Number(t.prompt_tokens || 0);
  const ct = Number(t.completion_tokens || 0);
  const reqs = Number(t.requests || 0);
  const errs = Number(t.errors || 0);
  const okRate = reqs ? (reqs - errs) / reqs * 100 : null;
  // 构成条要的是合法 CSS 宽度，usPct 在无样本时返回 "—"，不能直接拼进 style。
  const pctW = (part) => total ? Math.max(0, Math.min(100, Number(part || 0) / total * 100)).toFixed(2) + '%' : '0%';
  // 六张卡：主指标用强调色，completion 用成功色（与图表里的绿柱呼应），
  // 失败/延迟只在有值时上语义色——全绿全黄的仪表盘等于没有重点。
  $('usStats').innerHTML =
    usKpi(fmtNum(reqs), '请求数', 'c-accent', errs ? '其中失败 ' + fmtNum(errs) + ' 次' : '全部成功') +
    usKpi(fmtNum(total), '总 token', 'c-accent',
      'prompt ' + usPct(pt, total) + ' · completion ' + usPct(ct, total),
      '<div class="kbar"><i style="width:' + pctW(pt) + ';background:var(--accent)"></i>' +
      '<i style="width:' + pctW(ct) + ';background:var(--ok)"></i></div>') +
    usKpi(fmtNum(pt), 'prompt', 'c-soft', '占比 ' + usPct(pt, total)) +
    usKpi(fmtNum(ct), 'completion', 'c-ok', '占比 ' + usPct(ct, total)) +
    usKpi(String(errs), '失败尝试', errs ? 'c-warn' : 'c-mute',
      okRate == null ? '—' : (errs ? '成功率 ' + okRate.toFixed(1) + '%' : '成功率 100%')) +
    usKpi(fmtLat(t.avg_latency_ms), '平均延迟', 'c-soft',
      t.avg_tokens_per_second ? '吐字 ' + fmtRate(t.avg_tokens_per_second) : '无速率样本') +
    usKpi(fmtCredits(t.credits).replace(/<[^>]+>/g, ''), '估算积分', 'c-warn',
      '按官方单价推算，不是上游回报值');

  // 卡片、明细表与时序图全部按所选窗口统计（切窗口数字随之变化）。
  const win = usHours === '0' ? '全部历史' : '近 ' + usHours + ' 小时'
    + (usHours === '168' ? '（7 天）' : usHours === '720' ? '（30 天）' : '');
  const note = win + ' · ' + fmtNum(usageData.buckets) + ' 个分桶' +
    (usageData.since ? ' · 数据自 ' + String(usageData.since).replace('T', ' ') : '') +
    (usageData.file_bytes ? ' · 文件 ' + (usageData.file_bytes / 1024).toFixed(1) + ' KB' : '');
  $('usNote').textContent = note;
  $('usNote').title = note; // 窄屏单行截断时靠悬停看全

  renderUsageDim();

  // 按小时：同一份 series（窗口与卡片一致），列口径与 WB 的「按小时」一致（TRAE 无逐请求积分）。
  const series = usageData.series || [];
  if (series.length) {
    $('usSeriesBody').innerHTML = series.map(p => usRow(
      p.t, p.scope === 'day' ? '日' : '小时', p)).join('');
  } else usEmpty($('usSeriesBody'), 9, '这个窗口内还没有调用');
  $('usSeriesNote').textContent = usHours === '0' ? '全部历史（含折叠日桶）' : '按小时分片';
  renderUsageChart(series);
}

async function loadUsage(quiet) {
  try {
    const d = await api('usage?hours=' + encodeURIComponent(usHours));
    renderUsage(d);
  } catch (e) {
    if (!quiet) toast('读取用量失败：' + e.message, 'err');
  }
}

/* ── 配置 ─────────────────────────────────────────────────────────── */
const CFG_MAP = {
  // 服务
  listen: ['listen'], auth_dir: ['auth_dir'], state_file: ['state_file'],
  // 排程（整段改动需重启）
  checkin_hours: ['schedule', 'checkin_hours'], keepalive_hours: ['schedule', 'keepalive_hours'],
  checkin_enabled: ['schedule', 'checkin_enabled'], keepalive_enabled: ['schedule', 'keepalive_enabled'],
  balance_refresh_enabled: ['schedule', 'balance_refresh_enabled'],
  balance_refresh_minutes: ['schedule', 'balance_refresh_minutes'],
  // 冷却与熔断
  plan_credit: ['cooldown', 'plan_credit'], soft_rate: ['cooldown', 'soft_rate'],
  soft_rate_max: ['cooldown', 'soft_rate_max'],
  breaker_threshold: ['pool', 'breaker_threshold'], breaker_cooldown: ['pool', 'breaker_cooldown'],
  breaker_cooldown_max: ['pool', 'breaker_cooldown_max'],
  degrade_threshold: ['pool', 'degrade_threshold'], degrade_cooldown: ['pool', 'degrade_cooldown'],
  degrade_cooldown_max: ['pool', 'degrade_cooldown_max'],
  // 选号
  max_in_flight: ['pool', 'max_in_flight'], idle_weight_per_hour: ['pool', 'idle_weight_per_hour'],
  idle_weight_max: ['pool', 'idle_weight_max'], expiring_soon: ['pool', 'expiring_soon'],
  // 日志
  request_client_info: ['logging', 'request_client_info'],
  // 会话粘性 / 提示词 / 上游
  session_sticky_enabled: ['session_sticky', 'enabled'], session_sticky_ttl: ['session_sticky', 'ttl'],
  session_sticky_gc_interval: ['session_sticky', 'gc_interval'],
  prompt_mode: ['prompt', 'mode'], prompt_file: ['prompt', 'file'],
  default_model: ['default_model'], timeout_seconds: ['upstream', 'timeout_seconds'],
  api_key: ['api_key'],
  header_timeout_seconds: ['upstream', 'header_timeout_seconds'],
  idle_timeout_seconds: ['upstream', 'idle_timeout_seconds'],
  user_agent: ['upstream', 'user_agent'], client_version: ['upstream', 'client_version'],
};
// ARRAY_FIELDS 逗号分隔提交成 JSON 数组（后端是 []int）；BOOL_FIELDS 用 select 的
// "true"/"false" 提交成 JSON 布尔——直接把字符串塞进 json 会让后端 Unmarshal 失败。
const ARRAY_FIELDS = ['checkin_hours', 'keepalive_hours'];
const BOOL_FIELDS = ['checkin_enabled', 'keepalive_enabled', 'balance_refresh_enabled', 'session_sticky_enabled'];
function dig(obj, path) { return path.reduce((o, k) => (o == null ? undefined : o[k]), obj); }
function put(obj, path, val) {
  let o = obj;
  for (let i = 0; i < path.length - 1; i++) {
    if (typeof o[path[i]] !== 'object' || o[path[i]] === null) o[path[i]] = {};
    o = o[path[i]];
  }
  o[path[path.length - 1]] = val;
}
/* Go 时长字段即时校验：非空必须是 ParseDuration 语法（30m / 2h / 600s / 1h30m）。
   与后端 config.go normalize() 的 time.ParseDuration 同口径，脏值就地标红，
   不再等到保存被拒。 */
const DURATION_RE = /^(\d+(\.\d+)?(ns|us|µs|ms|s|m|h))+$/;
const DURATION_FIELDS = ['plan_credit', 'soft_rate', 'soft_rate_max', 'breaker_cooldown',
  'breaker_cooldown_max', 'degrade_cooldown', 'degrade_cooldown_max',
  'session_sticky_ttl', 'session_sticky_gc_interval', 'expiring_soon'];
const DURATION_TIP = '格式应为 Go 时长：30m / 2h / 600s / 1h30m';
function markDurationFields() {
  for (const name of DURATION_FIELDS) {
    const el = $('cfgForm').elements[name];
    if (!el) continue;
    const v = el.value.trim();
    const bad = v !== '' && !DURATION_RE.test(v);
    el.classList.toggle('invalid', bad);
    el.title = bad ? DURATION_TIP : '';
  }
}

async function loadConfig() {
  try {
    const d = await api('config');
    cfgLoaded = d.config;
    $('cfgPath').textContent = d.path || '';
    const f = $('cfgForm');
    for (const [name, path] of Object.entries(CFG_MAP)) {
      const el = f.elements[name];
      if (!el) continue;
      const v = dig(cfgLoaded, path);
      if (el.type === 'checkbox') el.checked = !!v;
      else if (Array.isArray(v)) el.value = v.join(', ');
      else if (BOOL_FIELDS.includes(name)) el.value = v ? 'true' : 'false';
      else el.value = v == null ? '' : v;
    }
    markDurationFields();
    $('cfgNote').textContent = '';
  } catch (e) { toast('读取配置失败：' + e.message, 'err'); }
}
function collectConfig() {
  const f = $('cfgForm');
  for (const [name, path] of Object.entries(CFG_MAP)) {
    const el = f.elements[name];
    if (!el) continue;
    // 复选框必须按 checked 提交（照 WB 面板）：el.value 恒为 "on"，用值判断会把「勾上」
    // 也写成 false；而且不勾时 value 非空，会绕过下面「空 = 沿用现值」的判断，
    // 于是开关永远存成 false —— 这正是 TRAE 原先没有 checkbox 分支时的坑。
    if (el.type === 'checkbox') { put(cfgLoaded, path, !!el.checked); continue; }
    const raw = el.value.trim();
    // api_key 例外：空也要提交（清空 = 关鉴权）；其他字段留空表示"沿用现值"。
    if (name === 'api_key') { put(cfgLoaded, path, raw); continue; }
    if (raw === '') continue;                       // 空 = 沿用现值
    if (ARRAY_FIELDS.includes(name)) {
      put(cfgLoaded, path, raw.split(/[,，\s]+/).filter(Boolean).map(Number));
    } else if (BOOL_FIELDS.includes(name)) {
      put(cfgLoaded, path, raw === 'true');
    } else {
      put(cfgLoaded, path, el.type === 'number' ? Number(raw) : raw);
    }
  }
  return cfgLoaded;
}

/* ── 积分构成 ─────────────────────────────────────────────────────── */
/* 一个账号的余额是若干积分包之和。包按来源命名（老用户福利 / 每月登录赠送 / 签到奖励…），
   面额从 100 到 4000 不等、按次发放，且各自带到期时间。所以只看聚合值看不出「余额为什么
   差这么多」——差别只在包里。这里把逐包明细摊开，并给每个来源一个稳定配色，跨账号对比
   时同色即同类。取数走 /panel/api/packages（逐账号实时查上游）。 */

// 数字格式化：积分/token 通用紧凑写法（1.2k / 3.4M）。
function fmtTok(n) {
  n = Number(n || 0);
  if (n >= 1e9) return (n / 1e9).toFixed(2) + 'B';
  if (n >= 1e6) return (n / 1e6).toFixed(2) + 'M';
  if (n >= 1e3) return (n / 1e3).toFixed(1) + 'k';
  return String(n);
}

const PK_COLORS = ['#4f8cff', '#25b08b', '#e8a33d', '#c96bd6', '#e2607a',
  '#5aa9e6', '#8fbf3f', '#b58b5a', '#7d8fa8', '#d4785c'];
const PK_ACCOUNT_COLORS = ['#4f8cff', '#25b08b', '#e8a33d', '#c96bd6',
  '#e2607a', '#20a4a4', '#8fbf3f', '#d4785c',
  '#7c83db', '#c48a2f', '#b45f8c', '#5aa9e6'];
const PK_DAY_MS = 24 * 3600 * 1000;

function pkColor(i) { return PK_COLORS[i % PK_COLORS.length]; }

// pkAccountColorMap 按 UID 稳定分配颜色：排序后分配，账号刷新/重排不会换色。
function pkAccountColorMap(list) {
  const uids = (list || []).filter(a => a && !a.error && a.uid).map(a => String(a.uid)).sort();
  const colors = new Map();
  uids.forEach((uid, i) => colors.set(uid, PK_ACCOUNT_COLORS[i % PK_ACCOUNT_COLORS.length]));
  return colors;
}

/* pkBySource 把包按来源归并，得到「来源 → 面额/余额/个数」。这是对比的关键视图：
   两个号的差异一定体现在某几个来源的面额上。分组键带 group，避免同名不同类被并成一类。 */
function pkBySource(packs) {
  const m = new Map();
  for (const p of packs || []) {
    const k = (p.group || '') + '|' + (p.name || '(未命名)');
    const e = m.get(k) || {
      key: k, name: p.name || p.group || '(未命名)', n: 0,
      remain: 0, size: 0, used: 0, minExpire: 0, minStart: 0,
    };
    e.n += 1;
    e.remain += Number(p.remain || 0);
    e.size += Number(p.size || 0);
    e.used += Number(p.used || 0);
    const ex = Number(p.expire || 0);
    if (ex > 0 && (!e.minExpire || ex < e.minExpire)) e.minExpire = ex;
    const st = Number(p.start || 0);
    if (st > 0 && (!e.minStart || st < e.minStart)) e.minStart = st;
    m.set(k, e);
  }
  return [...m.values()].sort((a, b) => b.size - a.size);
}

// pkExpiryMs 包到期毫秒时间戳：到期分布与明细排序共用这一份。0 = 上游没给到期，返回 null（不猜）。
function pkExpiryMs(p) {
  const s = Number(p && p.expire);
  return Number.isFinite(s) && s > 0 ? s * 1000 : null;
}

// pkCreditOpacity 越临近到期越不透明（urgency 可视化）。
function pkCreditOpacity(days) {
  if (days == null || !Number.isFinite(Number(days))) return 1;
  return 0.25 + 0.75 * Math.max(0, Math.min(29, Number(days) - 1)) / 29;
}

function pkExpiryText(expiresAt) {
  if (!expiresAt) return '无到期时间';
  const diff = expiresAt - Date.now();
  if (diff <= 0) return '已到期';
  const minutes = Math.max(1, Math.ceil(diff / 60000));
  if (minutes < 60) return '剩余 ' + minutes + ' 分钟';
  const hours = Math.ceil(diff / 3600000);
  if (hours < 24) return '剩余 ' + hours + ' 小时';
  return '剩余 ' + Math.ceil(diff / PK_DAY_MS) + ' 天';
}

function pkExpiryDateTime(expiresAt) {
  if (!expiresAt) return '—';
  return new Date(expiresAt).toLocaleString('zh-CN', {
    timeZone: 'Asia/Shanghai', hour12: false,
    year: 'numeric', month: '2-digit', day: '2-digit',
    hour: '2-digit', minute: '2-digit',
  });
}

// pkAccountSegments 账号内先按总余额约束逐包金额（上游可能重复记录），再按到期时间排序。
function pkAccountSegments(a, now) {
  let balance = Math.max(0, Number(a.remain || 0));
  const out = [];
  for (const p of a.packages || []) {
    const remain = Number(p.remain || 0);
    if (!Number.isFinite(remain) || remain <= 0 || balance <= 0) continue;
    const amount = Math.min(balance, remain);
    const expiresAt = pkExpiryMs(p);
    out.push({
      amount,
      expiresAt,
      days: expiresAt == null ? null : Math.max(0, Math.ceil((expiresAt - now) / PK_DAY_MS)),
      source: p.name || '积分',
      uid: String(a.uid || ''),
      accountName: a.nickname || String(a.uid || '').slice(0, 8) || '未命名账号',
    });
    balance -= amount;
  }
  return out.sort((x, y) => {
    if (x.expiresAt == null && y.expiresAt != null) return 1;
    if (x.expiresAt != null && y.expiresAt == null) return -1;
    return (x.expiresAt || 0) - (y.expiresAt || 0);
  });
}

// summarizeCreditDays 按精确剩余天数逐行聚合；无有效到期时间的余额不进图，也不猜到期日。
function summarizeCreditDays(list, now) {
  const buckets = new Map();
  let unavailable = 0;
  for (const a of list || []) {
    if (a.error || !Number.isFinite(Number(a.remain))) { unavailable++; continue; }
    for (const segment of pkAccountSegments(a, now)) {
      if (segment.days == null) continue;
      let row = buckets.get(segment.days);
      if (!row) { row = { days: segment.days, credits: 0, segments: [] }; buckets.set(segment.days, row); }
      row.credits += segment.amount;
      row.segments.push(segment);
    }
  }
  const rows = [...buckets.values()].sort((a, b) => a.days - b.days);
  for (const row of rows) {
    row.segments.sort((a, b) =>
      (a.expiresAt || Infinity) - (b.expiresAt || Infinity) ||
      a.accountName.localeCompare(b.accountName) ||
      a.source.localeCompare(b.source));
  }
  return { rows, accountCount: (list || []).length, unavailable };
}

function renderExpiryDistribution(list, now) {
  const summary = summarizeCreditDays(list, now);
  const colors = pkAccountColorMap(list);
  const rows = summary.rows.map(row => {
    const total = row.credits || 1;
    const nodes = row.segments.map(segment => {
      const color = colors.get(segment.uid) || 'var(--accent)';
      const title = segment.source + '\n' + fmtTok(segment.amount) + ' 积分\n到期时间 ' +
        pkExpiryDateTime(segment.expiresAt) + '（' + pkExpiryText(segment.expiresAt) + '）\n' +
        segment.accountName;
      return '<span class="pk-expiry-seg" style="--seg-color:' + color +
        ';opacity:' + pkCreditOpacity(segment.days).toFixed(5) +
        ';flex:' + Math.max(0.008, segment.amount / total).toFixed(4) +
        ' 1 0" title="' + esc(title) + '" aria-label="' + esc(title) + '"></span>';
    }).join('');
    return '<div class="pk-expiry-row"><span>' + esc(row.days === 0 ? '已到期' : row.days + ' 天') +
      '</span><div class="pk-expiry-track">' + nodes + '</div><b>' + esc(fmtTok(row.credits)) +
      '</b></div>';
  }).join('');
  const foot = summary.accountCount + ' 个账号' +
    (summary.unavailable ? ' · ' + summary.unavailable + ' 个未获取余额' : '');
  const legend = (list || []).filter(a =>
    a && !a.error && a.uid && pkAccountSegments(a, now).some(s => s.days != null)
  ).map(a => '<span><i style="background:' + (colors.get(String(a.uid)) || 'var(--accent)') +
    '"></i>' + esc(a.nickname || String(a.uid).slice(0, 8)) + '</span>').join('');
  const hdr = '<div class="pk-expiry-hdr"><span>剩余天数</span><span style="text-align:center">各账号该批剩余</span><b>剩余积分</b></div>';
  $('pkExpiry').innerHTML = (rows
    ? hdr + '<div class="pk-expiry-chart">' + rows + '</div>'
    : '<div class="pk-expiry-empty">暂无可汇总积分</div>') +
    (legend ? '<div class="pk-expiry-legend">' + legend + '</div>' : '') +
    '<div class="pk-expiry-foot">' + esc(foot) + '</div>';
}

// pkDetailCompare 单账号逐包明细排序：到期时间升序，同一到期按面额降序。
function pkDetailCompare(a, b) {
  const sizeOf = p => { const n = Number(p && p.size); return Number.isFinite(n) ? n : 0; };
  const ea = pkExpiryMs(a), eb = pkExpiryMs(b);
  if (ea == null && eb != null) return 1;
  if (ea != null && eb == null) return -1;
  if (ea != null && eb != null && ea !== eb) return ea - eb;
  return sizeOf(b) - sizeOf(a);
}

// pkDetailGroups 正余额包先按到期时间挑默认展示项，其余正余额包与已用完包分别折叠。
const PK_DEFAULT_DETAIL_LIMIT = 5;
function pkDetailGroups(packs, limit) {
  const active = [], used = [];
  let usedSize = 0, restSize = 0, restRemain = 0;
  for (const p of packs || []) {
    if (Number(p && p.remain) > 0) { active.push(p); continue; }
    used.push(p);
    const size = Number(p && p.size);
    if (Number.isFinite(size)) usedSize += size;
  }
  active.sort(pkDetailCompare);
  used.sort(pkDetailCompare);
  const n = Number.isFinite(Number(limit)) && Number(limit) > 0 ? Math.floor(Number(limit)) : PK_DEFAULT_DETAIL_LIMIT;
  const visible = active.slice(0, n);
  const rest = active.slice(visible.length);
  for (const p of rest) {
    restSize += Number(p.size || 0);
    restRemain += Number(p.remain || 0);
  }
  return { visible, rest, used, restSize, restRemain, usedSize };
}

function renderPackages(d) {
  const list = (d.accounts || []);
  const now = Date.now();
  renderExpiryDistribution(list, now);
  if (!list.length) {
    $('pkSummary').innerHTML = '<div class="empty">没有账号</div>';
    $('pkDetail').innerHTML = '';
    $('pkNote').textContent = '—';
    return;
  }

  // 包名 → 稳定色号（跨账号一致，方便肉眼对齐）
  const names = [];
  for (const a of list) for (const s of pkBySource(a.packages || [])) {
    if (!names.includes(s.key)) names.push(s.key);
  }
  const sizeOf = n => Math.max(0, ...list.map(a => {
    const f = pkBySource(a.packages || []).find(s => s.key === n);
    return f ? f.size : 0;
  }));
  names.sort((x, y) => sizeOf(y) - sizeOf(x));
  const colorOf = n => pkColor(names.indexOf(n));

  const maxRemain = Math.max(1, ...list.map(a => Number(a.remain || 0)));

  $('pkSummary').innerHTML = list.map(a => {
    if (a.error) {
      return '<div class="pk-card"><div class="who"><span class="nm">' +
        esc(a.nickname || String(a.uid || '').slice(0, 8)) + '</span>' +
        '<span class="realm">' + esc(a.realm || '') + '</span></div>' +
        '<div class="err">查询失败：' + esc(a.error) + '</div></div>';
    }
    const srcs = pkBySource(a.packages || []);
    const total = Math.max(1, Number(a.size || 0));
    const bar = srcs.map(s =>
      '<i style="width:' + (s.size / total * 100).toFixed(2) + '%;background:' +
      colorOf(s.key) + '" title="' + esc(s.name) + ' ' + fmtTok(s.size) + '"></i>').join('');
    const legend = srcs.map(s =>
      '<span><i style="background:' + colorOf(s.key) + '"></i>' +
      esc(s.name) + ' x' + s.n + ' · ' + fmtTok(s.size) + '</span>').join('');
    return '<div class="pk-card">' +
      '<div class="who"><span class="nm">' + esc(a.nickname || String(a.uid || '').slice(0, 8)) + '</span>' +
      '<span class="realm">' + esc(a.realm || '') + '</span></div>' +
      '<div class="big">' + fmtTok(a.remain) + '</div>' +
      '<div class="sub">共 ' + fmtTok(a.size) + ' · ' + (a.packages || []).length +
      ' 个包 · 占最高 ' + (Number(a.remain || 0) / maxRemain * 100).toFixed(0) + '%</div>' +
      '<div class="mixbar">' + bar + '</div>' +
      '<div class="pk-legend">' + legend + '</div>' +
      '</div>';
  }).join('');

  $('pkNote').textContent = list.length + ' 个账号 · 实时查询上游';

  // 逐包明细：每个账号一个表，包的**面额**列是重点
  $('pkDetail').innerHTML = list.map(a => {
    if (a.error) return '';
    const groups = pkDetailGroups(a.packages || [], PK_DEFAULT_DETAIL_LIMIT);
    const rowOf = (p, rowGroup) => {
      const k = (p.group || '') + '|' + (p.name || '(未命名)');
      const sub = p.group && p.group !== p.name ? p.group : '';
      return '<tr' + (rowGroup ? ' class="pk-hidden-row" data-pk-row="' + rowGroup + '" hidden' : '') +
        '><td class="mark" aria-hidden="true"><i style="background:' + colorOf(k) + '"></i></td>' +
        '<td>' + esc(p.name || p.group || '(未命名)') +
        (sub ? '<div class="note">' + esc(sub) + '</div>' : '') + '</td>' +
        '<td class="num">' + fmtTok(p.size) + '</td>' +
        '<td class="num">' + fmtTok(p.remain) + '</td>' +
        '<td class="num">' + fmtTok(p.used) + '</td>' +
        '<td class="num">' + (p.start ? esc(pkExpiryDateTime(Number(p.start) * 1000).slice(0, 10)) : '—') + '</td>' +
        '<td class="num">' + (p.expire ? esc(pkExpiryDateTime(Number(p.expire) * 1000).slice(0, 10)) : '—') + '</td>' +
        '</tr>';
    };
    const groupSummary = (group, label, count, size, remain) =>
      '<tr class="pk-group-summary"><td colspan="7"><button type="button" class="pk-group-toggle"' +
      ' data-pk-group="' + group + '" data-count="' + count + '" data-size="' + size +
      '" data-remain="' + remain + '" aria-expanded="false">' + label + '，展开</button></td></tr>';
    const rows = groups.visible.map(p => rowOf(p, '')).join('');
    const restSummary = groups.rest.length
      ? groupSummary('rest', '其余未用完 ' + groups.rest.length + ' 个包（面额合计 ' +
        fmtTok(groups.restSize) + ' · 剩余 ' + fmtTok(groups.restRemain) + '）',
        groups.rest.length, groups.restSize, groups.restRemain) +
      groups.rest.map(p => rowOf(p, 'rest')).join('')
      : '';
    const usedSummary = groups.used.length
      ? groupSummary('used', '已用完 ' + groups.used.length + ' 个包（面额合计 ' +
        fmtTok(groups.usedSize) + '）', groups.used.length, groups.usedSize, 0) +
      groups.used.map(p => rowOf(p, 'used')).join('')
      : '';
    return '<div class="box"><header><h3>' +
      esc(a.nickname || String(a.uid || '').slice(0, 8)) + ' · ' + esc(a.realm || '') +
      '</h3><span class="grow"></span><span class="note">余额 ' + fmtTok(a.remain) +
      ' / 总额 ' + fmtTok(a.size) + ' · 可用 ' + (groups.visible.length + groups.rest.length) + ' 个包' +
      (groups.used.length ? ' / 已用完 ' + groups.used.length + ' 个' : '') +
      ' · 默认展示最早到期 ' + PK_DEFAULT_DETAIL_LIMIT + ' 条</span>' +
      '</header><div class="tbl-wrap"><table class="acc"><thead><tr>' +
      '<th class="mark" aria-hidden="true"></th><th>包名 / 来源</th>' +
      '<th class="num">面额</th><th class="num">剩余</th><th class="num">已用</th>' +
      '<th class="num">发放</th><th class="num">到期</th>' +
      '</tr></thead><tbody>' + rows + restSummary + usedSummary + '</tbody></table></div></div>';
  }).join('');
}

async function loadPackages() {
  $('pkSummary').innerHTML = '<div class="empty">查询中…（逐账号向上游实时查询）</div>';
  $('pkDetail').innerHTML = '';
  $('pkExpiry').innerHTML = '<div class="pk-expiry-empty">查询中…</div>';
  try {
    const d = await api('packages');
    renderPackages(d);
  } catch (e) {
    $('pkSummary').innerHTML = '<div class="empty">读取失败：' + esc(e.message) + '</div>';
    $('pkExpiry').innerHTML = '<div class="pk-expiry-empty">读取失败：' + esc(e.message) + '</div>';
  }
}

/* ── 添加账号（不轮询：贴回调地址换票）────────────────────────────── */
// 地区：链接按地区切域（国内 www.trae.cn / 国际 www.trae.ai）。回调自带 OAuth host，
// 所以换票那步不用再选一次——面板按 host 自动落 domain/apiHost。
let addRealm = 'cn';
function openAdd() {
  loginID = '';
  $('addLoad').hidden = false;
  $('addReady').hidden = true;
  $('addErr').hidden = true;
  $('btnCopyUrl').hidden = true;
  $('btnOpenUrl').hidden = true;
  $('btnAddFinish').hidden = true;
  $('callback').value = '';
  $('addVeil').classList.add('on');
  api('login/start', { method: 'POST', body: JSON.stringify({ realm: addRealm }) }).then(d => {
    loginID = d.id;
    $('addUrl').textContent = d.url;
    $('addLoad').hidden = true;
    $('addReady').hidden = false;
    $('btnCopyUrl').hidden = false;
    $('btnOpenUrl').hidden = false;
    $('btnAddFinish').hidden = false;
  }).catch(e => {
    $('addLoad').hidden = true;
    $('addErr').hidden = false;
    $('addErr').textContent = e.message;
  });
}
async function finishAdd() {
  const btn = $('btnAddFinish');
  btn.disabled = true;
  try {
    const d = await api('login/finish', {
      method: 'POST',
      body: JSON.stringify({ id: loginID, callback: $('callback').value }),
    });
    // 加完立刻关弹窗：成功提示交给 toast（弹窗关了也看得见），省一次「关闭」点击。
    toast('已加入：' + (d.nickname || d.uid), 'ok');
    $('addVeil').classList.remove('on');
    loadOverview(true);
  } catch (e) {
    $('addErr').hidden = false;
    $('addErr').textContent = e.message;
  } finally { btn.disabled = false; }
}

/* ── 启动 ─────────────────────────────────────────────────────────── */
function start() {
  ready = true;
  go(view);                              // 认证已通过，加载当前视图
  if (refTimer) clearInterval(refTimer);
  refTimer = setInterval(() => {
    if (document.hidden) return;
    if (view === 'accounts') loadOverview(true);
    if (view === 'usage') loadUsage(true);
    if (view === 'logs') loadLogs();
  }, 5000);
}

function boot() {
  applyTheme();

  $('btnTheme').onclick = () => {
    theme = effTheme() === 'light' ? 'dark' : 'light';
    localStorage.setItem(LS_THEME, theme);
    applyTheme();
  };
  $('btnRefresh').onclick = () => {
    if (view === 'accounts') loadOverview();
    if (view === 'models') loadModels();
    if (view === 'usage') loadUsage();
    if (view === 'packages') loadPackages();
    if (view === 'config') loadConfig();
    if (view === 'logs') loadLogs();
  };
  $('btnAdd').onclick = openAdd;
  // 登出：API-key 门没有服务端半边（withAuth 只是 Bearer 比对，无会话/无 cookie），
  // 所以「登出」就是本浏览器不再保留密钥——清掉 + 重载（重载同时清掉 5s 轮询与已渲染的行）。
  $('btnLogout').onclick = () => {
    if (!confirm('登出将清除本浏览器保存的密钥；持有密钥的人仍可调用接口。确认登出？')) return;
    clearKey();
    location.reload();
  };
  $('addRealm').addEventListener('click', ev => {
    const b = ev.target.closest('button[data-realm]');
    if (!b || b.classList.contains('on')) return;
    addRealm = b.dataset.realm;
    document.querySelectorAll('#addRealm .chip').forEach(c => c.classList.toggle('on', c === b));
    openAdd(); // 换地区就换一条链接，免得拿国内链接登国际号
  });
  $('btnAddClose').onclick = () => $('addVeil').classList.remove('on');
  $('btnCopyUrl').onclick = () => {
    if (navigator.clipboard) navigator.clipboard.writeText($('addUrl').textContent).catch(() => {});
  };
  // 新标签页打开：点击是用户手势，弹窗拦截不挡；noopener 不给新页面 window.opener 句柄。
  $('btnOpenUrl').onclick = () => window.open($('addUrl').textContent, '_blank', 'noopener');
  $('btnAddFinish').onclick = finishAdd;
  $('callback').addEventListener('keydown', e => { if (e.key === 'Enter') finishAdd(); });
  $('btnKey').onclick = submitKey;
  $('keyInput').addEventListener('keydown', e => { if (e.key === 'Enter') submitKey(); });

  document.querySelectorAll('.nav a').forEach(a => a.onclick = e => {
    e.preventDefault();
    go(a.dataset.view);
    history.replaceState(null, '', '#' + a.dataset.view);
  });

/* 签到结果如实回报：本次 +N 分（分账号列出来），已签/冷却/失败分开说。
   以前这里拿「积分包合计」冒充签到收益，签失败也像成功。 */
function checkinMsg(rs, accounts) {
  const ok = rs.filter(r => r.status === 'ok' && r.credits > 0);
  const already = rs.filter(r => r.status === 'already').length;
  if (!rs.length) {
    // 冷却中的账号会被自动签到跳过（避免对拥塞的上游连打），这不是成功也不是失败。
    const cool = (accounts || []).filter(a => a.checkin_cooling && !a.disabled).length;
    return cool ? '本次没有账号可签：' + cool + ' 个在签到冷却中，下一轮补签会再试'
      : '本次没有账号需要签到';
  }
  const total = ok.reduce((s, r) => s + (r.credits || 0), 0);
  let msg;
  if (ok.length) {
    msg = '签到完成：' + ok.map(r => (r.nickname || r.uid) + ' +' + r.credits).join('、') + '（合计 +' + total + ' 分）';
  } else if (already) {
    msg = '今天 ' + already + ' 个账号都已签过，本次没有新增积分';
  } else {
    msg = '签到完成，本次没有新增积分';
  }
  if (ok.length && already) msg += '，另有 ' + already + ' 个今天已签';
  return msg;
}

  $('btnCheckinAll').onclick = async () => {
    try {
      const d = await api('checkin', { method: 'POST', body: '{}' });
      toast(checkinMsg(d.checkin || [], d.accounts || []), 'ok');
    } catch (e) { toast(e.message, 'err'); }
    loadOverview(true);
  };
  $('btnBalanceAll').onclick = async () => {
    try {
      const d = await api('balance', { method: 'POST', body: '{}' });
      if (d.failed) toast('刷新完成，' + d.failed + ' 个账号失败', 'err');
      else toast('积分已刷新', 'ok');
    } catch (e) { toast(e.message, 'err'); }
    loadOverview(true);
  };
  $('btnKeepalive').onclick = async () => {
    try {
      await api('keepalive', { method: 'POST', body: '{}' });
      toast('token 已刷新（保活）', 'ok');
    } catch (e) { toast(e.message, 'err'); }
    loadOverview(true);
  };
  $('accBody').addEventListener('click', async ev => {
    const b = ev.target.closest('button[data-a]');
    if (!b) return;
    const u = b.dataset.u, a = b.dataset.a;
    if (a === 'remove' && !confirm('移除账号将删除池状态与 auths/ 下的凭证文件，且不可恢复。确认移除？')) return;
    if (a === 'disable' && !confirm('禁用后该账号不再参与选号，需手动解冻才能恢复。确认禁用？')) return;
    if (a === 'enable' && !confirm('重新启用该账号，让它立刻参与选号？')) return;
    b.disabled = true;
    try {
      const r = await api('accounts/' + encodeURIComponent(u) + '/' + a, { method: 'POST', body: '{}' });
      if (a === 'checkin') {
        toast(r.status === 'already' ? '今天已经签过了'
          : (r.earned ? '签到完成 +' + r.earned + ' 分' : '签到完成，本次没有新增积分'), 'ok');
      } else if (a === 'balance') {
        toast('积分已刷新' + (r.account ? '，积分包 ' + r.account.credits +
          (r.account.credits_total ? '/' + r.account.credits_total : '') : ''), 'ok');
      } else if (a === 'clear-cooldown') toast('已解除冷却', 'ok');
      else if (a === 'disable') toast('已禁用', 'ok');
      else if (a === 'enable') toast('已启用', 'ok');
      else toast('已移除', 'ok');
    } catch (e) { toast(e.message, 'err'); }
    finally { b.disabled = false; loadOverview(true); }
  });

  $('btnModels').onclick = loadModels;
  // 模型筛选控件：输入框防抖 120ms（长列表不必逐字符重排），下拉即时。
  let mdQTimer = null;
  $('mdQ').addEventListener('input', () => {
    clearTimeout(mdQTimer);
    mdQTimer = setTimeout(() => { mdFilter.q = $('mdQ').value.trim(); renderModels(); }, 120);
  });
  for (const [id, key] of [['mdRealm', 'realm'], ['mdFn', 'fn'], ['mdSort', 'sort']]) {
    const el = $(id);
    if (!el) continue;
    el.onchange = () => { mdFilter[key] = el.value; renderModels(); };
  }
  $('mdReset').onclick = resetModelFilter;

  $('btnPk').onclick = loadPackages;
  // 逐包明细的「其余未用完 / 已用完」折叠：委托式，行是渲染时生成的也不丢事件。
  $('pkDetail').addEventListener('click', ev => {
    const btn = ev.target.closest('button[data-pk-group]');
    if (!btn) return;
    const body = btn.closest('tbody');
    if (!body) return;
    const group = btn.dataset.pkGroup;
    const expanded = btn.getAttribute('aria-expanded') === 'true';
    body.querySelectorAll('tr[data-pk-row="' + group + '"]').forEach(row => { row.hidden = expanded; });
    const count = btn.dataset.count || '0';
    const size = btn.dataset.size || '0';
    const remain = btn.dataset.remain || '0';
    btn.setAttribute('aria-expanded', String(!expanded));
    if (group === 'rest') {
      btn.textContent = expanded
        ? '其余未用完 ' + count + ' 个包（面额合计 ' + fmtTok(size) + ' · 剩余 ' +
          fmtTok(remain) + '），展开'
        : '收起其余未用完 ' + count + ' 个包';
    } else {
      btn.textContent = expanded
        ? '已用完 ' + count + ' 个包（面额合计 ' + fmtTok(size) + '），展开'
        : '收起已用完 ' + count + ' 个包';
    }
  });

  $('btnUsageReload').onclick = () => loadUsage();
  $('usDimTabs').addEventListener('click', ev => {
    const b = ev.target.closest('button[data-dim]');
    if (!b) return;
    usDim = b.dataset.dim;
    renderUsageDim();
  });
  $('usSort').onchange = () => { usSort = $('usSort').value; renderUsageDim(); };
  $('usChips').addEventListener('click', ev => {
    const b = ev.target.closest('button[data-h]');
    if (!b) return;
    usHours = b.dataset.h;
    document.querySelectorAll('#usChips .chip').forEach(c => c.classList.toggle('on', c === b));
    loadUsage();
  });

  $('logChips').addEventListener('click', ev => {
    const b = ev.target.closest('button[data-ch]');
    if (!b) return;
    logCh = b.dataset.ch;
    document.querySelectorAll('#logChips .chip').forEach(c => c.classList.toggle('on', c === b));
    loadLogs();
  });
  $('btnLogPin').onclick = () => {
    logPin = !logPin;
    $('btnLogPin').textContent = '自动滚动：' + (logPin ? '开' : '关');
  };
  bindRequestLogControls();

  $('cfgForm').addEventListener('input', ev => {
    if (DURATION_FIELDS.includes(ev.target.name)) markDurationFields();
  });
  $('btnCfgReload').onclick = loadConfig;
  $('cfgForm').addEventListener('submit', async ev => {
    ev.preventDefault();
    if (DURATION_FIELDS.some(n => $('cfgForm').elements[n].classList.contains('invalid'))) {
      toast('有时长字段格式不对，已标红', 'err');
      return;
    }
    const btn = $('btnCfgSave');
    btn.disabled = true;
    try {
      const d = await api('config', { method: 'POST', body: JSON.stringify(collectConfig()) });
      // 密钥改了：本页立刻换上新的，否则下一次轮询就 401 把钥匙门弹出来。
      const nk = cfgLoaded.api_key || '';
      if (nk !== storedKey()) saveKey(nk);
      const f = d.restart_required || [];
      $('cfgNote').textContent = f.length ? ('已保存。重启后生效：' + f.join('、')) : '已保存，全部字段已热生效。';
      toast('配置已保存', 'ok');
    } catch (e) { toast('保存失败：' + e.message, 'err'); }
    finally { btn.disabled = false; }
  });

  const h = (location.hash || '#accounts').slice(1);
  go(TITLES[h] ? h : 'accounts');

  // 启动探测：需要密钥时由 401 拉出密钥门（本机免鉴权则静默通过）。
  api('overview').then(() => start()).catch(e => {
    if (!String(e.message).includes('密钥')) toast(e.message, 'err');
  });
}

document.addEventListener('DOMContentLoaded', boot);
