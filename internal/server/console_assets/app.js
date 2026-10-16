/* ============================================================================
 * Trae2Api 控制台前端
 *
 * 视觉与结构来自参考实现（workbuddy2api/internal/server/dashboard.html），
 * 但**数据层整体改绑**到 Trae2Api 自己的端点。参考实现调用的是 /ui/*，
 * 本项目一个都没有 —— 照抄会导致整页按钮全坏，所以这里逐个改绑：
 *
 *   参考实现                         Trae2Api
 *   ------------------------------   ----------------------------------------
 *   GET  /ui/data                    GET  /status + /v1/models + /v1/stats
 *   POST /ui/oauth/start             POST /admin/login/start
 *   POST /ui/oauth/poll              GET  /admin/login/status?session_id=
 *   POST /ui/oauth/batch_import      POST /admin/accounts/import
 *   POST /ui/account/admin           POST /admin/accounts/revive | control | delete
 *                                    POST /admin/accounts/{uid}/clear-cooldown
 *   POST /ui/action/delete           DELETE /admin/accounts/{name}
 *   POST /ui/action/signin           POST /admin/checkin
 *   POST /ui/action/trial            （无对应端点 —— 按钮明确禁用，不做假动作）
 *   GET  /ui/admin/users             GET  /admin/users
 *   POST /ui/admin/user/create       POST /admin/users (+ /reset-password)
 *   POST /ui/admin/user/reset_key    POST /admin/users/{id}/reset-key
 *   POST /ui/admin/user/toggle       POST /admin/users/{id}/disable|enable
 *   POST /ui/admin/user/role         POST /admin/users/{id}/role
 *   POST /ui/admin/user/delete       DELETE /admin/users/{id}
 *   POST /ui/admin/system/allow_reg  POST /admin/system/allow_register
 *   GET  /ui/auth/*                  POST /auth/login|register|logout, /me/*
 *   POST /ui/config/password         POST /admin/password
 *   GET  /ui/dsh/status              GET  /admin/dsh/status
 *   POST /ui/dsh/start|stop|reset    POST /admin/dsh/start|stop|reset
 *   POST /ui/dsh/install             POST /admin/dsh/install（只检测，不下载）
 *   POST /ui/dsh/export              GET  /admin/dsh/export
 *   GET  /ui/system/update/check     GET  /admin/system/update/check
 *
 * 约定：
 *   - 所有请求都是**同源相对路径**（页面在 /console，相对路径解析到根）。
 *   - API Key 存 localStorage('trae2api_key')，请求带 Authorization: Bearer。
 *   - 后端错误统一形如 {"error":{"code","message"}}，这里统一抽成 Error.message。
 *
 * ⛔ 硬约束（别破坏它）：**页面不得引用任何外域资源**。
 *   CSP 是 default-src 'self'，任何外域 CSS/JS/字体都会被浏览器直接拦掉，症状是
 *   "页面加载了但样式全无、JS 不执行"，而控制台自己还不报错 —— 极难排查。所以：
 *     · 不引外网 CSS/JS/字体（Tailwind 本地编译，字体用系统字体栈）；
 *     · **`/v1/models` 的 `icon_url` 指向 Trae 的对象存储域名，永远不要把它渲染成
 *       img 标签的 src** —— 一旦渲染，C3「零外部资源」当场破功。
 *   （注意：TestConsoleNoExternalResources 是对整页 HTML 做**文本**扫描的，连注释里
 *     写出那几段被禁的域名片段都会判失败 —— 这条注释本身就是被它逼着改过一次的。
 *     所以这里刻意只用"外域 CDN / 外网字体"这种说法，不写域名。）
 * ========================================================================== */

'use strict';

// ---------------------------------------------------------------- 全局状态

var currentActiveTab = 'accounts';
var currentUser = null;          // {id, username, role} —— 来自 /auth/login 或 /auth/register
var hasWebPassword = false;      // 控制台管理密码（/admin/password）
var currentApiKey = '';          // 当前生效的 API Key（localStorage）
var cachedStatus = null;         // /status 快照
var cachedAccounts = [];         // cachedStatus.accounts
var cachedModels = [];           // /v1/models data
var cachedStats = null;          // /v1/stats 的 stats 对象
var cachedMetricsSavedAt = '';   // /v1/stats 的 metrics_last_save
var selectedMetricsDate = 'today';
var accountFileByUid = {};       // uid -> 快照文件名（删除用；来自 /admin/accounts）
var selectedUids = {};           // uid -> true（批量动作选择集）
var systemAllowRegister = false; // /admin/system 的 allow_register
var systemInfoAvailable = false; // /admin/system 是否可用（后端可能尚未实现）
var rawAdminUsers = [];          // /admin/users 原始列表
var rawAdminUserFilter = '';

var currentChatFile = null;      // {type:'image'|'doc', name, data|text, ext}
var dshState = null;
var dshCurrentUrl = '';
var dshIframeSrc = '';
var loginEnforced = false;

// ---------------------------------------------------------------- 基础工具

function escapeHtml(str) {
  return String(str == null ? '' : str).replace(/[&<>"']/g, function (s) {
    return ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' })[s];
  });
}
function $(id) { return document.getElementById(id); }
function setText(id, text) { var el = $(id); if (el) el.innerText = text; }
function setHtml(id, html) { var el = $(id); if (el) el.innerHTML = html; }
function show(id) { var el = $(id); if (el) el.classList.remove('hidden'); }
function hide(id) { var el = $(id); if (el) el.classList.add('hidden'); }
function num(v) { return (typeof v === 'number' && isFinite(v)) ? v : 0; }
function fmtInt(v) { return num(v).toLocaleString(); }
function secs(n) {
  n = Math.max(0, Math.round(num(n)));
  if (n >= 3600) return Math.floor(n / 3600) + 'h' + Math.floor((n % 3600) / 60) + 'm';
  if (n >= 60) return Math.floor(n / 60) + 'm' + (n % 60) + 's';
  return n + 's';
}
// 哨兵 -1 表示"无观测"，不能当 0 展示
function fmtNoObs(v, suffix, digits) {
  if (v === null || v === undefined || v === -1) return '<span class="text-slate-500">—</span>';
  var x = Number(v);
  if (!isFinite(x)) return '<span class="text-slate-500">—</span>';
  return (digits ? x.toFixed(digits) : x) + (suffix || '');
}
function truncateTime(s) {
  if (!s) return '—';
  return String(s).replace('T', ' ').replace('Z', '').slice(0, 19);
}
function copyToClipboard(text, msg) {
  if (!text) { toast('没有可复制的内容', 'warn'); return; }
  var done = function () { toast(msg || '已复制到剪贴板', 'ok'); };
  if (navigator.clipboard && navigator.clipboard.writeText) {
    navigator.clipboard.writeText(text).then(done, function () { prompt('请手动按 Ctrl+C 复制:', text); });
  } else {
    var ta = document.createElement('textarea');
    ta.value = text; document.body.appendChild(ta); ta.select();
    try { document.execCommand('copy'); } catch (e) { /* 忽略 */ }
    document.body.removeChild(ta); done();
  }
}
function copyText(elementId) {
  var el = $(elementId);
  if (!el) return;
  copyToClipboard((el.value !== undefined ? el.value : el.innerText).trim());
}

// toast：比 alert 少打断操作，但错误仍会明确显示
function toast(text, kind) {
  var box = $('t2aToastBox');
  if (!box) {
    box = document.createElement('div');
    box.id = 't2aToastBox';
    box.className = 'fixed bottom-4 right-4 z-[100] space-y-2 max-w-sm';
    document.body.appendChild(box);
  }
  var color = kind === 'err' ? 'border-rose-500/40 bg-rose-950/80 text-rose-200'
    : kind === 'warn' ? 'border-amber-500/40 bg-amber-950/80 text-amber-200'
      : kind === 'ok' ? 'border-emerald-500/40 bg-emerald-950/80 text-emerald-200'
        : 'border-slate-700 bg-slate-900/95 text-slate-200';
  var div = document.createElement('div');
  div.className = 'glass-panel border rounded-xl px-3.5 py-2.5 text-xs shadow-2xl whitespace-pre-wrap ' + color;
  div.innerText = String(text);
  box.appendChild(div);
  setTimeout(function () { div.remove(); }, kind === 'err' ? 9000 : 4500);
}
function errText(e) {
  if (!e) return '未知错误';
  return e.message || String(e);
}

// ---------------------------------------------------------------- HTTP 层

function authHeaders(extra) {
  var h = Object.assign({ 'Accept': 'application/json' }, extra || {});
  if (currentApiKey) h['Authorization'] = 'Bearer ' + currentApiKey;
  return h;
}

// api：所有 JSON 端点的统一入口。非 2xx 抛 Error（message 取后端的 error.message）。
async function api(path, opts) {
  opts = opts || {};
  if (path && typeof path === 'string' && !path.startsWith('/') && !path.startsWith('http')) {
    path = '/' + path;
  }
  var init = { method: opts.method || 'GET', headers: authHeaders(opts.headers), credentials: 'same-origin' };
  if (opts.body !== undefined) {
    if (typeof opts.body === 'string' || opts.body instanceof FormData) {
      init.body = opts.body;
    } else {
      init.headers['Content-Type'] = 'application/json';
      init.body = JSON.stringify(opts.body);
    }
  }
  var res = await fetch(path, init);
  var text = await res.text();
  var json = null;
  try { json = JSON.parse(text); } catch (e) { /* 非 JSON */ }
  if (!res.ok) {
    var msg = '';
    if (json && json.error) {
      msg = typeof json.error === 'string' ? json.error : (json.error.message || json.error.code || '');
    }
    if (!msg) msg = text ? text.slice(0, 300) : ('HTTP ' + res.status);
    var err = new Error(msg);
    err.status = res.status;
    err.body = json;
    throw err;
  }
  return json;
}
function post(path, body) { return api(path, { method: 'POST', body: body }); }

// 端点不存在（后端角色尚未实现 / 版本较旧）时给出可读提示，而不是静默失败
function isMissingEndpoint(e) {
  return e && (e.status === 404 || e.status === 405 || e.status === 501);
}

// ---------------------------------------------------------------- 标签页

function switchMainTab(tab) {
  currentActiveTab = tab;
  var tabs = ['accounts', 'access', 'analytics', 'playground', 'dsh', 'users'];
  for (var i = 0; i < tabs.length; i++) {
    var t = tabs[i];
    var btn = $('mainTabBtn-' + t);
    var panel = $('tabContent-' + t);
    if (t === tab) {
      if (btn) btn.className = 'px-4 py-2 rounded-xl text-xs font-semibold flex items-center gap-2 bg-indigo-600 text-white shadow-md shadow-indigo-600/25 transition whitespace-nowrap';
      if (panel) panel.classList.remove('hidden');
    } else {
      if (btn) {
        var def = (t === 'users') ? 'text-purple-400 hover:text-purple-300 hover:bg-purple-500/10' : 'text-slate-400 hover:text-white hover:bg-slate-800/60';
        btn.className = 'px-4 py-2 rounded-xl text-xs font-semibold flex items-center gap-2 ' + def + ' transition whitespace-nowrap';
      }
      if (panel) panel.classList.add('hidden');
    }
  }
  if (tab === 'dsh') fetchDshStatus(false);
  if (tab === 'users') loadAdminUsers();
  if (tab === 'analytics' && cachedStats) renderTokenAnalytics(cachedStats);
  var cmSel = $('chatModel');
  if (tab === 'playground' && cmSel && !cmSel.options.length) loadModels();
}
function toggleHeaderAuthMenu() {
  var menu = $('headerAuthDropdown');
  if (menu) menu.classList.toggle('hidden');
}
document.addEventListener('click', function (e) {
  var menu = $('headerAuthDropdown');
  var btn = $('btnHeaderAuthMenu');
  if (menu && btn && !menu.classList.contains('hidden')) {
    if (!menu.contains(e.target) && !btn.contains(e.target)) menu.classList.add('hidden');
  }
});
window.addEventListener('keydown', function (e) {
  if (e.key === 'Escape') {
    closeOAuthModal(); closeImportModal(); showLoginModal(false);
    hide('changePasswordModal'); hide('updateModal');
  }
});

// ---------------------------------------------------------------- 主刷新

async function refreshData() {
  var icon = $('refreshIcon');
  if (icon) icon.classList.add('animate-spin');
  try {
    var st = await api('status');
    cachedStatus = st;
    cachedAccounts = st.accounts || [];
    showLoginModal(false);
    renderStatus(st);
    await Promise.all([
      loadModels(),
      loadStats(),
      loadSnapshots(),
      loadPasswordStatus(),
      loadSystemInfo()
    ]);
  } catch (e) {
    if (e && e.status === 401) {
      // 两种情况都落在 401：① 配了全局 API Key 但页面还没填；② 启用了用户体系
      showLoginModal(true, false);
      toast('需要凭据：请在「接入」页填入 API Key，或用用户名 / 密码登录', 'warn');
    } else {
      toast('读取状态失败：' + errText(e), 'err');
    }
  } finally {
    if (icon) setTimeout(function () { icon.classList.remove('animate-spin'); }, 400);
  }
}

function renderStatus(st) {
  // 账号列表与统计同源：都在这里落到模块状态，renderStatus 可被单独调用
  // （冒烟测试就是这么用的 —— 依赖调用顺序的隐式状态最容易在改动中失效）。
  cachedAccounts = st.accounts || [];
  setText('statTotal', num(st.total));
  setText('statHealthy', num(st.healthy));
  setText('statCooling', num(st.cooling));
  setText('statDisabled', num(st.disabled));
  setText('statSticky', num(st.sticky_sessions));
  setText('statInFlight', num(st.in_flight_full));
  setText('accountCountBadge', cachedAccounts.length);
  var ver = $('appVersionText');
  if (ver && st.version) ver.innerText = st.version;

  var chips = (st.channels || []).map(function (c) {
    return '<span class="inline-flex items-center gap-1 mr-2 mb-1"><span class="w-1.5 h-1.5 rounded-full bg-indigo-400"></span>' +
      escapeHtml(c.realm) + ' ' + num(c.accounts) + ' 账号 / ' + num(c.models) + ' 模型</span>';
  }).join('');
  setHtml('poolSubtitle', '运行 ' + secs(st.uptime_seconds) +
    ' · 会话粘性 ' + (st.sticky_enabled ? ('开启（' + num(st.sticky_sessions) + ' 个绑定）') : '关闭') +
    ' · 账号池按归属隔离' + (chips ? '<br>' + chips : ''));

  renderAccounts(cachedAccounts);
}

// ---------------------------------------------------------------- 账号池

// 模型域（realm: trae / solo / trae-intl / solo-intl）与账号产品键（product:
// trae-cn / solo-cn / trae-intl / solo-intl）**不是同一个字符串**：
// 域是模型名前缀（`trae:glm-5.2`），产品键是账号凭据来源。
// pool.Account 同时带 Realm（域）与 Product（产品键），优先用 Realm 精确匹配。
function accountsOfRealm(realm, list) {
  list = list || cachedAccounts || [];
  return list.filter(function (a) {
    if (a.realm) return a.realm === realm;
    var p = a.product || '';
    return p === realm || p === realm + '-cn';
  });
}
function realmLabel(realm) {
  switch (realm) {
    case 'solo': return 'SOLO CN';
    case 'solo-intl': return 'SOLO INTL';
    case 'trae-intl': return 'TRAE INTL';
    case 'agent': return 'AGENT';
    case 'trae': return 'TRAE CN';
  }
  return String(realm || '').toUpperCase();
}

function realmBadge(a) {
  var key = a.realm || a.product || (a.uid || '').split('-').slice(0, 2).join('-');
  var label = realmLabel(key);
  var cls = 'bg-blue-500/10 text-blue-400 border-blue-500/20';
  if (key === 'solo') {
    cls = 'bg-emerald-500/10 text-emerald-400 border-emerald-500/20';
  } else if (key === 'trae-intl') {
    cls = 'bg-sky-500/10 text-sky-400 border-sky-500/20';
  } else if (key === 'solo-intl') {
    cls = 'bg-teal-500/10 text-teal-400 border-teal-500/20';
  }
  return '<span class="inline-flex items-center gap-1 px-2.5 py-0.5 rounded-full text-[11px] font-semibold border ' + cls + '">' + escapeHtml(label) + '</span>';
}

