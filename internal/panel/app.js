'use strict';
/* ── 状态 ─────────────────────────────────────────────────────────── */
/* ponytail: 密钥放 sessionStorage（WB 用 localStorage）。面板可经公网隧道访问，
   密钥不该跨标签页长期驻留；换票落盘后重新输入一次即可。 */
const SS_KEY = 'tw2a_key', LS_THEME = 'tw2a_theme';
let key = '';
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

/* ── 请求 ─────────────────────────────────────────────────────────── */
async function api(path, opts = {}) {
  const h = Object.assign({}, opts.headers || {});
  const sent = key;                       // 本次实际发出的密钥
  if (key) h['Authorization'] = 'Bearer ' + key;
  if (opts.body) h['Content-Type'] = 'application/json';
  const r = await fetch('/panel/api/' + path, Object.assign({}, opts, { headers: h }));
  if (r.status === 401) {
    // 后台轮询发出时还没密钥，这次 401 回来时密钥已填好——不能把弹窗再盖上去。
    if (sent === key) openKey();
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
  key = v;
  try { sessionStorage.setItem(SS_KEY, key); } catch (e) {}
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
const TITLES = { accounts: '账号池', models: '模型', usage: '用量', config: '配置', logs: '运行日志' };
function go(v) {
  view = v;
  document.querySelectorAll('.view').forEach(s => s.hidden = s.id !== 'view-' + v);
  document.querySelectorAll('.nav a').forEach(a => a.classList.toggle('on', a.dataset.view === v));
  $('ttl').textContent = TITLES[v];
  if (!ready) return;                    // 认证探测通过后 start() 会再调一次
  if (v === 'accounts') loadOverview(true);
  if (v === 'models' && !$('mdBody').children.length) loadModels();
  if (v === 'usage') loadUsage(true);
  if (v === 'config') loadConfig();
  if (v === 'logs') loadLogs();
}

/* ── 账号池 ───────────────────────────────────────────────────────── */
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
    const acts = ['<button class="xs" data-a="checkin" data-u="' + esc(a.uid) + '">签到</button>',
      '<button class="xs" data-a="balance" data-u="' + esc(a.uid) + '">刷新</button>'];
    if (a.cooling) acts.push('<button class="xs" data-a="clear-cooldown" data-u="' + esc(a.uid) + '">解除冷却</button>');
    if (a.disabled) acts.push('<button class="xs" data-a="enable" data-u="' + esc(a.uid) + '">启用</button>');
    if (!a.disabled) acts.push('<button class="xs danger" data-a="disable" data-u="' + esc(a.uid) + '">禁用</button>');
    acts.push('<button class="xs danger" data-a="remove" data-u="' + esc(a.uid) + '">移除</button>');
    return '<tr class="' + cls + '"><td class="mark" aria-hidden="true"><i></i></td>' +
      '<td class="who"><div class="nm">' + esc(a.nickname || a.uid) + '</div><div class="id">' + esc(a.uid) + '</div></td>' +
      '<td>' + st.join(' ') + '</td>' +
      '<td class="cred" title="剩余 / 总额（上游积分包）"><div class="n">' + (a.credits || 0) +
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
async function loadModels() {
  const tb = $('mdBody');
  tb.innerHTML = '<tr><td colspan="6"><div class="empty">正在向上游查询…</div></td></tr>';
  try {
    const d = await api('models');
    const list = d.data || [];
    if (!list.length) {
      tb.innerHTML = '<tr><td colspan="6"><div class="empty">上游未返回模型</div></td></tr>';
      return;
    }
    tb.innerHTML = list.map(m =>
      '<tr><td class="mark" aria-hidden="true"><i></i></td>' +
      '<td class="who"><div class="nm">' + esc(m.id) + '</div></td>' +
      '<td>' + (m.capability ? tag('mute', m.capability) : '—') + '</td>' +
      '<td>' + thinkCell(m) + '</td>' +
      '<td>' + tag('mute', m.owned_by || '—') + '</td>' +
      '<td class="num">' + (m.context_length ? Math.round(m.context_length / 1000) + 'K' : '—') + '</td></tr>'
    ).join('');
    $('mdNote').textContent = list.length + ' 个模型 · 路由层缓存 1 小时';
  } catch (e) {
    tb.innerHTML = '<tr><td colspan="6"><div class="empty">' + esc(e.message) + '</div></td></tr>';
    $('mdNote').textContent = '查询失败';
  }
}

/* ── 日志（频道：全部/任务/对话/系统）─────────────────────────────── */
async function loadLogs() {
  const box = $('logBox');
  const atEnd = box.scrollTop + box.clientHeight >= box.scrollHeight - 24;
  try {
    const d = await api('logs');
    const all = d.entries || [];
    const entries = all.filter(e => logCh === 'all' || e.ch === logCh);
    box.innerHTML = entries.length
      ? entries.map(e => {
        const lvl = /error|失败|错误/.test(e.text) ? ' e' : /warn|冷却|熔断/.test(e.text) ? ' w' : '';
        const t = e.ts ? new Date(e.ts).toLocaleTimeString('zh-CN', { hour12: false }) : '';
        const ch = logCh === 'all'
          ? '<i class="lch c-' + esc(e.ch) + '">' + esc({ task: '任务', chat: '对话', sys: '系统' }[e.ch] || e.ch) + '</i>'
          : '';
        return '<span class="ln' + lvl + '">' + ch + esc(t + ' ' + e.text) + '</span>';
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

/* aggCells 请求/失败/输入/输出/合计五列，三个表共用（列头顺序一致）。 */
function aggCells(a) {
  const err = a.errors ? '<span style="color:var(--bad)">' + fmtNum(a.errors) + '</span>' : '0';
  return '<td class="num">' + fmtNum(a.requests) + '</td>' +
    '<td class="num">' + err + '</td>' +
    '<td class="num">' + fmtNum(a.prompt_tokens) + '</td>' +
    '<td class="num">' + fmtNum(a.completion_tokens) + '</td>' +
    '<td class="num">' + fmtNum(a.total_tokens) + '</td>';
}
function usRow(name, sub, a, extra) {
  return '<tr><td class="mark" aria-hidden="true"><i></i></td>' +
    '<td class="who"><div class="nm">' + esc(name) + '</div>' +
    (sub ? '<div class="id">' + esc(sub) + '</div>' : '') + '</td>' +
    aggCells(a) + (extra || '') + '</tr>';
}
function usEmpty(tb, cols, msg) {
  tb.innerHTML = '<tr><td colspan="' + cols + '"><div class="empty">' + esc(msg) + '</div></td></tr>';
}

function renderUsage(d) {
  const t = d.totals || {};
  $('usReq').textContent = fmtNum(t.requests);
  $('usErr').textContent = fmtNum(t.errors);
  $('usPt').textContent = fmtNum(t.prompt_tokens);
  $('usCt').textContent = fmtNum(t.completion_tokens);
  $('usTt').textContent = fmtNum(t.total_tokens);
  $('usLat').textContent = fmtLat(t.avg_latency_ms);
  $('usNote').textContent = '速率 ' + fmtRate(t.avg_tokens_per_second) +
    (d.since ? ' · 自 ' + d.since + ' 起记录' : '') +
    ' · ' + fmtNum(d.buckets) + ' 个分片 / ' + fmtNum(d.file_bytes) + ' B';

  const models = d.by_model || [];
  if (models.length) {
    $('usModelBody').innerHTML = models.map(m => usRow(m.key, '', m,
      '<td class="num">' + fmtLat(m.avg_latency_ms) + '</td>' +
      '<td class="num">' + fmtRate(m.avg_tokens_per_second) + '</td>')).join('');
  } else usEmpty($('usModelBody'), 9, '这个窗口内还没有调用');

  const accts = d.by_account || [];
  if (accts.length) {
    $('usAcctBody').innerHTML = accts.map(a => usRow(a.extra || a.key, a.extra ? a.key : '', a,
      '<td class="num">' + fmtLat(a.avg_latency_ms) + '</td>')).join('');
  } else usEmpty($('usAcctBody'), 8, '这个窗口内还没有调用');
  $('usAcctNote').textContent = accts.length ? accts.length + ' 个账号有调用' : '';

  const series = d.series || [];
  if (series.length) {
    $('usSeriesBody').innerHTML = series.map(p => usRow(
      p.t, p.scope === 'day' ? '日' : '小时', p)).join('');
  } else usEmpty($('usSeriesBody'), 7, '这个窗口内还没有调用');
  $('usSeriesNote').textContent = usHours === '0' ? '全部历史（含折叠日桶）' : '按小时分片';
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
      if (Array.isArray(v)) el.value = v.join(', ');
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

/* ── 添加账号（不轮询：贴回调地址换票）────────────────────────────── */
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
  api('login/start', { method: 'POST', body: '{}' }).then(d => {
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
  try { key = sessionStorage.getItem(SS_KEY) || ''; } catch (e) {}
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
    if (view === 'config') loadConfig();
    if (view === 'logs') loadLogs();
  };
  $('btnAdd').onclick = openAdd;
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

  $('btnCheckinAll').onclick = async () => {
    try {
      const d = await api('checkin', { method: 'POST', body: '{}' });
      toast('签到完成，积分合计 ' + (d.accounts || []).reduce((s, a) => s + (a.credits || 0), 0), 'ok');
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
      if (a === 'checkin' || a === 'balance') {
        toast((a === 'checkin' ? '签到完成' : '积分已刷新') + (r.account ? '，积分 ' + r.account.credits : ''), 'ok');
      } else if (a === 'clear-cooldown') toast('已解除冷却', 'ok');
      else if (a === 'disable') toast('已禁用', 'ok');
      else if (a === 'enable') toast('已启用', 'ok');
      else toast('已移除', 'ok');
    } catch (e) { toast(e.message, 'err'); }
    finally { b.disabled = false; loadOverview(true); }
  });

  $('btnModels').onclick = loadModels;

  $('btnUsageReload').onclick = () => loadUsage();
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
      if (nk !== key) {
        key = nk;
        try { sessionStorage.setItem(SS_KEY, key); } catch (e) {}
      }
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