function accountStatusBadge(a) {
  var mc = a.model_cooling || {};
  var mcKeys = Object.keys(mc);
  if (a.disabled) {
    return '<span class="inline-flex items-center gap-1.5 text-rose-400 text-xs" title="' + escapeHtml(a.disable_reason || '') + '">' +
      '<span class="w-1.5 h-1.5 rounded-full bg-rose-400"></span>手动停用</span>';
  }
  if (a.cooling) {
    return '<span class="inline-flex items-center gap-1.5 text-amber-400 text-xs cursor-help" title="' + escapeHtml(a.cool_reason || '上游触发频控/限流保护') + '">' +
      '<span class="w-1.5 h-1.5 rounded-full bg-amber-400 animate-pulse"></span>限流冷却中 (' + secs(a.cool_remain_seconds) + ')</span>';
  }
  if (a.breaker) {
    return '<span class="inline-flex items-center gap-1.5 text-amber-400 text-xs" title="连续失败触发熔断">' +
      '<span class="w-1.5 h-1.5 rounded-full bg-amber-400 animate-pulse"></span>熔断中 (' + secs(a.breaker_remain_seconds) + ')</span>';
  }
  if (a.degraded) {
    return '<span class="inline-flex items-center gap-1.5 text-purple-400 text-xs" title="提示词降级（上游拦截 system 提示词）">' +
      '<span class="w-1.5 h-1.5 rounded-full bg-purple-400"></span>降权中 (' + secs(a.degrade_remain_seconds) + ')</span>';
  }
  if (mcKeys.length) {
    return '<span class="inline-flex items-center gap-1.5 text-amber-400 text-xs" title="部分模型冷却中">' +
      '<span class="w-1.5 h-1.5 rounded-full bg-amber-400 animate-pulse"></span>模型级冷却 (' + mcKeys.length + ')</span>';
  }
  if (num(a.in_flight) >= num(a.max_in_flight)) {
    return '<span class="inline-flex items-center gap-1.5 text-purple-400 text-xs">' +
      '<span class="w-1.5 h-1.5 rounded-full bg-purple-400"></span>在途已满 ' + num(a.in_flight) + '/' + num(a.max_in_flight) + '</span>';
  }
  return '<span class="inline-flex items-center gap-1.5 text-emerald-400 text-xs">' +
    '<span class="w-1.5 h-1.5 rounded-full bg-emerald-400"></span>健康在线</span>';
}

function modelCoolingBadges(a) {
  var mc = a.model_cooling || {};
  var keys = Object.keys(mc);
  if (!keys.length) {
    if (a.disabled || a.cooling || a.breaker) return '';
    return '<div class="mt-1 flex items-center gap-1 text-[10px] text-emerald-400/70 font-mono"><span class="w-1.5 h-1.5 rounded-full bg-emerald-400"></span>全模型正常</div>';
  }
  var out = '<div class="mt-1.5 flex flex-wrap gap-1 max-w-[340px]">';
  for (var i = 0; i < keys.length; i++) {
    var m = keys[i];
    out += '<span class="inline-flex items-center gap-1 px-1.5 py-0.5 rounded text-[10px] bg-amber-500/15 text-amber-300 border border-amber-500/30 font-mono" title="该模型在此账号上处于冷却">' +
      '<span class="w-1.5 h-1.5 rounded-full bg-amber-400 animate-pulse"></span>⚠️ ' + escapeHtml(m) + ' (' + secs(mc[m]) + ')</span>';
  }
  return out + '</div>';
}

function renderAccounts(accounts) {
  var tbody = $('accountsTable');
  if (!tbody) return;
  if (!accounts || !accounts.length) {
    tbody.innerHTML = '<tr><td colspan="6" class="px-6 py-12 text-center text-slate-500 font-sans">' +
      '<div class="flex flex-col items-center justify-center space-y-3">' +
      '<div class="w-12 h-12 rounded-2xl bg-slate-800/50 flex items-center justify-center text-slate-600">' +
      '<svg class="w-6 h-6" fill="none" stroke="currentColor" viewBox="0 0 24 24"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="1.5" d="M17 20h5v-2a3 3 0 00-5.356-1.857M17 20H7m10 0v-2c0-.656-.126-1.283-.356-1.857M7 20H2v-2a3 3 0 015.356-1.857M7 20v-2c0-.656.126-1.283.356-1.857m0 0a5.002 5.002 0 019.288 0M15 7a3 3 0 11-6 0 3 3 0 016 0zm6 3a2 2 0 11-4 0 2 2 0 014 0zM7 10a2 2 0 11-4 0 2 2 0 014 0z"/></svg></div>' +
      '<p class="text-sm text-slate-400 font-medium">账号池当前为空</p>' +
      '<p class="text-xs text-slate-500 max-w-sm">点右上「添加账号」走 OAuth 授权，或用「一键批量导入」导入凭证</p>' +
      '</div></td></tr>';
    updateSelectedCount();
    return;
  }

  var html = '';
  for (var i = 0; i < accounts.length; i++) {
    var a = accounts[i];
    var uid = a.uid || '';
    var checked = selectedUids[uid] ? ' checked' : '';
    var ownerTag = a.owner
      ? '<span class="ml-1.5 px-1.5 py-0.2 rounded text-[10px] bg-purple-500/20 text-purple-400 font-sans border border-purple-500/30" title="归属: ' + escapeHtml(a.owner) + '">专属账号</span>'
      : '<span class="ml-1.5 px-1.5 py-0.2 rounded text-[10px] bg-slate-800 text-slate-400 font-sans border border-slate-700">公共账号</span>';

    var revivable = a.disabled || a.cooling || a.breaker || a.degraded || Object.keys(a.model_cooling || {}).length > 0;
    var actions = '<div class="flex items-center justify-end gap-1.5">';
    actions += '<button onclick="testAccount(\'' + escapeHtml(uid) + '\')" title="发起连通性与额度测速探针" class="px-2 py-1 rounded-lg text-xs font-medium text-cyan-400 hover:text-cyan-300 hover:bg-cyan-500/10 border border-transparent hover:border-cyan-500/30 transition">测速</button>';
    actions += '<button onclick="setAccountProxy(\'' + escapeHtml(uid) + '\',\'' + escapeHtml(a.proxy || '') + '\')" title="配置该账号独立出口代理（HTTP/SOCKS5）" class="px-2 py-1 rounded-lg text-xs font-medium text-amber-400 hover:text-amber-300 hover:bg-amber-500/10 border border-transparent hover:border-amber-500/30 transition">代理</button>';
    if (revivable) {
      actions += '<button onclick="controlAccount(\'' + escapeHtml(uid) + '\',\'revive\')" title="清冷却 + 清熔断 + 清连败 + 清降权 + 清模型级冷却" class="px-2 py-1 rounded-lg text-xs font-medium text-amber-400 hover:text-amber-300 hover:bg-amber-500/10 border border-transparent hover:border-amber-500/30 transition">复活</button>';
    }
    if (a.cooling) {
      actions += '<button onclick="controlAccount(\'' + escapeHtml(uid) + '\',\'clear_cooling\')" title="只清账号级冷却" class="px-2 py-1 rounded-lg text-xs font-medium text-amber-400 hover:text-amber-300 hover:bg-amber-500/10 border border-transparent hover:border-amber-500/30 transition">解除冷却</button>';
    }
    actions += a.disabled
      ? '<button onclick="controlAccount(\'' + escapeHtml(uid) + '\',\'enable\')" class="px-2 py-1 rounded-lg text-xs font-medium text-emerald-400 hover:text-emerald-300 hover:bg-emerald-500/10 border border-transparent hover:border-emerald-500/30 transition">启用</button>'
      : '<button onclick="controlAccount(\'' + escapeHtml(uid) + '\',\'disable\')" class="px-2 py-1 rounded-lg text-xs font-medium text-slate-400 hover:text-slate-300 hover:bg-slate-800 border border-transparent hover:border-slate-700 transition">停用</button>';
    if (accountFileByUid[uid]) {
      actions += '<button onclick="deleteAccount(\'' + escapeHtml(uid) + '\')" class="px-2 py-1 rounded-lg text-xs font-medium text-rose-400 hover:text-rose-300 hover:bg-rose-500/10 border border-transparent hover:border-rose-500/30 transition">删除</button>';
    } else {
      actions += '<span class="px-2 py-1 text-xs text-slate-600 cursor-not-allowed" title="该账号来自 config.trae.accounts，需在配置文件中移除">删除</span>';
    }
    actions += '</div>';

    html += '<tr class="hover:bg-slate-800/40 transition">' +
      '<td class="px-4 py-4"><input type="checkbox" class="acct-check accent-indigo-500 cursor-pointer" data-uid="' + escapeHtml(uid) + '"' + checked + ' onchange="onAccountSelect()" /></td>' +
      '<td class="px-6 py-4"><div class="flex flex-col gap-1.5 items-start">' + realmBadge(a) +
      '<span class="text-[10px] text-slate-500 font-sans truncate max-w-[100px]">' + escapeHtml(a.label || '') + '</span></div></td>' +
      '<td class="px-6 py-4"><div class="font-sans font-semibold text-white text-xs flex items-center"><span>' + escapeHtml(a.label || uid) + '</span>' + ownerTag + '</div>' +
      '<div class="text-[11px] text-slate-500 font-mono mt-0.5">' + escapeHtml(uid) + '</div>' +
      (a.proxy ? '<div class="text-[10px] text-amber-400 font-mono mt-0.5 flex items-center gap-1" title="独立出口代理: ' + escapeHtml(a.proxy) + '"><span>⚡ 代理: ' + escapeHtml(a.proxy) + '</span></div>' : '') +
      modelCoolingBadges(a) + '</td>' +
      '<td class="px-6 py-4">' + accountStatusBadge(a) +
      '<div class="text-[10px] text-slate-500 font-mono mt-1">在途 ' + num(a.in_flight) + '/' + num(a.max_in_flight) +
      ' · 成功/失败 ' + num(a.ok_count) + '/' + num(a.err_count) + '</div></td>' +
      '<td class="px-6 py-4 font-mono font-medium text-slate-200">' +
      (function() {
        if (a.realm === 'trae-intl' || a.realm === 'solo-intl' || (a.product && a.product.indexOf('intl') >= 0)) {
          return '<span class="inline-flex items-center gap-1 text-sky-400 font-sans text-xs font-semibold bg-sky-500/10 px-2 py-0.5 rounded-full border border-sky-500/20">国际版订阅 · ' + num(a.credits || 1000) + '</span>';
        }
        return (a.credits === undefined || a.credits === null ? '<span class="text-slate-500">—</span>' : (num(a.credits) + ' <span class="text-[10px] text-slate-400 font-sans">积分</span>'));
      })() +
      '<div class="text-[10px] text-slate-500 mt-1">到期 ' + escapeHtml(truncateTime(a.token_expire)) + '</div></td>' +
      '<td class="px-6 py-4 text-right font-sans">' + actions + '</td>' +
      '</tr>';
  }
  tbody.innerHTML = html;
  syncSelectAllCheckbox();
  updateSelectedCount();
}

// ---- 选择集（批量动作） ----

function collectSelectedUids() {
  var out = [];
  var boxes = document.querySelectorAll('.acct-check');
  for (var i = 0; i < boxes.length; i++) if (boxes[i].checked) out.push(boxes[i].getAttribute('data-uid'));
  return out;
}
function onAccountSelect() { updateSelectedCount(); syncSelectAllCheckbox(); }
function updateSelectedCount() {
  var n = collectSelectedUids().length;
  setText('acctSelectedCount', '已选 ' + n);
}
function syncSelectAllCheckbox() {
  var all = $('acctSelectAll');
  if (!all) return;
  var boxes = document.querySelectorAll('.acct-check');
  var checked = 0;
  for (var i = 0; i < boxes.length; i++) if (boxes[i].checked) checked++;
  all.checked = boxes.length > 0 && checked === boxes.length;
  all.indeterminate = checked > 0 && checked < boxes.length;
}
function toggleSelectAllAccounts(checked) {
  var boxes = document.querySelectorAll('.acct-check');
  for (var i = 0; i < boxes.length; i++) boxes[i].checked = !!checked;
  updateSelectedCount();
}

// ---- 账号动作 ----

async function controlAccount(uid, action) {
  try {
    if (action === 'revive') {
      var r = await post('admin/accounts/revive', { uids: [uid], all: false });
      toast('已复活 ' + num(r.revived) + ' 个账号（冷却/熔断/连败/降权/模型冷却 全清）', 'ok');
    } else if (action === 'clear_cooling') {
      await post('admin/accounts/' + encodeURIComponent(uid) + '/clear-cooldown');
      toast('已解除账号级冷却（模型级冷却仍在，需要时用「复活」）', 'ok');
    } else if (action === 'enable' || action === 'disable') {
      await post('admin/accounts/' + encodeURIComponent(uid) + '/' + action);
      toast('已' + (action === 'enable' ? '启用' : '停用') + ' ' + uid, 'ok');
    }
    await refreshData();
  } catch (e) {
    if (action === 'revive' && isMissingEndpoint(e)) {
      toast('批量复活端点不可用（后端未实现 /admin/accounts/revive）', 'err');
    } else {
      toast('操作失败：' + errText(e), 'err');
    }
  }
}

async function testAccount(uid) {
  toast('正在测速账号 ' + uid + ' ...', 'ok');
  try {
    var r = await post('admin/accounts/' + encodeURIComponent(uid) + '/test');
    if (r.ok) {
      var credMsg = (uid.indexOf('intl') >= 0) ? '国际版权益正常（健康订阅）' : (Number(r.credits || 0).toFixed(2) + ' 积分');
      toast('✅ 账号 [' + uid + '] 测速成功！\n往返延迟: ' + r.latency_ms + 'ms\n状态/额度: ' + credMsg, 'ok');
      await refreshData();
    } else {
      toast('❌ 账号 [' + uid + '] 测速未通过：' + (r.error || r.message || '未知错误'), 'err');
    }
  } catch (e) {
    toast('测速异常：' + errText(e), 'err');
  }
}

async function setAccountProxy(uid, currentProxy) {
  var p = prompt('请输入账号 [' + uid + '] 专用的出口代理 URL\n支持 http / https / socks5 (例如 socks5://127.0.0.1:1080)；留空表示使用全局/系统代理:', currentProxy || '');
  if (p === null) return;
  try {
    var r = await post('admin/accounts/' + encodeURIComponent(uid) + '/proxy', { proxy: p.trim() });
    toast(r.message || '代理已更新', 'ok');
    await refreshData();
  } catch (e) {
    toast('设置代理失败：' + errText(e), 'err');
  }
}

async function deleteAccount(uid) {
  var file = accountFileByUid[uid];
  if (!file) { toast('该账号没有对应快照文件，无法删除', 'err'); return; }
  if (!confirm('确认删除账号 ' + uid + ' 的快照 ' + file + '？删除后该账号立即移出池。')) return;
  try {
    await api('admin/accounts/' + encodeURIComponent(file), { method: 'DELETE' });
    toast('已删除 ' + file, 'ok');
    await refreshData();
  } catch (e) { toast('删除失败：' + errText(e), 'err'); }
}

async function batchRevive() {
  var uids = collectSelectedUids();
  if (!uids.length) { toast('请先勾选要复活的账号', 'warn'); return; }
  try {
    var r = await post('admin/accounts/revive', { uids: uids, all: false });
    toast('批量复活完成：' + num(r.revived) + ' 个（清冷却/熔断/连败/降权/模型冷却）', 'ok');
    selectedUids = {};
    await refreshData();
  } catch (e) {
    toast(isMissingEndpoint(e) ? '批量复活端点不可用（后端未实现）' : ('批量复活失败：' + errText(e)), 'err');
  }
}
async function reviveAll() {
  if (!confirm('确认对**全部**账号执行复活（清冷却 + 清熔断 + 清连败 + 清降权 + 清模型级冷却）？')) return;
  try {
    var r = await post('admin/accounts/revive', { all: true });
    toast('全部复活完成：' + num(r.revived) + ' 个账号', 'ok');
    await refreshData();
  } catch (e) {
    toast(isMissingEndpoint(e) ? '批量复活端点不可用（后端未实现）' : ('全部复活失败：' + errText(e)), 'err');
  }
}
async function batchControl(action) {
  var uids = collectSelectedUids();
  if (!uids.length) { toast('请先勾选账号', 'warn'); return; }
  try {
    var r = await post('admin/accounts/control', { uids: uids, all: false, action: action });
    toast('批量' + (action === 'enable' ? '启用' : '停用') + '完成：' + num(r.changed) + ' 个', 'ok');
    await refreshData();
  } catch (e) {
    toast(isMissingEndpoint(e) ? '批量启停端点不可用（后端未实现）' : ('批量启停失败：' + errText(e)), 'err');
  }
}
async function batchDelete() {
  var uids = collectSelectedUids();
  if (!uids.length) { toast('请先勾选要删除的账号', 'warn'); return; }
  if (!confirm('确认删除勾选的 ' + uids.length + ' 个账号？其快照文件会被删除，操作不可逆。')) return;
  try {
    var r = await post('admin/accounts/delete', { uids: uids });
    var msg = '批量删除完成：' + num(r.deleted) + ' 个';
    if (r.errors && r.errors.length) msg += '\n未删除 ' + r.errors.length + ' 个：' + JSON.stringify(r.errors).slice(0, 300);
    toast(msg, r.errors && r.errors.length ? 'warn' : 'ok');
    selectedUids = {};
    await refreshData();
  } catch (e) {
    toast(isMissingEndpoint(e) ? '批量删除端点不可用（后端未实现）' : ('批量删除失败：' + errText(e)), 'err');
  }
}

async function runBatchSignin() {
  try {
    var r = await post('admin/checkin');
    toast('签到完成：新签 ' + num(r.checked) + '，已签 ' + num(r.already) +
      '，失败 ' + num(r.failed) + '，禁用 ' + num(r.disabled), num(r.failed) ? 'warn' : 'ok');
    await refreshData();
  } catch (e) { toast('签到失败：' + errText(e), 'err'); }
}
async function refreshCredits() {
  try {
    var r = await post('admin/credits/refresh');
    var c = await api('v1/credits');
    toast('额度缓存已刷新（' + num(r.refreshed) + ' 个账号）；合计剩余 ' + Number(c.total_remaining || 0).toFixed(2) + ' 积分', 'ok');
    await refreshData();
  } catch (e) { toast('刷新额度失败：' + errText(e), 'err'); }
}
async function resetWAF() {
  try {
    var r = await post('admin/waf/reset');
    toast('已解除 IP 级闸；此前原因：' + JSON.stringify((r.before && r.before.last_reason) || '—'), 'ok');
    await refreshData();
  } catch (e) { toast('解除 IP 闸失败：' + errText(e), 'err'); }
}
async function resetDegrade() {
  try {
    await post('admin/degrade/reset');
    toast('已解除提示词降级（账号级降权需要「复活」才清）', 'ok');
    await refreshData();
  } catch (e) { toast('解除降级失败：' + errText(e), 'err'); }
}

// 快照列表：只需要 uid → 文件名 的映射（删除账号用）与目录提示
async function loadSnapshots() {
  try {
    var r = await api('admin/accounts');
    accountFileByUid = {};
    var list = r.data || [];
    for (var i = 0; i < list.length; i++) {
      var e = list[i];
      var uid = (e.product || '') + '-' + (e.userId || '');
      if (e.product && e.userId) accountFileByUid[uid] = e.file;
    }
    window.__snapshotDir = r.dir || '';
    window.__snapshotCount = list.length;
  } catch (e) {
    if (e && e.status !== 401) toast('读取账号快照列表失败：' + errText(e), 'warn');
  }
}

// ---------------------------------------------------------------- 接入页

async function loadModels() {
  try {
    var r = await api('v1/models');
    cachedModels = r.data || [];
    renderModels(cachedModels, cachedAccounts);
  } catch (e) {
    setHtml('modelsList', '<div class="text-rose-400 text-center py-8">读取模型清单失败：' + escapeHtml(errText(e)) + '</div>');
  }
}

function renderModels(models, accounts) {
  accounts = accounts || cachedAccounts || [];
  var list = $('modelsList');
  var select = $('chatModel');
  var prev = select ? select.value : '';
  setText('modelCountBadge', models.length);
  if (!models.length) {
    setHtml('modelsList', '<div class="text-slate-500 text-center py-8">暂无可路由模型</div>');
    return;
  }
  var listHtml = '';
  var selectHtml = '';
  var hasPrev = false;
  for (var i = 0; i < models.length; i++) {
    var m = models[i];
    var realm = m.realm || String(m.id || '').split(':')[0];
    var bare = String(m.id || '').replace(/^[^:]+:/, '');
    var rl = realmLabel(realm);
    var badge = realm === 'solo' || realm === 'solo-intl'
      ? '<span class="text-[10px] font-mono bg-emerald-500/20 text-emerald-400 border border-emerald-500/30 px-1.5 py-0.2 rounded">' + escapeHtml(rl) + '</span>'
      : (realm === 'agent'
        ? '<span class="text-[10px] font-mono bg-cyan-500/20 text-cyan-300 border border-cyan-500/30 px-1.5 py-0.2 rounded">AGENT</span>'
        : '<span class="text-[10px] font-mono bg-blue-500/20 text-blue-400 border border-blue-500/30 px-1.5 py-0.2 rounded">' + escapeHtml(rl) + '</span>');

    var creditsBadge = (m.multiplier && m.multiplier !== '—')
      ? '<span class="text-[10px] font-mono bg-amber-500/15 text-amber-400 border border-amber-500/30 px-1.5 py-0.2 rounded font-semibold" title="上游真实消耗倍率">' + escapeHtml(m.multiplier) + '</span>'
      : '';
    var visionBadge = m.multimodal
      ? '<span class="text-[10px] font-mono bg-sky-500/20 text-sky-300 border border-sky-500/30 px-1.5 py-0.2 rounded" title="支持图片 / 文档多模态">✨ 多模态</span>'
      : '';
    var agentBadge = m.agent
      ? '<span class="text-[10px] font-mono bg-rose-500/15 text-rose-300 border border-rose-500/30 px-1.5 py-0.2 rounded" title="虚拟智能体模型：不能当上游模型调用">不可直接调用</span>'
      : '';

    // 可用性：按同域账号数与该模型的模型级冷却数推算
    var realmAccounts = accountsOfRealm(realm, accounts).filter(function (a) { return !a.disabled; });
    var cooling = 0;
    for (var j = 0; j < realmAccounts.length; j++) {
      var mc = realmAccounts[j].model_cooling || {};
      if (mc[bare] !== undefined) cooling++;
    }
    var total = realmAccounts.length;
    var ready = Math.max(0, total - cooling);
    var avail;
    if (m.agent) {
      avail = '<span class="text-[10px] font-mono bg-slate-800 text-slate-400 border border-slate-700/80 px-1.5 py-0.2 rounded" title="在 Agent (dsh) 标签页使用">仅 Agent 页可用</span>';
    } else if (total === 0) {
      avail = '<span class="text-[10px] font-mono bg-slate-800 text-slate-400 border border-slate-700/80 px-1.5 py-0.2 rounded">○ 无同域账号</span>';
    } else if (ready === 0) {
      avail = '<span class="text-[10px] font-mono bg-rose-500/15 text-rose-300 border border-rose-500/30 px-1.5 py-0.2 rounded font-semibold" title="同域账号全部处于该模型冷却">✕ 全池限流</span>';
    } else if (cooling > 0) {
      avail = '<span class="text-[10px] font-mono bg-amber-500/15 text-amber-300 border border-amber-500/30 px-1.5 py-0.2 rounded font-semibold" title="' + cooling + ' 个账号该模型冷却中">▲ ' + ready + '/' + total + ' 可用</span>';
    } else {
      avail = '<span class="text-[10px] font-mono bg-emerald-500/15 text-emerald-400 border border-emerald-500/30 px-1.5 py-0.2 rounded font-semibold" title="同域 ' + total + ' 个账号健康就绪">● ' + ready + '/' + total + ' 就绪</span>';
    }

    listHtml += '<div class="p-3 bg-slate-900/60 rounded-xl border border-slate-800/80 hover:border-slate-700 transition flex items-center justify-between group">' +
      '<div class="space-y-1 max-w-[230px]">' +
      '<div class="font-mono font-medium text-slate-200 flex items-center gap-1.5 flex-wrap">' +
      '<span class="truncate max-w-[130px] font-semibold" title="' + escapeHtml(m.id) + '">' + escapeHtml(m.id) + '</span>' + badge + avail + visionBadge + creditsBadge + agentBadge + '</div>' +
      '<div class="text-[11px] text-slate-400 truncate" title="' + escapeHtml(m.display_name || '') + '">' + escapeHtml(m.display_name || m.model_type || '官方模型') + '</div>' +
      '</div><div class="flex items-center gap-1.5">' +
      '<span class="text-[11px] font-mono text-slate-400 bg-slate-800/80 px-2 py-0.5 rounded-lg border border-slate-700/50">' +
      (m.context_window ? Math.round(m.context_window / 1024) + 'k' : 'auto') + '</span>' +
      '<button onclick="testModelQuick(\'' + escapeHtml(m.id) + '\')" class="text-[11px] text-slate-400 hover:text-emerald-400 p-1 rounded hover:bg-slate-800/80 transition" title="发一条 5-token 探针测连通性">' +
      '<svg class="w-3.5 h-3.5" fill="none" stroke="currentColor" viewBox="0 0 24 24"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M13 10V3L4 14h7v7l9-11h-7z"/></svg></button>' +
      '<button onclick="copyModelId(\'' + escapeHtml(m.id) + '\')" class="text-[11px] text-slate-400 hover:text-indigo-300 p-1 rounded hover:bg-slate-800/80 transition" title="复制模型 ID 并填入演练场">' +
      '<svg class="w-3.5 h-3.5" fill="none" stroke="currentColor" viewBox="0 0 24 24"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M8 5H6a2 2 0 00-2 2v12a2 2 0 002 2h10a2 2 0 002-2v-1M8 5a2 2 0 002 2h2a2 2 0 002-2M8 5a2 2 0 012-2h2a2 2 0 012 2m0 0h2a2 2 0 012 2v3m2 4H10m0 0l3-3m-3 3l3 3"/></svg></button>' +
      '</div></div>';

    var suffix = '';
    if (m.multimodal) suffix += ' [✨多模态]';
    if (m.multiplier && m.multiplier !== '—') suffix += ' (' + m.multiplier + ')';
    if (m.agent) suffix += ' [Agent 页专用]';
    else if (ready === 0 && total > 0) suffix += ' (✕当前不可用)';
    var label = m.id + suffix;
    if (m.id === prev) { hasPrev = true; selectHtml += '<option value="' + escapeHtml(m.id) + '" selected>' + escapeHtml(label) + '</option>'; }
    else selectHtml += '<option value="' + escapeHtml(m.id) + '">' + escapeHtml(label) + '</option>';
  }
  setHtml('modelsList', listHtml);
  if (select) {
    select.innerHTML = selectHtml;
    if (hasPrev) select.value = prev;
  }
}

async function loadPasswordStatus() {
  try {
    var r = await api('admin/password');
    hasWebPassword = !!r.enabled;
    updatePasswordBadge(hasWebPassword);
  } catch (e) {
    if (e && e.status !== 401) { hasWebPassword = false; updatePasswordBadge(false); }
  }
}
function updatePasswordBadge(hasPw) {
  var badge = $('webPasswordStatusBadge');
  if (badge) {
    if (hasPw) {
      badge.className = 'text-[10px] px-1.5 py-0.2 rounded bg-emerald-500/20 text-emerald-400 border border-emerald-500/30';
      badge.innerText = '已启用保护';
    } else {
      badge.className = 'text-[10px] px-1.5 py-0.2 rounded bg-slate-800 text-slate-400';
      badge.innerText = '未设密码（免密）';
    }
  }
  if (hasPw) hide('unprotectedWarningBanner'); else show('unprotectedWarningBanner');
}

async function loadSystemInfo() {
  try {
    var r = await api('admin/system');
    systemInfoAvailable = true;
    systemAllowRegister = !!r.allow_register;
    setText('appVersionText', r.version || '0.1.0');
  } catch (e) {
    systemInfoAvailable = false;
  }
}

function copyApiKey() {
  var v = ($('inputApiKey') ? $('inputApiKey').value : '').trim();
  copyToClipboard(v, v ? '已复制 API Key' : '当前没有填写 API Key');
}
function saveApiKey() {
  var v = ($('inputApiKey') ? $('inputApiKey').value : '').trim();
  currentApiKey = v;
  try { localStorage.setItem('trae2api_key', v); } catch (e) { /* 隐私模式 */ }
  var chat = $('chatApiKey');
  if (chat) chat.value = v;
  var fb = $('saveKeyFeedback');
  if (fb) { fb.classList.remove('hidden'); setTimeout(function () { fb.classList.add('hidden'); }, 2000); }
  toast(v ? 'API Key 已保存到浏览器本地，并立即用于所有请求' : '已清空本地 API Key', 'ok');
  refreshData();
}
async function resetMyAPIKey() {
  if (!confirm('确认重置专属 API Key？旧 Key 会立即失效，所有用旧 Key 的客户端都要改配置。')) return;
  try {
    var r = null;
    try {
      r = await post('me/api-key/reset');
    } catch (errMe) {
      // 未登录 /me 会话时（单机控制台运维场景），回退至控制台管理员 Key 重置
      if (errMe && (errMe.status === 401 || errMe.status === 404)) {
        r = await post('admin/users/admin/reset-key');
      } else {
        throw errMe;
      }
    }
    if (r && r.api_key) {
      currentApiKey = r.api_key;
      try { localStorage.setItem('trae2api_key', r.api_key); } catch (e) { }
      if ($('inputApiKey')) $('inputApiKey').value = r.api_key;
      if ($('chatApiKey')) $('chatApiKey').value = r.api_key;
      prompt('新 API Key（仅显示这一次，已自动写入输入框并保存）：', r.api_key);
    }
    toast(r && r.warning ? r.warning : 'API Key 已成功重置并应用', 'ok');
    await refreshData();
  } catch (e) {
    toast('重置失败：' + errText(e), 'err');
  }
}

// ---------------------------------------------------------------- 用量分析

async function loadStats() {
  try {
    var r = await api('v1/stats');
    cachedStats = r.stats || null;
    cachedMetricsSavedAt = r.metrics_last_save || '';
    if (cachedStats) renderTokenAnalytics(cachedStats);
  } catch (e) {
    if (e && e.status === 503) {
      setHtml('tokenModelsTable', '<tr><td colspan="9" class="py-6 text-center text-amber-400 font-sans">指标采集已关闭（config metrics.enabled=false）</td></tr>');
      setHtml('dailyHistoryTable', '<tr><td colspan="8" class="py-6 text-center text-amber-400 font-sans">指标采集已关闭</td></tr>');
    } else if (e && e.status !== 401) {
      toast('读取指标失败：' + errText(e), 'warn');
    }
  }
}
function onMetricsDateChange(val) {
  selectedMetricsDate = val;
  if (cachedStats) renderTokenAnalytics(cachedStats);
}
function selectDateFromHistory(dateStr) {
  selectedMetricsDate = dateStr;
  var sel = $('metricsDateSelector');
  if (sel) sel.value = dateStr;
  if (cachedStats) renderTokenAnalytics(cachedStats);
}

// 我们的 /v1/stats 形状：
//   {global: View, by_model: {id: View}, by_account: {...}, by_realm: {...},
//    by_day: [{day, view: View}], retain_days, no_observation_sentinel}
// View: requests/success/failed/prompt_tokens/completion_tokens/total_tokens/
//       avg_ttfb_ms/avg_latency_ms/cache_hit_rate_percent/avg_credits_x1000/last_seen
function renderTokenAnalytics(stats) {
  if (!stats) return;
  cachedStats = stats;
  var days = (stats.by_day || []).slice().sort(function (a, b) { return a.day < b.day ? 1 : -1; });
  var today = new Date().toISOString().slice(0, 10);
  var g = stats.global || {};

  // 1) 日期选择器
  var sel = $('metricsDateSelector');
  if (sel) {
    var cur = selectedMetricsDate;
    var opt = '<option value="today">今日消耗 (' + today + ')</option><option value="all">历史全量累计</option>';
    for (var i = 0; i < days.length; i++) {
      if (days[i].day !== today) opt += '<option value="' + escapeHtml(days[i].day) + '">' + escapeHtml(days[i].day) + '</option>';
    }
    sel.innerHTML = opt;
    var known = cur === 'today' || cur === 'all' || days.some(function (d) { return d.day === cur; });
    sel.value = known ? cur : 'today';
    if (!known) selectedMetricsDate = 'today';
  }

  // 2) 当前作用域
  var scopeView = g;
  var scopeLabel = '全量';
  var titlePrefix = '历史全量';
  var isDay = false;
  if (selectedMetricsDate === 'today') {
    var t = days.filter(function (d) { return d.day === today; })[0];
    scopeView = t ? t.view : {};
    scopeLabel = '今日';
    titlePrefix = '今日 (' + today + ')';
    isDay = true;
  } else if (selectedMetricsDate === 'all') {
    scopeView = g; scopeLabel = '全量'; titlePrefix = '历史全量'; isDay = false;
  } else {
    var d2 = days.filter(function (d) { return d.day === selectedMetricsDate; })[0];
    scopeView = d2 ? d2.view : {};
    scopeLabel = selectedMetricsDate;
    titlePrefix = selectedMetricsDate;
    isDay = true;
  }

  var promptTok = num(scopeView.prompt_tokens);
  var compTok = num(scopeView.completion_tokens);
  var totalTok = num(scopeView.total_tokens) || (promptTok + compTok);
  var reqs = num(scopeView.requests);
  var succ = num(scopeView.success);

  setText('labelTotalTokens', titlePrefix + ' 消耗 Token');
  setText('badgeDateScope', scopeLabel);
  setText('statTotalTokens', fmtInt(totalTok));
  setText('statCompTokens', fmtInt(compTok));
  setText('statPromptTokens', fmtInt(promptTok));
  setText('statTotalReqs', fmtInt(reqs));
  setText('statSuccessRate', '成功率: ' + (reqs > 0 ? ((succ / reqs) * 100).toFixed(1) : '100.0') + '% (成功 ' + succ + ' / 失败 ' + num(scopeView.failed) + ')');

  var up = $('tokenUptimeText');
  if (up) {
    up.innerText = cachedMetricsSavedAt
      ? ('指标落盘于 ' + truncateTime(cachedMetricsSavedAt) + ' · 保留 ' + num(stats.retain_days) + ' 天')
      : ('保留 ' + num(stats.retain_days) + ' 天');
  }

  // 3) 分模型明细（我们的按天聚合**不含**分模型维度，只有全量 by_model）
  var title = $('currentScopeTitle');
  if (title) {
    title.innerHTML = '<span class="w-1.5 h-1.5 rounded-full bg-indigo-400"></span><span>模型消耗明细 [' + escapeHtml(titlePrefix) + ']</span>';
  }
  var models = Object.keys(stats.by_model || {}).sort(function (a, b) {
    return num((stats.by_model[b] || {}).requests) - num((stats.by_model[a] || {}).requests);
  });
  var rows = '';
  if (isDay) {
    rows += '<tr><td colspan="9" class="py-2 px-3 text-[11px] text-amber-300/80 font-sans">' +
      '注意：本网关按天只聚合总量，分模型维度是全量累计（下表为全量数据）。' + escapeHtml(titlePrefix) + ' 的总量见上方四张卡片。</td></tr>';
  }
  if (!models.length) {
    rows += '<tr><td colspan="9" class="py-6 text-center text-slate-500 font-sans">还没有请求调用数据，调用后自动记录</td></tr>';
  } else {
    for (var k = 0; k < models.length; k++) {
      var id = models[k];
      var v = stats.by_model[id] || {};
      var p = num(v.prompt_tokens), c = num(v.completion_tokens);
      var tt = num(v.total_tokens) || (p + c);
      var lat = num(v.avg_latency_ms);
      var tps = (lat > 0 && c > 0) ? (c / (lat / 1000)) : 0;
      rows += '<tr class="hover:bg-slate-800/40 transition">' +
        '<td class="py-2.5 px-3 font-semibold text-slate-200"><span class="text-indigo-300">' + escapeHtml(id) + '</span></td>' +
        '<td class="py-2.5 px-3 font-mono text-slate-400">' + escapeHtml(multiplierOf(id)) + '</td>' +
        '<td class="py-2.5 px-3 text-slate-300 font-mono">' + fmtInt(v.requests) + '</td>' +
        '<td class="py-2.5 px-3 text-emerald-400 font-mono font-bold">' + fmtInt(tt) + '</td>' +
        '<td class="py-2.5 px-3 text-indigo-300 font-mono">' + fmtInt(p) + '</td>' +
        '<td class="py-2.5 px-3 text-cyan-300 font-mono">' + fmtInt(c) + '</td>' +
        '<td class="py-2.5 px-3 text-indigo-400 font-mono">' + (tps > 0 ? tps.toFixed(1) + ' tok/s' : '<span class="text-slate-500">—</span>') + '</td>' +
        '<td class="py-2.5 px-3 text-slate-400 font-mono">' + fmtNoObs(v.cache_hit_rate_percent, '%', 1) + '</td>' +
        '<td class="py-2.5 px-3 text-right text-slate-300 font-mono">' + fmtNoObs(v.avg_latency_ms, 'ms') + '</td>' +
        '</tr>';
    }
  }
  setHtml('tokenModelsTable', rows);

  // 4) 按日历史
  var dHtml = '';
  if (!days.length) {
    dHtml = '<tr><td colspan="8" class="py-6 text-center text-slate-500 font-sans">暂无每日统计历史数据</td></tr>';
  } else {
    for (var q = 0; q < days.length; q++) {
      var dd = days[q], dv = dd.view || {};
      var dreq = num(dv.requests), dsuc = num(dv.success);
      var dp = num(dv.prompt_tokens), dc = num(dv.completion_tokens);
      var isToday = dd.day === today;
      var hl = (selectedMetricsDate === dd.day || (isToday && selectedMetricsDate === 'today')) ? ' bg-indigo-950/20' : '';
      dHtml += '<tr class="hover:bg-slate-800/40 transition' + hl + '">' +
        '<td class="py-2.5 px-3 font-semibold font-mono text-slate-200"><span>' + escapeHtml(dd.day) + '</span>' +
        (isToday ? '<span class="ml-1.5 text-[10px] px-1.5 py-0.5 rounded bg-emerald-500/20 text-emerald-400 font-normal">今日</span>' : '') + '</td>' +
        '<td class="py-2.5 px-3 text-amber-400 font-mono font-medium">' + fmtInt(dreq) + '</td>' +
        '<td class="py-2.5 px-3 text-slate-400 font-mono">' + (dreq > 0 ? ((dsuc / dreq) * 100).toFixed(1) + '%' : '100.0%') + '</td>' +
        '<td class="py-2.5 px-3 text-emerald-400 font-mono font-bold">' + fmtInt(num(dv.total_tokens) || (dp + dc)) + '</td>' +
        '<td class="py-2.5 px-3 text-indigo-300 font-mono">' + fmtInt(dp) + '</td>' +
        '<td class="py-2.5 px-3 text-cyan-300 font-mono">' + fmtInt(dc) + '</td>' +
        '<td class="py-2.5 px-3 text-slate-400 font-mono">' + fmtNoObs(dv.cache_hit_rate_percent, '%', 1) + '</td>' +
        '<td class="py-2.5 px-3 text-right"><button onclick="selectDateFromHistory(\'' + escapeHtml(dd.day) + '\')" class="text-indigo-400 hover:text-indigo-300 text-xs hover:underline font-mono">查看明细 →</button></td>' +
        '</tr>';
    }
  }
  setHtml('dailyHistoryTable', dHtml);

  renderAccountAggregates(stats);
}
function multiplierOf(modelID) {
  for (var i = 0; i < cachedModels.length; i++) {
    if (cachedModels[i].id === modelID) return cachedModels[i].multiplier || '—';
  }
  return '—';
}
// 参考实现的「实时请求流水」在我们这里没有对应端点（网关不暴露逐请求日志）。
// 用 /v1/stats 的 by_account 聚合派生一张**如实标注**的按账号表，而不是留空。
function renderAccountAggregates(stats) {
  var by = stats.by_account || {};
  var keys = Object.keys(by);
  setText('logCountBadge', keys.length + ' 个账号');
  var tbody = $('requestLogsTable');
  if (!tbody) return;
  if (!keys.length) {
    tbody.innerHTML = '<tr><td colspan="9" class="py-8 text-center text-slate-500 font-sans">还没有账号级调用记录（调用网关接口后在此按账号聚合呈现）</td></tr>';
    return;
  }
  keys.sort(function (a, b) { return num(by[b].requests) - num(by[a].requests); });
  var html = '';
  for (var i = 0; i < keys.length; i++) {
    var uid = keys[i], v = by[uid] || {};
    var c = num(v.completion_tokens), lat = num(v.avg_latency_ms);
    var tps = (lat > 0 && c > 0) ? (c / (lat / 1000)).toFixed(1) + ' tok/s' : '<span class="text-slate-500">—</span>';
    var failed = num(v.failed);
    html += '<tr class="hover:bg-slate-800/40 transition">' +
      '<td class="py-2.5 px-2 text-slate-500">#' + (i + 1) + '</td>' +
      '<td class="py-2.5 px-2 text-slate-400">' + escapeHtml(truncateTime(v.last_seen)) + '</td>' +
      '<td class="py-2.5 px-2 font-medium text-slate-200 truncate max-w-[200px]" title="' + escapeHtml(uid) + '"><span class="text-indigo-300">' + escapeHtml(uid) + '</span></td>' +
      '<td class="py-2.5 px-2 text-slate-300">' + fmtInt(v.requests) + '</td>' +
      '<td class="py-2.5 px-2">' + (failed ? '<span class="text-rose-400 font-bold">失败 ' + failed + '</span>' : '<span class="text-emerald-400 font-bold">全部成功</span>') + '</td>' +
      '<td class="py-2.5 px-2 text-slate-400">' + fmtNoObs(v.avg_ttfb_ms, 'ms') + '</td>' +
      '<td class="py-2.5 px-2 text-slate-300">' + fmtNoObs(v.avg_latency_ms, 'ms') + '</td>' +
      '<td class="py-2.5 px-2 text-cyan-300">' + fmtInt(c) + '</td>' +
      '<td class="py-2.5 px-2 text-right text-indigo-400">' + tps + '</td>' +
      '</tr>';
  }
  tbody.innerHTML = html;
}

// ---------------------------------------------------------------- 演练场

function handleChatFileSelect(event) {
  var f = event.target.files && event.target.files[0];
  if (!f) return;
  processChatAttachmentFile(f);
}
function processChatAttachmentFile(file) {
  if (file.size > 20 * 1024 * 1024) { toast('文件过大（建议 20MB 以内）', 'warn'); return; }
  var isImage = /^image\//.test(file.type);
  var name = file.name || 'attachment';
  var ext = (name.split('.').pop() || 'doc').toLowerCase();
  var reader = new FileReader();
  if (isImage) {
    reader.onload = function (e) {
      currentChatFile = { type: 'image', name: name, data: e.target.result };
      var img = $('chatImagePreview');
      if (img) img.src = e.target.result;
      show('chatImagePreviewWrapper'); hide('chatDocPreviewWrapper'); show('chatFilePreviewArea');
      setText('chatFileName', name);
      setText('chatFileTypeDesc', '已就绪多模态（OpenAI image_url 格式）');
    };
    reader.readAsDataURL(file);
  } else {
    reader.onload = function (e) {
      currentChatFile = { type: 'doc', name: name, ext: ext, text: e.target.result };
      setText('chatDocExt', (ext || 'doc').toUpperCase().slice(0, 4));
      show('chatDocPreviewWrapper'); hide('chatImagePreviewWrapper'); show('chatFilePreviewArea');
      setText('chatFileName', name);
      setText('chatFileTypeDesc', '已就绪文件附件（' + (file.size / 1024).toFixed(1) + ' KB）');
    };
    reader.readAsText(file);
  }
}
function removeChatFile() {
  currentChatFile = null;
  var input = $('chatFileInput');
  if (input) input.value = '';
  var img = $('chatImagePreview');
  if (img) img.src = '';
  hide('chatFilePreviewArea'); hide('chatImagePreviewWrapper'); hide('chatDocPreviewWrapper');
}
function copyModelId(modelId) {
  var sel = $('chatModel');
  if (sel) sel.value = modelId;
  copyToClipboard(modelId, '已复制模型 ID 并填入演练场选择框');
}
async function testModelQuick(modelId) {
  var sel = $('chatModel');
  if (sel) sel.value = modelId;
  var key = ($('chatApiKey') ? $('chatApiKey').value : '') || currentApiKey;
  var t0 = Date.now();
  try {
    var res = await fetch('/v1/chat/completions', {
      method: 'POST',
      headers: Object.assign({ 'Content-Type': 'application/json' }, key ? { 'Authorization': 'Bearer ' + key } : {}),
      body: JSON.stringify({ model: modelId, messages: [{ role: 'user', content: 'hi' }], max_tokens: 5, stream: false })
    });
    var text = await res.text();
    var json = null; try { json = JSON.parse(text); } catch (e) { }
    var dt = ((Date.now() - t0) / 1000).toFixed(2);
    if (res.ok) toast('✅ ' + modelId + ' 连通正常（' + dt + 's）', 'ok');
    else {
      var msg = (json && json.error && (json.error.message || json.error.code)) || res.statusText;
      toast('❌ ' + modelId + ' 测试未通过（HTTP ' + res.status + '，' + dt + 's）\n' + msg, 'err');
    }
  } catch (e) {
    toast('❌ 网络异常：' + errText(e), 'err');
  }
  loadStats();
}

async function sendChatMessage() {
  var model = $('chatModel') ? $('chatModel').value : '';
  var key = ($('chatApiKey') ? $('chatApiKey').value : '') || currentApiKey;
  var prompt = $('chatPrompt') ? $('chatPrompt').value.trim() : '';
  var box = $('chatResponseBox');
  var timing = $('chatTiming');
  var btn = $('btnSendChat');
  if (!model) { toast('请先选择模型', 'warn'); return; }
  if (!prompt && !currentChatFile) { toast('请输入提示词或添加附件', 'warn'); return; }

  var content;
  if (currentChatFile) {
    if (currentChatFile.type === 'image') {
      content = [];
      content.push({ type: 'text', text: prompt || '请分析并描述这张图片。' });
      content.push({ type: 'image_url', image_url: { url: currentChatFile.data } });
    } else {
      var docHeader = '\n\n【文件附件: ' + currentChatFile.name + '】\n```' + (currentChatFile.ext || '') + '\n' + currentChatFile.text + '\n```\n';
      content = prompt ? (prompt + docHeader) : ('请阅读并分析以下文件内容：' + docHeader);
    }
  } else {
    content = prompt;
  }

  btn.disabled = true;
  btn.innerText = '流式生成中...';
  if (box) { box.classList.remove('hidden'); box.innerText = ''; }
  if (timing) { timing.classList.remove('hidden'); timing.innerText = '耗时: 计时中...'; }
  var t0 = Date.now();
  try {
    var res = await fetch('/v1/chat/completions', {
      method: 'POST',
      headers: Object.assign({ 'Content-Type': 'application/json' }, key ? { 'Authorization': 'Bearer ' + key } : {}),
      body: JSON.stringify({ model: model, stream: true, messages: [{ role: 'user', content: content }] })
    });
    if (!res.ok) {
      var t = await res.text();
      var j = null; try { j = JSON.parse(t); } catch (e) { }
      if (box) box.innerText = '请求失败 [' + res.status + ']: ' + ((j && j.error) ? JSON.stringify(j.error) : t.slice(0, 400));
      return;
    }
    var reader = res.body.getReader();
    var dec = new TextDecoder('utf-8');
    var buf = '';
    var full = '';
    var ttfb = null;
    var hasReasoning = false;
    var hasContentStarted = false;
    while (true) {
      var r = await reader.read();
      if (r.done) break;
      if (ttfb === null) ttfb = Date.now() - t0;
      buf += dec.decode(r.value, { stream: true });
      var parts = buf.split('\n');
      buf = parts.pop();
      for (var i = 0; i < parts.length; i++) {
        var line = parts[i].trim();
        if (!line || line.indexOf('data: ') !== 0) continue;
        var payload = line.slice(6).trim();
        if (payload === '[DONE]') continue;
        var chunk = null; try { chunk = JSON.parse(payload); } catch (e) { continue; }
        if (chunk.error) { full += '\n[错误] ' + JSON.stringify(chunk.error); continue; }
        var d = chunk.choices && chunk.choices[0] && chunk.choices[0].delta;
        if (d) {
          if (d.reasoning_content) {
            if (!hasReasoning) {
              hasReasoning = true;
              full += '【💭 思维链思考过程 (Reasoning)】\n';
            }
            full += d.reasoning_content;
          }
          if (d.content) {
            if (hasReasoning && !hasContentStarted) {
              hasContentStarted = true;
              full += '\n\n【💬 回答正文】\n';
            }
            full += d.content;
          }
        }
        if (box) { box.innerText = full; box.scrollTop = box.scrollHeight; }
      }
    }
    if (box && !full) box.innerText = '（已收到完成响应，但内容为空）';
    var elapsedSec = (Date.now() - t0) / 1000;
    var speedText = (elapsedSec > 0 && full.length > 0) ? (' · ' + (full.length / elapsedSec).toFixed(1) + ' 字/s') : '';
    if (timing) timing.innerText = '耗时: ' + elapsedSec.toFixed(2) + 's' + (ttfb !== null ? ' · 首字节 ' + ttfb + 'ms' : '') + speedText;
  } catch (e) {
    if (box) box.innerText += '\n[网络或响应异常] ' + errText(e);
  } finally {
    if (timing && timing.innerText === '耗时: 计时中...') timing.innerText = '耗时: ' + ((Date.now() - t0) / 1000).toFixed(2) + 's';
    btn.disabled = false;
    btn.innerHTML = '<svg class="w-4 h-4" fill="none" stroke="currentColor" viewBox="0 0 24 24"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M14 5l7 7m0 0l-7 7m7-7H3"/></svg><span>发送测试</span>';
    setTimeout(loadStats, 500);
  }
}

// 粘贴截图 / 文件
(function () {
  var el = $('chatPrompt');
  if (!el) return;
  el.addEventListener('paste', function (e) {
    var items = e.clipboardData && e.clipboardData.items;
    if (!items) return;
    for (var i = 0; i < items.length; i++) {
      if (items[i].kind === 'file') {
        var f = items[i].getAsFile();
        if (f) { e.preventDefault(); processChatAttachmentFile(f); toast('已粘贴附件 ' + (f.name || ''), 'ok'); return; }
      }
    }
  });
})();

// ---------------------------------------------------------------- OAuth 授权

var oauthSession = null;
var oauthUrl = '';
var pollTimer = null;
var pollCountdownTimer = null;
var pollRemaining = 180;
var pollAbort = null;

// OAuth 授权弹窗。
//
// ⚠️ 参数是**产品键**（trae-cn / solo-cn / trae-intl / solo-intl），不是 realm。
//
// 为什么改掉原来的 realm 参数：原来传的是 'cn' | 'global'，再由 productOfRealm()
// 映射成 'trae-cn' | 'solo-cn' —— 那是**产品**（Trae vs SOLO）的区分，被错叫成了
// "global"。结果是**国际版根本没有授权入口**，尽管后端早就支持 trae-intl / solo-intl。
// 产品键本来就是要传给后端的东西，直接用它做参数既少一层映射，也不会再叫错。
function oauthProductLabel(key) {
  switch (key) {
    case 'trae-cn': return { text: '授权 Trae CN 账号', cls: 'text-blue-400' };
    case 'solo-cn': return { text: '授权 TRAE SOLO CN 账号', cls: 'text-emerald-400' };
    case 'trae-intl': return { text: '授权 Trae International 账号', cls: 'text-sky-400' };
    case 'solo-intl': return { text: '授权 TRAE SOLO International 账号', cls: 'text-teal-400' };
    default: return { text: '授权账号', cls: 'text-slate-300' };
  }
}

async function openOAuthModal(productKey) {
  if (pollAbort) { try { pollAbort.abort(); } catch (e) { } }
  pollAbort = new AbortController();
  stopPolling();
  oauthSession = null;
  oauthUrl = '';
  var title = $('oauthModalTitle');
  if (title) {
    var lb = oauthProductLabel(productKey);
    title.innerHTML = '<span class="' + lb.cls + '">' + lb.text + '</span>';
  }
  show('oauthModal'); show('oauthStep1'); hide('oauthStep2'); hide('oauthStep3');
  // 重置上一次可能留下的错误状态：重新打开时应当是"正在申请"的样子，
  // 而不是还挂着上一次的失败（那会让人以为这次也失败了）。
  var _eb = $('oauthErrorBox'); if (_eb) _eb.classList.add('hidden');
  var _em = $('oauthErrorMsg'); if (_em) _em.innerText = '';
  var _sp = $('oauthSpinner'); if (_sp) _sp.classList.remove('hidden');
  var _s1 = $('oauthStep1Text'); if (_s1) _s1.innerText = '正在向 Trae 授权服务器申请设备码...';

  try {
    var res = await fetch('/admin/login/start', {
      method: 'POST',
      headers: authHeaders({ 'Content-Type': 'application/json' }),
      credentials: 'same-origin',
      body: JSON.stringify({ product: productKey, open_browser: true }),
      signal: pollAbort.signal
    });
    var data = await res.json().catch(function () { return {}; });
    var modal = $('oauthModal');
    if (!modal || modal.classList.contains('hidden')) return;
    if (!res.ok || !data.url) {
      // **不关闭模态**：这类错误通常很长（例如"登录页按区域封锁，请在代理里加 DIRECT 规则"），
      // 而且要照着做几步操作。原先只 toast 一下就 closeOAuthModal()，9 秒后信息就没了，
      // 用户只知道"失败了"，不知道该怎么办。
      var emsg = (data.error && data.error.message) || data.error || ('HTTP ' + res.status);
      var ebox = $('oauthErrorBox'), emsgEl = $('oauthErrorMsg');
      if (ebox && emsgEl) {
        emsgEl.innerText = String(emsg);
        ebox.classList.remove('hidden');
      }
      var sp = $('oauthSpinner'), st1 = $('oauthStep1Text');
      if (sp) sp.classList.add('hidden');           // 停止转圈（已经失败了，转圈是在骗人）
      if (st1) st1.innerText = '获取授权链接失败';
      toast('获取授权链接失败', 'err');
      return;
    }
    oauthUrl = String(data.url).trim();
    oauthSession = data.session_id;
    var cbInput = $('oauthCallbackUrlInput');
    if (cbInput) cbInput.value = '';
    if (data.preflight_warn) {
      toast('预检提示：' + data.preflight_warn, 'warn');
    }
    var link = $('oauthLinkBtn');
    if (link) link.href = oauthUrl;
    hide('oauthStep1'); show('oauthStep2');
    try { window.open(oauthUrl, '_blank'); } catch (e) { /* 被拦截就让用户点按钮 */ }
    startPolling();
  } catch (e) {
    if (e && e.name === 'AbortError') return;
    toast('网络请求失败：' + errText(e), 'err');
    closeOAuthModal();
  }
}

async function pasteAndSubmitOAuthCallback() {
  try {
    if (navigator.clipboard && navigator.clipboard.readText) {
      var text = await navigator.clipboard.readText();
      if (text && text.trim()) {
        var input = $('oauthCallbackUrlInput');
        if (input) input.value = text.trim();
        toast('已从剪贴板读取网址，正在提交授权...', 'info');
        return submitManualOAuthCallback();
      }
    }
  } catch (e) { /* 剪贴板权限可能被浏览器阻止，降级由用户手动粘贴 */ }
  submitManualOAuthCallback();
}

async function submitManualOAuthCallback() {
  var input = $('oauthCallbackUrlInput');
  var raw = input ? input.value.trim() : '';
  if (!raw) {
    toast('请先粘贴浏览器跳转后地址栏中的网址', 'warn');
    if (input) input.focus();
    return;
  }
  var btn = $('btnSubmitOAuthCallback');
  var oldHTML = btn ? btn.innerHTML : '';
  if (btn) {
    btn.disabled = true;
    btn.innerHTML = '<span class="inline-block animate-spin mr-1">⌛</span> 正在换取令牌...';
  }
  try {
    var res = await fetch('/admin/login/callback', {
      method: 'POST',
      headers: authHeaders({ 'Content-Type': 'application/json' }),
      credentials: 'same-origin',
      body: JSON.stringify({
        session_id: oauthSession || '',
        callback_url: raw
      })
    });
    var data = await res.json().catch(function () { return {}; });
    if (!res.ok || !data.success) {
      var msg = (data.error && data.error.message) || data.error || ('HTTP ' + res.status);
      toast('提交授权回调失败：' + msg, 'err');
      return;
    }
    stopPolling();
    hide('oauthStep2');
    show('oauthStep3');
    setText('oauthSuccessDetail', 'UID: ' + (data.user_id || '—') + ' | 已保存为: ' + (data.saved_as || '成功'));
    toast('账号授权成功并已自动注册进池！', 'ok');
    await refreshData();
    setTimeout(closeOAuthModal, 2500);
  } catch (e) {
    toast('提交失败：' + errText(e), 'err');
  } finally {
    if (btn) {
      btn.disabled = false;
      btn.innerHTML = oldHTML;
    }
  }
}
function copyOAuthUrl() {
  if (!oauthUrl) return;
  copyToClipboard(oauthUrl, '已复制授权链接（建议在无痕窗口打开，避免复用已登录账号）');
}
function startPolling() {
  stopPolling();
  pollRemaining = 180;
  updatePollUI();
  hide('btnPollResume');
  var st = $('pollStatusText');
  if (st) { st.innerText = '后台自动轮询检测中...'; st.className = 'text-emerald-400 font-mono text-[11px]'; }
  pollCountdownTimer = setInterval(function () {
    pollRemaining--;
    updatePollUI();
    if (pollRemaining <= 0) {
      stopPolling();
      if (st) { st.innerText = '轮询等待超时（可点「继续检测」）'; st.className = 'text-amber-400 font-mono text-[11px]'; }
      show('btnPollResume');
    }
  }, 1000);
  pollTimer = setInterval(async function () {
    if (await checkOAuthPoll()) stopPolling();
  }, 2500);
}
function resumePolling() { startPolling(); }
function stopPolling() {
  if (pollTimer) clearInterval(pollTimer);
  if (pollCountdownTimer) clearInterval(pollCountdownTimer);
  pollTimer = null; pollCountdownTimer = null;
}
function updatePollUI() {
  setText('pollCountdown', Math.max(0, pollRemaining) + 's');
  var bar = $('pollProgressBar');
  if (bar) bar.style.width = Math.max(0, (pollRemaining / 180) * 100) + '%';
}
async function checkOAuthPoll() {
  if (!oauthSession) return false;
  try {
    var data = await api('admin/login/status?session_id=' + encodeURIComponent(oauthSession));
    var state = data.state;
    if (state === 'success') {
      hide('oauthStep2'); show('oauthStep3');
      setText('oauthSuccessDetail', 'UID: ' + (data.user_id || '—') +
        ' | 保存为: ' + (data.saved_as || '（保存中）') + (data.saved ? '' : ''));
      await refreshData();
      setTimeout(closeOAuthModal, 2500);
      return true;
    }
    var st = $('pollStatusText');
    if (state === 'error' || state === 'unknown') {
      if (st) { st.innerText = '授权失败：' + (data.error || state); st.className = 'text-rose-400 font-mono text-[11px]'; }
      return true;
    }
    if (st) st.innerText = '等待浏览器中完成授权…（已等待 ' + num(data.elapsed_sec) + 's）';
    if (data.save_error) toast(data.save_error, 'warn');
  } catch (e) { /* 轮询失败不中断 */ }
  return false;
}
async function triggerImmediatePoll() {
  var st = $('pollStatusText');
  if (st) { st.innerText = '正在主动检查授权凭证...'; st.className = 'text-indigo-400 font-mono text-[11px] animate-pulse'; }
  if (!oauthSession) { toast('没有进行中的授权会话', 'warn'); return; }
  try {
    var data = await api('admin/login/status?session_id=' + encodeURIComponent(oauthSession));
    if (data.state === 'success') {
      hide('oauthStep2'); show('oauthStep3');
      setText('oauthSuccessDetail', 'UID: ' + (data.user_id || '—') + ' | 保存为: ' + (data.saved_as || ''));
      await refreshData();
      setTimeout(closeOAuthModal, 2500);
      return;
    }
    toast('暂未检测到授权完成。若刚在浏览器点完确认，请等 2-3 秒再同步。' + (data.error ? ('\n' + data.error) : ''), 'warn');
  } catch (e) { toast('检查失败：' + errText(e), 'err'); }
}
async function closeOAuthModal() {
  if (pollAbort) { try { pollAbort.abort(); } catch (e) { } pollAbort = null; }
  stopPolling();
  if (oauthSession) {
    var sid = oauthSession;
    oauthSession = null;
    try { await post('admin/login/cancel?session_id=' + encodeURIComponent(sid)); } catch (e) { /* 忽略 */ }
  }
  hide('oauthModal');
}

// ---------------------------------------------------------------- 导入

var importTab = 'upload';
var importFiles = [];

function switchImportTab(tab) {
  importTab = tab;
  var on = 'flex-1 py-1.5 rounded-lg bg-indigo-600 text-white font-semibold transition flex items-center justify-center gap-1.5';
  var off = 'flex-1 py-1.5 rounded-lg text-slate-400 hover:text-white transition flex items-center justify-center gap-1.5';
  var tu = $('tabImportUpload'), tp = $('tabImportPaste');
  if (tu) tu.className = tab === 'upload' ? on : off;
  if (tp) tp.className = tab === 'paste' ? on : off;
  if (tab === 'upload') { show('importPanelUpload'); hide('importPanelPaste'); }
  else { hide('importPanelUpload'); show('importPanelPaste'); }
  hide('importError'); hide('importSuccess');
}
function openImportModal() {
  importFiles = [];
  var fi = $('batchFileInput');
  if (fi) fi.value = '';
  var list = $('batchFileList');
  if (list) { list.innerHTML = ''; list.classList.add('hidden'); }
  var ta = $('importJsonText');
  if (ta) ta.value = '';
  hide('importError'); hide('importSuccess');
  switchImportTab('upload');
  show('importModal');
}
function closeImportModal() { hide('importModal'); }
function handleImportDragOver(e) {
  e.preventDefault(); e.stopPropagation();
  var z = $('importDropZone');
  if (z) z.classList.add('border-indigo-500', 'bg-indigo-500/10');
}
function handleImportDragLeave(e) {
  e.preventDefault(); e.stopPropagation();
  var z = $('importDropZone');
  if (z) z.classList.remove('border-indigo-500', 'bg-indigo-500/10');
}
function handleImportDrop(e) {
  e.preventDefault(); e.stopPropagation();
  handleImportDragLeave(e);
  if (e.dataTransfer && e.dataTransfer.files && e.dataTransfer.files.length) handleBatchFiles(Array.prototype.slice.call(e.dataTransfer.files));
}
function handleBatchFileSelect(event) {
  var fs = event.target.files;
  if (fs && fs.length) handleBatchFiles(Array.prototype.slice.call(fs));
}
function handleBatchFiles(files) {
  importFiles = files;
  var list = $('batchFileList');
  if (!list) return;
  var html = '<div class="font-semibold text-slate-400 mb-1">已选择 ' + files.length + ' 个文件:</div>';
  for (var i = 0; i < files.length; i++) {
    var f = files[i];
    var sz = f.size > 1024 * 1024 ? (f.size / 1024 / 1024).toFixed(1) + ' MB' : (f.size / 1024).toFixed(1) + ' KB';
    var zip = /\.zip$/i.test(f.name);
    html += '<div class="flex items-center justify-between text-[11px] py-0.5"><span class="truncate max-w-[320px] ' +
      (zip ? 'text-indigo-400 font-bold' : 'text-slate-200') + '">📄 ' + escapeHtml(f.name) + '</span>' +
      '<span class="text-slate-500">' + sz + '</span></div>';
  }
  list.innerHTML = html;
  list.classList.remove('hidden');
  hide('importError'); hide('importSuccess');
}
async function submitImport() {
  var btn = $('btnSubmitImport');
  var errBox = $('importError');
  var okBox = $('importSuccess');
  hide('importError'); hide('importSuccess');
  var product = ($('importTargetRealm') ? $('importTargetRealm').value : '').trim();
  var orig = btn.innerHTML;
  btn.disabled = true;
  btn.innerHTML = '<span>正在导入...</span>';
  try {
    var r;
    if (importTab === 'upload') {
      if (!importFiles.length) throw new Error('请先选择或拖入 .zip / .json / .txt 文件');
      var fd = new FormData();
      for (var i = 0; i < importFiles.length; i++) fd.append('files', importFiles[i]);
      if (product) fd.append('product', product);
      r = await api('admin/accounts/import', { method: 'POST', body: fd });
    } else {
      var text = ($('importJsonText') ? $('importJsonText').value : '').trim();
      if (!text) throw new Error('请粘贴凭证 JSON / storage.json 原文 / 卡密文本');
      r = await post('admin/accounts/import', { text: text, product: product });
    }
    var msg = '导入完成：成功 ' + num(r.succeed) + ' / 共 ' + num(r.total) + '，当前池内 ' + num(r.accounts_total) + ' 个账号';
    if (num(r.failed)) {
      var bad = (r.results || []).filter(function (x) { return !x.ok; }).slice(0, 5)
        .map(function (x) { return '#' + x.index + ' ' + (x.file || '') + '：' + (x.error || ''); });
      msg += '\n失败 ' + num(r.failed) + ' 个：\n' + bad.join('\n');
    }
    if (okBox) { okBox.innerText = msg; okBox.classList.remove('hidden'); }
    toast(msg, num(r.failed) ? 'warn' : 'ok');
    await refreshData();
    if (r.check_job_id) await pollImportCheck(r.check_job_id);
    setTimeout(function () { if (!$('importModal').classList.contains('hidden') && !num(r.failed)) closeImportModal(); }, 1500);
  } catch (e) {
    if (errBox) { errBox.innerText = errText(e); errBox.classList.remove('hidden'); }
    toast('导入失败：' + errText(e), 'err');
  } finally {
    btn.disabled = false;
    btn.innerHTML = orig;
  }
}
// 导入后端的异步校验任务（续期 + 拉额度），进度回显到 importSuccess
async function pollImportCheck(jobID) {
  var okBox = $('importSuccess');
  for (var i = 0; i < 40; i++) {
    await new Promise(function (r) { setTimeout(r, 1500); });
    var st;
    try { st = await api('admin/accounts/import/status?job_id=' + encodeURIComponent(jobID)); }
    catch (e) { return; }
    if (okBox) {
      okBox.innerText = '后台校验中… ' + num(st.done) + '/' + num(st.total) +
        '（成功 ' + num(st.ok) + '，失败 ' + num(st.failed) + '，暂时性 ' + num(st.transient) + '）';
      okBox.classList.remove('hidden');
    }
    if (st.state === 'done' || st.state === 'finished' || num(st.done) >= num(st.total)) {
      if (okBox) okBox.innerText += '\n后台校验完成。';
      await refreshData();
      return;
    }
  }
}

// ---------------------------------------------------------------- 用户管理

async function loadAdminUsers() {
  var tbody = $('adminUsersTableBody');
  if (tbody) tbody.innerHTML = '<tr><td colspan="8" class="py-6 text-center text-slate-500 font-sans">正在加载用户数据...</td></tr>';
  try {
    var r = await api('admin/users');
    rawAdminUsers = r.data || [];
    if (!systemInfoAvailable) await loadSystemInfo();
    var regText = $('allowRegisterStatusText');
    if (regText) {
      if (!systemInfoAvailable) {
        regText.innerText = '不可用';
        regText.className = 'text-base font-bold text-slate-400';
        regText.title = '后端未实现 GET /admin/system';
      } else {
        regText.innerText = systemAllowRegister ? '已开放' : '已关闭自主注册';
        regText.className = systemAllowRegister ? 'text-base font-bold text-emerald-400' : 'text-base font-bold text-amber-400';
      }
    }
    var total = rawAdminUsers.length;
    var admins = rawAdminUsers.filter(function (u) { return u.role === 'admin'; }).length;
    setText('userStatTotal', total);
    setText('userStatAdmins', admins);
    setText('userStatRegular', total - admins);
    filterAdminUsers();
  } catch (e) {
    if (tbody) {
      tbody.innerHTML = '<tr><td colspan="8" class="py-6 text-center text-rose-400 font-sans">' +
        escapeHtml(e && e.status === 403 ? '需要管理员权限' : ('加载失败：' + errText(e))) + '</td></tr>';
    }
  }
}
function filterAdminUsers() {
  rawAdminUserFilter = ($('userSearchInput') ? $('userSearchInput').value : '').trim().toLowerCase();
  var list = rawAdminUsers;
  if (rawAdminUserFilter) {
    list = rawAdminUsers.filter(function (u) {
      return (u.username || '').toLowerCase().indexOf(rawAdminUserFilter) >= 0 ||
        (u.id || '').toLowerCase().indexOf(rawAdminUserFilter) >= 0 ||
        (u.role || '').toLowerCase().indexOf(rawAdminUserFilter) >= 0 ||
        (u.note || '').toLowerCase().indexOf(rawAdminUserFilter) >= 0;
    });
  }
  renderAdminUsers(list);
}
function ownerAccountCount(u) {
  var n = 0;
  for (var i = 0; i < cachedAccounts.length; i++) {
    var o = cachedAccounts[i].owner || '';
    if (o && (o === u.id || o === u.username)) n++;
  }
  return n;
}
function renderAdminUsers(users) {
  var tbody = $('adminUsersTableBody');
  if (!tbody) return;
  if (!users || !users.length) {
    tbody.innerHTML = '<tr><td colspan="8" class="py-8 text-center text-slate-500 font-sans">暂无匹配的用户记录</td></tr>';
    return;
  }
  var html = '';
  for (var i = 0; i < users.length; i++) {
    var u = users[i];
    var isAdmin = u.role === 'admin';
    var roleBadge = isAdmin
      ? '<span class="px-2 py-0.5 rounded text-[10px] font-bold bg-indigo-500/20 text-indigo-400 border border-indigo-500/30">系统管理员</span>'
      : '<span class="px-2 py-0.5 rounded text-[10px] font-bold bg-slate-800 text-slate-300 border border-slate-700">普通用户</span>';
    var statusBadge = u.disabled
      ? '<span class="px-2 py-0.5 rounded text-[10px] bg-rose-500/20 text-rose-400 border border-rose-500/30">已禁用</span>'
      : '<span class="px-2 py-0.5 rounded text-[10px] bg-emerald-500/20 text-emerald-400 border border-emerald-500/30">正常</span>';
    var created = u.created_at ? truncateTime(u.created_at) : '—';
    var keyCell = u.api_key_masked
      ? '<div class="flex items-center gap-1.5 font-mono text-[11px] text-indigo-300"><span class="truncate max-w-[150px]" title="' +
      escapeHtml(u.api_key_masked) + '">' + escapeHtml(u.api_key_masked) + '</span></div>'
      : '<span class="text-slate-500 text-[11px]">（不可见，重置后可见新值）</span>';
    var acct = isAdmin ? '<span class="text-indigo-400 font-sans text-xs">系统全量</span>'
      : '<span class="font-mono text-xs text-slate-300">' + ownerAccountCount(u) + ' 个</span>';
    var idEsc = escapeHtml(u.id);
    var nameEsc = escapeHtml(u.username);

    var quotaText = (u.quota && u.quota > 0)
      ? '<span class="text-amber-400 font-semibold">' + fmtInt(u.used_quota || 0) + '</span> / <span class="text-slate-300">' + fmtInt(u.quota) + '</span>'
      : '<span class="text-slate-400">无限制 (已用: ' + fmtInt(u.used_quota || 0) + ')</span>';
    var quotaCell = '<div class="flex items-center gap-1.5"><span class="font-mono text-xs">' + quotaText + '</span> ' +
      '<button onclick="adminSetQuota(\'' + idEsc + '\',\'' + nameEsc + '\',' + (u.quota || 0) + ')" class="text-[10px] text-indigo-400 hover:text-indigo-300 hover:underline">设额度</button></div>';

    var wlText = (u.model_whitelist && u.model_whitelist.length)
      ? '<span class="text-emerald-400 font-mono text-xs" title="' + escapeHtml(u.model_whitelist.join(', ')) + '">' + u.model_whitelist.length + ' 个限定</span>'
      : '<span class="text-slate-400 text-xs">全部可用</span>';
    var wlCell = '<div class="flex items-center gap-1.5">' + wlText + ' ' +
      '<button onclick="adminSetWhitelist(\'' + idEsc + '\',\'' + nameEsc + '\')" class="text-[10px] text-cyan-400 hover:text-cyan-300 hover:underline">设置</button></div>';

    var ops = '<button onclick="adminResetPassword(\'' + idEsc + '\',\'' + nameEsc + '\')" class="text-xs text-indigo-400 hover:text-indigo-300 hover:underline">改密</button> ' +
      '<button onclick="adminResetKey(\'' + idEsc + '\',\'' + nameEsc + '\')" class="text-xs text-cyan-400 hover:text-cyan-300 hover:underline">换Key</button> ';
    if (isAdmin) {
      ops += '<button onclick="toggleUserRole(\'' + idEsc + '\',\'user\')" class="text-xs text-slate-400 hover:text-slate-300 hover:underline">降普通</button>';
    } else {
      ops += '<button onclick="toggleUserDisabled(\'' + idEsc + '\',' + (u.disabled ? 'false' : 'true') + ')" class="text-xs ' +
        (u.disabled ? 'text-emerald-400 hover:text-emerald-300' : 'text-amber-400 hover:text-amber-300') + ' hover:underline">' +
        (u.disabled ? '启用' : '禁用') + '</button> ' +
        '<button onclick="toggleUserRole(\'' + idEsc + '\',\'admin\')" class="text-xs text-purple-400 hover:text-purple-300 hover:underline">升管理</button> ' +
        '<button onclick="deleteUser(\'' + idEsc + '\',\'' + nameEsc + '\')" class="text-xs text-rose-400 hover:text-rose-300 hover:underline">删除</button>';
    }
    html += '<tr class="hover:bg-slate-800/40 transition">' +
      '<td class="px-4 py-3 text-slate-400 font-mono text-xs"><span class="cursor-pointer hover:text-white" onclick="copyToClipboard(\'' + idEsc + '\',\'已复制用户 ID\')" title="点击复制">' + idEsc + '</span></td>' +
      '<td class="px-4 py-3 font-semibold text-white font-sans text-xs">' + nameEsc +
      (u.note ? '<div class="text-[10px] text-slate-500 font-normal">' + escapeHtml(u.note) + '</div>' : '') + '</td>' +
      '<td class="px-4 py-3">' + roleBadge + '</td>' +
      '<td class="px-4 py-3">' + keyCell + '</td>' +
      '<td class="px-4 py-3">' + quotaCell + '</td>' +
      '<td class="px-4 py-3">' + wlCell + '</td>' +
      '<td class="px-4 py-3">' + acct + '</td>' +
      '<td class="px-4 py-3">' + statusBadge + '</td>' +
      '<td class="px-4 py-3 text-slate-400 text-[11px] font-sans">' + escapeHtml(created) + '</td>' +
      '<td class="px-4 py-3 text-right space-x-1.5 font-sans">' + ops + '</td>' +
      '</tr>';
  }
  tbody.innerHTML = html;
}
async function adminSetQuota(id, username, currentQuota) {
  var val = prompt('设置用户 "' + username + '" 的 Token 总配额（填数字，0 或 -1 表示无限制）：', currentQuota > 0 ? currentQuota : '1000000');
  if (val === null) return;
  var q = parseInt(val.trim(), 10);
  if (isNaN(q)) { toast('配额必须为有效数字', 'warn'); return; }
  try {
    await post('admin/users/' + encodeURIComponent(id) + '/quota', { quota: q });
    toast('已成功更新用户 "' + username + '" 的配额', 'ok');
    loadAdminUsers();
  } catch (e) {
    toast('更新配额失败：' + errText(e), 'err');
  }
}
async function adminSetWhitelist(id, username) {
  var u = rawAdminUsers.find(function (item) { return item.id === id; });
  var cur = (u && u.model_whitelist) ? u.model_whitelist.join(', ') : '';
  var val = prompt('设置用户 "' + username + '" 允许调用的模型列表（以逗号分隔，留空表示允许全部）：\n如: trae:glm-5.3-flash, trae:Doubao-Seed-2.1-Pro', cur);
  if (val === null) return;
  var arr = val.split(',').map(function (s) { return s.trim(); }).filter(Boolean);
  try {
    await post('admin/users/' + encodeURIComponent(id) + '/whitelist', { models: arr });
    toast('已成功更新用户 "' + username + '" 的模型白名单', 'ok');
    loadAdminUsers();
  } catch (e) {
    toast('更新白名单失败：' + errText(e), 'err');
  }
}
async function adminResetPassword(id, username) {
  var pw = prompt('请输入用户 "' + username + '" 的新登录密码（至少 6 位）：');
  if (pw === null) return;
  if (pw.trim().length < 6) { toast('新密码至少 6 位', 'warn'); return; }
  try {
    await post('admin/users/' + encodeURIComponent(id) + '/reset-password', { password: pw.trim() });
    toast('用户 "' + username + '" 的登录密码已重置', 'ok');
    loadAdminUsers();
  } catch (e) {
    toast(isMissingEndpoint(e) ? '重置密码端点不可用（后端未实现）' : ('重置密码失败：' + errText(e)), 'err');
  }
}
async function adminResetKey(id, username) {
  if (!confirm('确认重置用户 "' + username + '" 的专属 API Key？该用户旧 Key 会立即失效。')) return;
  try {
    var r = await post('admin/users/' + encodeURIComponent(id) + '/reset-key');
    if (r && r.api_key) prompt('用户 "' + username + '" 的新 API Key（仅显示这一次）：', r.api_key);
    toast('已重置 ' + username + ' 的 API Key', 'ok');
    loadAdminUsers();
  } catch (e) { toast('重置 Key 失败：' + errText(e), 'err'); }
}
async function toggleUserDisabled(id, disabled) {
  if (!confirm('确认' + (disabled ? '禁用' : '启用') + '该用户？')) return;
  try {
    await post('admin/users/' + encodeURIComponent(id) + '/' + (disabled ? 'disable' : 'enable'));
    toast('已' + (disabled ? '禁用' : '启用'), 'ok');
    loadAdminUsers();
  } catch (e) { toast('操作失败：' + errText(e), 'err'); }
}
async function toggleUserRole(id, role) {
  if (!confirm('确认把该用户角色改为 "' + (role === 'admin' ? '系统管理员' : '普通用户') + '"？')) return;
  try {
    await post('admin/users/' + encodeURIComponent(id) + '/role', { role: role });
    toast('角色已更新', 'ok');
    loadAdminUsers();
  } catch (e) { toast('改角色失败：' + errText(e), 'err'); }
}
async function deleteUser(id, username) {
  if (!confirm('确认永久删除用户 "' + username + '"？其 API Key 会立即失效，操作不可逆。')) return;
  try {
    await api('admin/users/' + encodeURIComponent(id), { method: 'DELETE' });
    toast('已删除用户 ' + username, 'ok');
    loadAdminUsers();
  } catch (e) { toast('删除失败：' + errText(e), 'err'); }
}
function toggleCreateUserForm() {
  var form = $('adminCreateUserForm');
  if (!form) return;
  if (form.classList.contains('hidden')) {
    form.classList.remove('hidden');
    if ($('newAdminUname')) $('newAdminUname').value = '';
    if ($('newAdminPass')) $('newAdminPass').value = '';
    if ($('newAdminRole')) $('newAdminRole').value = 'user';
    hide('createUserErrorMsg');
    setTimeout(function () { if ($('newAdminUname')) $('newAdminUname').focus(); }, 50);
  } else {
    form.classList.add('hidden');
  }
}
async function submitAdminCreateUser() {
  var uname = ($('newAdminUname') ? $('newAdminUname').value : '').trim();
  var pass = $('newAdminPass') ? $('newAdminPass').value : '';
  var role = $('newAdminRole') ? $('newAdminRole').value : 'user';
  var errEl = $('createUserErrorMsg');
  var btn = $('btnSubmitCreateUser');
  if (!uname) { if (errEl) { errEl.innerText = '请填写用户名'; errEl.classList.remove('hidden'); } return; }
  if (pass && pass.length < 6) { if (errEl) { errEl.innerText = '密码长度至少 6 位'; errEl.classList.remove('hidden'); } return; }
  btn.disabled = true; btn.innerText = '正在创建...';
  hide('createUserErrorMsg');
  try {
    var r = await post('admin/users', { username: uname, role: role, note: '' });
    if (pass && r && r.id) {
      // 本项目的 POST /admin/users 不带密码；按契约用 reset-password 补设
      try { await post('admin/users/' + encodeURIComponent(r.id) + '/reset-password', { password: pass }); }
      catch (e) { toast('用户已创建，但初始密码设置失败：' + errText(e), 'warn'); }
    }
    if (r && r.api_key) prompt('用户 "' + uname + '" 已创建。专属 API Key（仅显示这一次）：', r.api_key);
    toast('用户 ' + uname + ' 创建成功（' + role + '）', 'ok');
    toggleCreateUserForm();
    loadAdminUsers();
  } catch (e) {
    if (errEl) { errEl.innerText = errText(e); errEl.classList.remove('hidden'); }
  } finally {
    btn.disabled = false; btn.innerText = '确认创建';
  }
}
async function toggleAllowRegister() {
  if (!systemInfoAvailable) { toast('后端未实现 GET /admin/system，无法切换开放注册', 'err'); return; }
  var want = !systemAllowRegister;
  try {
    var r = await post('admin/system/allow_register', { enabled: want });
    systemAllowRegister = !!r.allow_register;
    toast('开放注册已' + (systemAllowRegister ? '开启' : '关闭'), 'ok');
    loadAdminUsers();
  } catch (e) { toast('切换失败：' + errText(e), 'err'); }
}

// ---------------------------------------------------------------- 登录 / 注册

function switchAuthTab(tab) {
  var bl = $('tabAuthLogin'), br = $('tabAuthRegister');
  var fl = $('formLogin'), fr = $('formRegister');
  var sub = $('loginOverlaySub');
  if (tab === 'login') {
    if (bl) bl.className = 'flex-1 py-1.5 rounded-lg bg-indigo-600 text-white font-semibold transition';
    if (br) br.className = 'flex-1 py-1.5 rounded-lg text-slate-400 hover:text-white transition';
    if (fl) fl.classList.remove('hidden');
    if (fr) fr.classList.add('hidden');
    if (sub) sub.innerText = 'Trae2Api · 控制台（/console）';
    setTimeout(function () { if ($('loginUsernameInput')) $('loginUsernameInput').focus(); }, 50);
  } else {
    if (br) br.className = 'flex-1 py-1.5 rounded-lg bg-emerald-600 text-white font-semibold transition';
    if (bl) bl.className = 'flex-1 py-1.5 rounded-lg text-slate-400 hover:text-white transition';
    if (fr) fr.classList.remove('hidden');
    if (fl) fl.classList.add('hidden');
    if (sub) sub.innerText = '注册普通用户 · 获专属独立 API Key';
    setTimeout(function () { if ($('regUsernameInput')) $('regUsernameInput').focus(); }, 50);
  }
}
function showLoginModal(showIt, enforced) {
  if (typeof enforced === 'boolean') loginEnforced = enforced;
  var overlay = $('loginOverlay');
  if (!overlay) return;
  if (showIt) {
    overlay.classList.remove('hidden');
    switchAuthTab('login');
    var closeBtn = $('loginCloseBtn');
    if (closeBtn) { if (loginEnforced) closeBtn.classList.add('hidden'); else closeBtn.classList.remove('hidden'); }
  } else {
    if (loginEnforced && !currentUser) { window.location.href = '/'; return; }
    overlay.classList.add('hidden');
  }
}
async function doLogin(e) {
  if (e) e.preventDefault();
  var uname = ($('loginUsernameInput') ? $('loginUsernameInput').value : '').trim();
  var pw = $('loginPasswordInput') ? $('loginPasswordInput').value : '';
  var errEl = $('loginErrorMsg');
  var btn = $('loginBtn');
  hide('loginErrorMsg');
  if (!pw) { if (errEl) { errEl.innerText = '请输入密码'; errEl.classList.remove('hidden'); } return; }
  btn.disabled = true; btn.innerText = '正在验证...';
  try {
    var r = await post('auth/login', { username: uname, password: pw });
    currentUser = { id: r.id, username: r.username, role: r.role };
    if (r.api_key) {
      currentApiKey = r.api_key;
      try { localStorage.setItem('trae2api_key', r.api_key); } catch (e2) { }
      if ($('inputApiKey')) $('inputApiKey').value = r.api_key;
      if ($('chatApiKey')) $('chatApiKey').value = r.api_key;
    }
    loginEnforced = false;
    showLoginModal(false);
    toast('登录成功：' + r.username + '（' + r.role + '）', 'ok');
    applyCurrentUser();
    await refreshData();
  } catch (e) {
    if (isMissingEndpoint(e)) {
      // 后端还没实现 /auth/login 时，回落到控制台密码闸（POST /login，form 表单）
      try {
        var body = new URLSearchParams();
        body.set('password', pw);
        var res = await fetch('/login', {
          method: 'POST', credentials: 'same-origin',
          headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
          body: body.toString(), redirect: 'manual'
        });
        if (res.status === 0 || res.ok || res.type === 'opaqueredirect' || res.status === 302 || res.status === 200) {
          window.location.reload();
          return;
        }
        throw new Error('HTTP ' + res.status);
      } catch (e2) {
        if (errEl) { errEl.innerText = '登录失败：' + errText(e2); errEl.classList.remove('hidden'); }
      }
    } else if (errEl) {
      errEl.innerText = errText(e);
      errEl.classList.remove('hidden');
    }
  } finally {
    btn.disabled = false; btn.innerText = '登 录 进 入';
  }
}
async function doRegister(e) {
  if (e) e.preventDefault();
  var uname = ($('regUsernameInput') ? $('regUsernameInput').value : '').trim();
  var pw = $('regPasswordInput') ? $('regPasswordInput').value : '';
  var pw2 = $('regPasswordConfirmInput') ? $('regPasswordConfirmInput').value : '';
  var errEl = $('regErrorMsg');
  var btn = $('regBtn');
  hide('regErrorMsg');
  if (!uname || !pw) { if (errEl) { errEl.innerText = '请填写用户名和密码'; errEl.classList.remove('hidden'); } return; }
  if (pw !== pw2) { if (errEl) { errEl.innerText = '两次输入的密码不一致'; errEl.classList.remove('hidden'); } return; }
  btn.disabled = true; btn.innerText = '正在注册...';
  try {
    var r = await post('auth/register', { username: uname, password: pw });
    currentUser = { id: r.id, username: r.username, role: r.role };
    if (r.api_key) {
      currentApiKey = r.api_key;
      try { localStorage.setItem('trae2api_key', r.api_key); } catch (e2) { }
      if ($('inputApiKey')) $('inputApiKey').value = r.api_key;
      if ($('chatApiKey')) $('chatApiKey').value = r.api_key;
    }
    loginEnforced = false;
    showLoginModal(false);
    toast('注册成功：' + r.username + '（' + r.role + '）', 'ok');
    applyCurrentUser();
    await refreshData();
  } catch (e) {
    // 后端给的是机器可读 code（见 auth.go:registerErrCode），按 code 给**能照着做**的提示，
    // 而不是把服务端的原始错误直接甩给用户。
    var code = (e && e.body && e.body.error && e.body.error.code) || '';
    var hint = '';
    if (code === 'first_user_must_be_admin') {
      // 全新部署时这条必然触发：自助注册**故意**不给"第一个注册的人"管理员
      // （否则等于把管理权交给第一个访问者），所以注册弹窗此时必然失败。
      hint = '本机还没有任何用户：第一个账号必须在「用户管理」标签里创建（本机免密时该操作即管理员），' +
        '之后才能使用自助注册。';
    } else if (code === 'username_exists') {
      hint = '这个用户名已经有人用了，换一个。';
    } else if (code === 'register_disabled') {
      hint = '管理员关闭了自助注册。可以在「用户管理」标签里打开「开放注册」开关。';
    }
    if (errEl) { errEl.innerText = hint || errText(e); errEl.classList.remove('hidden'); }
  } finally {
    btn.disabled = false; btn.innerText = '立即注册并登录';
  }
}
async function doLogout() {
  if (!confirm('确定退出当前控制台账号？')) return;
  try { await post('auth/logout'); } catch (e) { /* 忽略 */ }
  try { await post('logout'); } catch (e) { /* 控制台密码闸 */ }
  currentUser = null;
  currentApiKey = '';
  try { localStorage.removeItem('trae2api_key'); } catch (e) { }
  window.location.reload();
}
function applyCurrentUser() {
  var badge = $('navUserBadge');
  var usersTab = $('mainTabBtn-users');
  if (currentUser) {
    if (badge) badge.classList.remove('hidden');
    setText('navUsernameText', currentUser.username);
    var roleTag = $('navUserRoleTag');
    if (roleTag) {
      if (currentUser.role === 'admin') {
        roleTag.innerText = '管理员';
        roleTag.className = 'text-[10px] px-1.5 py-0.2 rounded bg-indigo-500/20 text-indigo-300 border border-indigo-500/30';
      } else {
        roleTag.innerText = '普通用户';
        roleTag.className = 'text-[10px] px-1.5 py-0.2 rounded bg-emerald-500/20 text-emerald-300 border border-emerald-500/30';
      }
    }
    // 普通用户没有管理面：隐藏用户管理标签
    if (usersTab) { if (currentUser.role === 'admin') usersTab.classList.remove('hidden'); else usersTab.classList.add('hidden'); }
  } else {
    if (badge) badge.classList.add('hidden');
    // 未登录（单机免密 / 仅 API Key）时按管理员对待：否则用户管理页永远打不开
    if (usersTab) usersTab.classList.remove('hidden');
  }
}

// ---------------------------------------------------------------- 改密

function openChangePasswordModal() {
  hide('changePasswordError'); hide('changePasswordSuccess');
  if ($('inputOldPassword')) $('inputOldPassword').value = '';
  if ($('inputNewPassword')) $('inputNewPassword').value = '';
  
  var targetSel = $('passwordTarget');
  if (targetSel) {
    // 若当前未以特定多租户账号登录，默认锁定为 console（控制台管理密码）
    if (!currentUser) {
      targetSel.value = 'console';
    }
    targetSel.onchange = onPasswordTargetChange;
  }
  onPasswordTargetChange();
  show('changePasswordModal');
  setTimeout(function () {
    var el = (hasWebPassword && (!targetSel || targetSel.value === 'console')) ? $('inputOldPassword') : $('inputNewPassword');
    if (el) el.focus();
  }, 100);
}
function onPasswordTargetChange() {
  var target = $('passwordTarget') ? $('passwordTarget').value : 'console';
  if (target === 'me') {
    // 修改个人密码：原密码必填
    show('oldPasswordGroup');
    setText('labelOldPassword', '原登录密码');
    setText('labelNewPassword', '新登录密码');
  } else {
    // 控制台密码：若当前有密码则需要原密码，若免密则不需要
    if (hasWebPassword) {
      show('oldPasswordGroup');
      setText('labelOldPassword', '原管理密码');
    } else {
      hide('oldPasswordGroup');
    }
    setText('labelNewPassword', '新管理密码');
  }
}
function closeChangePasswordModal() { hide('changePasswordModal'); }
async function submitChangePassword() {
  var target = $('passwordTarget') ? $('passwordTarget').value : 'console';
  var oldPw = $('inputOldPassword') ? $('inputOldPassword').value : '';
  var newPw = ($('inputNewPassword') ? $('inputNewPassword').value : '').trim();
  var btn = $('btnSubmitPassword');
  var errEl = $('changePasswordError');
  var okEl = $('changePasswordSuccess');
  hide('changePasswordError'); hide('changePasswordSuccess');

  if (target === 'me' && !oldPw) {
    if (errEl) { errEl.innerText = '请输入原登录密码'; errEl.classList.remove('hidden'); }
    return;
  }
  if (newPw !== '' && newPw.length < 6) {
    if (errEl) { errEl.innerText = '新密码至少 6 位（清空控制台密码恢复免密请直接留空）'; errEl.classList.remove('hidden'); }
    return;
  }
  btn.disabled = true; btn.innerText = '保存中...';
  try {
    if (target === 'me') {
      await post('me/password', { old_password: oldPw, new_password: newPw });
      if (okEl) { okEl.innerText = '✅ 你的登录密码已更新（旧会话已注销，请使用新密码重新登录）'; okEl.classList.remove('hidden'); }
      toast('登录密码已更新', 'ok');
      setTimeout(function () { closeChangePasswordModal(); showLoginModal(true); }, 1500);
    } else {
      await post('admin/password', { old_password: oldPw, new_password: newPw });
      hasWebPassword = newPw !== '';
      updatePasswordBadge(hasWebPassword);
      if (okEl) {
        okEl.innerText = hasWebPassword ? '✅ 控制台管理密码已设置（旧会话已作废）' : '✅ 已清除管理密码，恢复单机免密';
        okEl.classList.remove('hidden');
      }
      toast('控制台密码已更新', 'ok');
      setTimeout(function () {
        closeChangePasswordModal();
        if (hasWebPassword) {
          // 跳转或弹出登录
          location.reload();
        } else {
          refreshData();
        }
      }, 1200);
    }
  } catch (e) {
    if (errEl) {
      errEl.innerText = isMissingEndpoint(e)
        ? ('该端点不可用（后端未实现）：' + (target === 'me' ? 'POST /me/password' : 'POST /admin/password'))
        : errText(e);
      errEl.classList.remove('hidden');
    }
  } finally {
    btn.disabled = false; btn.innerText = '保存密码';
  }
}

// ---------------------------------------------------------------- Agent (dsh)

async function fetchDshStatus(manual) {
  try {
    var s = await api('admin/dsh/status');
    dshState = s;
    renderDsh(s);
    if (manual) toast('dsh 状态已刷新', 'ok');
  } catch (e) {
    if (e && e.status !== 401) {
      setHtml('dshLogBox', '<div class="text-rose-400 font-sans">读取 dsh 状态失败：' + escapeHtml(errText(e)) + '</div>');
    }
  }
}
function dshIframeURL(s) {
  if (!s || !s.url) return '';
  var hosts = s.trusted_hosts || [];
  var here = location.hostname;
  // dsh 的登录 cookie 是 SameSite=Strict：用 localhost 打开控制台时，
  // iframe 也必须用 localhost，否则 cookie 不带、内嵌页会一直停在未登录。
  if (s.port && hosts.indexOf(here) >= 0) {
    return location.protocol + '//' + here + ':' + s.port + '/?token=' + encodeURIComponent(s.token || '');
  }
  return s.url;
}
function renderDsh(s) {
  s = s || {};
  var running = !!s.running;
  var node = s.node || {};

  var navDot = $('dshNavStatusDot');
  if (navDot) navDot.className = running ? 'w-1.5 h-1.5 rounded-full bg-emerald-400 animate-pulse' : 'w-1.5 h-1.5 rounded-full bg-slate-500';

  var badge = $('dshStatusBadge');
  if (badge) {
    if (running) {
      badge.className = 'px-2.5 py-0.5 rounded-full text-[11px] font-semibold bg-emerald-500/10 text-emerald-400 border border-emerald-500/20';
      badge.innerText = '运行中 · ' + (s.state || '');
    } else if (s.state === 'node_missing') {
      badge.className = 'px-2.5 py-0.5 rounded-full text-[11px] font-semibold bg-rose-500/10 text-rose-400 border border-rose-500/20';
      badge.innerText = '未就绪（缺 Node）';
    } else if (s.state === 'failed') {
      badge.className = 'px-2.5 py-0.5 rounded-full text-[11px] font-semibold bg-rose-500/10 text-rose-400 border border-rose-500/20';
      badge.innerText = '启动失败';
    } else {
      badge.className = 'px-2.5 py-0.5 rounded-full text-[11px] font-semibold bg-slate-800 text-slate-400 border border-slate-700';
      badge.innerText = '已就绪未运行（' + (s.state || 'stopped') + '）';
    }
  }

  var env = $('dshEnvInfo');
  if (env) {
    if (node.found) { env.innerText = (node.version || 'node') + ' 就绪'; env.className = 'font-bold text-emerald-400'; }
    else { env.innerText = '未找到 Node'; env.className = 'font-bold text-rose-400'; }
    env.title = node.path || '';
  }
  setText('dshGatewayURL', location.origin + '/v1');
  setText('dshPortText', running && s.port ? String(s.port) : '—');

  var btnStart = $('btnDshStart'), btnStop = $('btnDshStop'), btnExt = $('btnDshOpenWindow');
  if (running) {
    if (btnStart) btnStart.classList.add('hidden');
    if (btnStop) btnStop.classList.remove('hidden');
    if (btnExt) btnExt.classList.remove('hidden');
    show('dshIframeContainer'); hide('dshGuideContainer');
    dshCurrentUrl = dshIframeURL(s);
    setText('dshIframeUrlText', dshCurrentUrl);
    if (dshCurrentUrl && dshCurrentUrl !== dshIframeSrc) {
      dshIframeSrc = dshCurrentUrl;
      var f = $('dshIframe');
      if (f) f.src = dshCurrentUrl;
    }
  } else {
    if (btnStart) {
      btnStart.classList.remove('hidden');
      btnStart.disabled = false;
      if (!node.found) {
        btnStart.className = 'px-4 py-2 bg-gradient-to-r from-emerald-600 to-teal-600 hover:from-emerald-500 hover:to-teal-500 text-white text-xs font-semibold rounded-xl shadow-lg shadow-emerald-600/20 transition flex items-center gap-1.5';
        btnStart.innerHTML = '<svg class="w-4 h-4" fill="none" stroke="currentColor" viewBox="0 0 24 24"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M4 16v1a3 3 0 003 3h10a3 3 0 003-3v-1m-4-4l-4 4m0 0l-4-4m4 4V4"/></svg><span>检测安装环境（只检测，不下载）</span>';
        btnStart.onclick = installDshEnv;
      } else {
        btnStart.className = 'px-4 py-2 bg-gradient-to-r from-cyan-600 to-blue-600 hover:from-cyan-500 hover:to-blue-500 text-white text-xs font-semibold rounded-xl shadow-lg shadow-cyan-600/20 transition flex items-center gap-1.5';
        btnStart.innerHTML = '<svg class="w-4 h-4" fill="none" stroke="currentColor" viewBox="0 0 24 24"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M14.752 11.168l-3.197-2.132A1 1 0 0010 9.87v4.263a1 1 0 001.555.832l3.197-2.132a1 1 0 000-1.664z"/><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M21 12a9 9 0 11-18 0 9 9 0 0118 0z"/></svg><span>一键启动 dsh</span>';
        btnStart.onclick = startDsh;
      }
    }
    if (btnStop) btnStop.classList.add('hidden');
    if (btnExt) btnExt.classList.add('hidden');
    hide('dshIframeContainer'); show('dshGuideContainer');
    dshIframeSrc = '';
  }

  var logs = s.log_tail || [];
  setText('dshLogCount', logs.length + ' 条');
  var logBox = $('dshLogBox');
  if (logBox) {
    if (logs.length) {
      logBox.innerHTML = logs.map(function (l) { return '<div class="hover:text-cyan-300 transition">' + escapeHtml(l) + '</div>'; }).join('');
      logBox.scrollTop = logBox.scrollHeight;
    } else {
      logBox.innerHTML = '<div class="text-slate-500 font-sans">暂无 dsh 运行日志</div>';
    }
  }

  var codeEl = $('dshEnvCode');
  if (codeEl) {
    codeEl.innerText = '# 快捷终端执行（跨平台单行命令）\n' +
      'OPENAI_BASE_URL="' + location.origin + '/v1" OPENAI_API_KEY="' + (currentApiKey || '<你的 API Key>') + '" \\\n' +
      '  npx @deepseek-ai/dsh web --port ' + (running && s.port ? s.port : 3080);
  }
  if (s.hint) {
    var guide = $('dshGuideContainer');
    if (guide && !running) {
      // hint 已经由状态徽标体现，这里只把 last_error 明确展示
    }
  }
  if (s.last_error) toast('dsh 上次错误：' + s.last_error, 'warn');
}
async function installDshEnv() {
  var btn = $('btnDshStart');
  if (btn) { btn.disabled = true; btn.innerHTML = '<span>正在检测环境...</span>'; }
  try {
    var r = await post('admin/dsh/install');
    var lines = [];
    lines.push('Node：' + (r.node && r.node.found ? ((r.node.version || '') + ' ' + (r.node.path || '')) : '未找到'));
    if (r.desktop) lines.push('dsh 桌面端：' + (r.desktop.found ? (r.desktop.path || '已安装') : '未找到'));
    lines.push('状态：' + (r.state || ''));
    if (r.hint) lines.push('提示：' + r.hint);
    prompt('环境检测结果（本项目只检测、不下载 Node）：', lines.join('\n'));
    await fetchDshStatus(false);
  } catch (e) {
    toast(isMissingEndpoint(e) ? '检测端点不可用（后端未实现 /admin/dsh/install）' : ('检测失败：' + errText(e)), 'err');
  } finally {
    if (btn) btn.disabled = false;
  }
}
async function startDsh() {
  var btn = $('btnDshStart');
  if (btn) { btn.disabled = true; btn.innerHTML = '<span>正在启动（冷启动可能十几秒）...</span>'; }
  try {
    var r = await post('admin/dsh/start');
    var st = r.status || r;
    renderDsh(st);
    toast(st.running ? ('dsh 已启动，端口 ' + st.port) : 'dsh 未启动', st.running ? 'ok' : 'err');
    setTimeout(function () { fetchDshStatus(false); }, 1500);
    setTimeout(function () { fetchDshStatus(false); }, 4000);
  } catch (e) {
    toast('启动失败：' + errText(e), 'err');
    fetchDshStatus(false);
  } finally {
    if (btn) btn.disabled = false;
  }
}
async function stopDsh() {
  if (!confirm('确定停止 dsh 服务？正在 dsh 里进行的操作会中断。')) return;
  try {
    var r = await post('admin/dsh/stop');
    renderDsh(r.status || r);
    toast('dsh 已停止', 'ok');
    fetchDshStatus(false);
  } catch (e) { toast('停止失败：' + errText(e), 'err'); }
}
async function resetDshEnv() {
  if (!confirm('确定重置 dsh 运行环境？\n会停止进程并清空日志与运行状态（不删你的账号数据）。')) return;
  try {
    var r = await post('admin/dsh/reset');
    var cleared = r.cleared || {};
    toast('已重置：状态 ' + (r.state || 'stopped') + '，日志 ' + (cleared.log ? '已清' : '未清') + '，运行态 ' + (cleared.run ? '已清' : '未清'), 'ok');
    dshIframeSrc = '';
    await fetchDshStatus(false);
  } catch (e) {
    toast(isMissingEndpoint(e) ? '重置端点不可用（后端未实现 /admin/dsh/reset）' : ('重置失败：' + errText(e)), 'err');
  }
}
function openDshExternal() {
  if (!dshCurrentUrl) { toast('dsh 未运行', 'warn'); return; }
  window.open(dshCurrentUrl, '_blank', 'noopener');
}
function reloadDshIframe() {
  var f = $('dshIframe');
  if (f && dshCurrentUrl) f.src = dshCurrentUrl;
}
function copyDshCmd() {
  var el = $('dshEnvCode');
  if (el) copyToClipboard(el.innerText, '已复制 dsh 启动命令');
}
async function exportAccountsBackup() {
  toast('正在打包全部账号凭证为 ZIP 备份包...', 'ok');
  try {
    var res = await fetch('/admin/accounts/export', {
      headers: authHeaders(),
      credentials: 'same-origin'
    });
    if (!res.ok) {
      var t = await res.text();
      throw new Error(t ? t.slice(0, 300) : ('HTTP ' + res.status));
    }
    var blob = await res.blob();
    var url = URL.createObjectURL(blob);
    var a = document.createElement('a');
    a.href = url;
    a.download = 'trae2api-accounts-backup.zip';
    document.body.appendChild(a);
    a.click();
    document.body.removeChild(a);
    setTimeout(function () { URL.revokeObjectURL(url); }, 5000);
    toast('已成功下载账号凭证 ZIP 备份包！', 'ok');
  } catch (e) {
    toast('导出备份失败：' + errText(e), 'err');
  }
}

async function exportDshEnv() {
  try {
    var res = await fetch('/admin/dsh/export', { headers: authHeaders(), credentials: 'same-origin' });
    if (!res.ok) {
      var t = await res.text();
      throw new Error(t ? t.slice(0, 300) : ('HTTP ' + res.status));
    }
    var blob = await res.blob();
    var url = URL.createObjectURL(blob);
    var a = document.createElement('a');
    a.href = url;
    a.download = 'trae2api-dsh-export.txt';
    document.body.appendChild(a); a.click(); document.body.removeChild(a);
    setTimeout(function () { URL.revokeObjectURL(url); }, 5000);
    toast('已导出 dsh 状态与日志（附件）', 'ok');
  } catch (e) {
    toast(isMissingEndpoint(e) ? '导出端点不可用（后端未实现 /admin/dsh/export）' : ('导出失败：' + errText(e)), 'err');
  }
}

// ---------------------------------------------------------------- 系统更新

function closeUpdateModal() { hide('updateModal'); }
async function checkAppUpdate(isManual) {
  if (isManual) {
    show('updateModal'); show('updateStateChecking'); hide('updateStateResult');
    hide('updateErrorMsg'); hide('updateSuccessMsg');
  }
  try {
    var d = await api('admin/system/update/check');
    hide('updateAvailableDot');
    if (!isManual) {
      if (d.current) setText('appVersionText', d.current);
      return;
    }
    hide('updateStateChecking'); show('updateStateResult');
    setText('updateModalCurVer', d.current || '0.1.0');
    setText('updateModalLatestVer', d.latest || '—');
    var badge = $('updateStatusBadge');
    var notes = $('updateNotesBox');
    var dockerTip = $('updateDockerTip');
    var btnDo = $('btnDoUpdate');
    if (d.supported) {
      if (badge) {
        badge.className = 'text-xs font-semibold px-3 py-1 rounded-full bg-indigo-500/20 text-indigo-400 border border-indigo-500/30';
        badge.innerText = d.has_update ? '发现新版本' : '当前已是最新';
      }
      if (notes && d.release_notes) { notes.innerText = '更新说明：\n' + d.release_notes; notes.classList.remove('hidden'); }
      if (btnDo) { if (d.has_update) btnDo.classList.remove('hidden'); else btnDo.classList.add('hidden'); }
    } else {
      // 本项目没有发布渠道：如实显示"不支持"，并隐藏一键升级按钮
      if (badge) {
        badge.className = 'text-xs font-semibold px-3 py-1 rounded-full bg-amber-500/20 text-amber-300 border border-amber-500/30';
        badge.innerText = '不支持自动更新';
      }
      if (notes) { notes.innerText = d.reason || '本项目未配置发布渠道，无法检查更新。'; notes.classList.remove('hidden'); }
      if (dockerTip) dockerTip.classList.add('hidden');
      if (btnDo) btnDo.classList.add('hidden');
    }
  } catch (e) {
    if (isManual) {
      hide('updateStateChecking'); show('updateStateResult');
      var errEl = $('updateErrorMsg');
      if (errEl) {
        errEl.innerText = isMissingEndpoint(e)
          ? '该端点不可用（后端未实现 GET /admin/system/update/check）'
          : ('检查更新失败：' + errText(e));
        errEl.classList.remove('hidden');
      }
    }
  }
}
async function triggerAppUpdate() {
  var btn = $('btnDoUpdate');
  if (btn) {
    btn.disabled = true;
    btn.innerText = '正在下载并更新...';
  }
  var successEl = $('updateSuccessMsg');
  var errorEl = $('updateErrorMsg');
  if (successEl) successEl.classList.add('hidden');
  if (errorEl) errorEl.classList.add('hidden');

  try {
    var res = await api('admin/system/update/apply', { method: 'POST' });
    if (successEl) {
      successEl.innerText = res.message || '更新完成！服务正在重启...';
      successEl.classList.remove('hidden');
    }
    toast(res.message || '更新成功！网关正在自动重启...', 'ok');
    if (btn) btn.innerText = '正在重启服务...';
    // 轮询健康检查，一旦重启成功自动刷新页面
    var pollCount = 0;
    var timer = setInterval(async function () {
      pollCount++;
      try {
        var r = await fetch('/healthz');
        if (r.ok) {
          clearInterval(timer);
          toast('网关已成功重启到新版本！', 'ok');
          setTimeout(function () { location.reload(); }, 1200);
        }
      } catch (e) {
        if (pollCount > 30) {
          clearInterval(timer);
          toast('重启等待超时，请手动确认网关运行状态', 'warn');
        }
      }
    }, 1000);
  } catch (e) {
    if (btn) {
      btn.disabled = false;
      btn.innerText = '一键自动更新';
    }
    if (errorEl) {
      errorEl.innerText = '自动更新失败：' + errText(e);
      errorEl.classList.remove('hidden');
    }
    toast('更新失败：' + errText(e), 'err');
  }
}

// ---------------------------------------------------------------- 启动

(function init() {
  try { currentApiKey = localStorage.getItem('trae2api_key') || ''; } catch (e) { currentApiKey = ''; }
  if ($('inputApiKey')) $('inputApiKey').value = currentApiKey;
  if ($('chatApiKey')) $('chatApiKey').value = currentApiKey;
  setText('displayBaseUrlText', location.origin + '/v1');
  applyCurrentUser();
  switchMainTab('accounts');
  refreshData();
  setInterval(function () {
    // 后台标签页不轮询：账号池状态只在账号池页可见时才需要刷新
    if (currentActiveTab === 'accounts' || currentActiveTab === 'access') refreshData();
  }, 8000);
  setInterval(function () { if (currentActiveTab === 'dsh') fetchDshStatus(false); }, 5000);
  setTimeout(function () { checkAppUpdate(false); }, 2000);
})();
