document.addEventListener("DOMContentLoaded", async () => {
  if (!(await ensureAuth())) return;
  initConfig(); initActions(); initLogs();
});

let stateTasks = [];
let stateUpstreams = [];
let stateStrategies = [];

// ── Auth ──

async function ensureAuth() {
  try {
    const r = await fetch("/api/config");
    if (r.status === 401) { showLogin(); return false; }
    return true;
  } catch (e) { showLogin(); return false; }
}

function showLogin() {
  const ov = $("login-overlay"); if (!ov) return;
  ov.classList.remove("hidden");
  const pwd = $("login-password");
  if (pwd) {
    pwd.focus();
    pwd.addEventListener("keydown", e => { if (e.key === "Enter") submitLogin(); });
  }
  $("login-submit")?.addEventListener("click", submitLogin);
}

async function submitLogin() {
  const pwd = $("login-password")?.value || "";
  const err = $("login-error");
  try {
    const r = await fetch("/api/login", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ password: pwd }) });
    if (r.ok) { location.reload(); return; }
    if (err) err.classList.remove("hidden");
  } catch (e) {
    if (err) { err.textContent = "登录失败"; err.classList.remove("hidden"); }
  }
}

async function apiGet(p) { const r = await fetch(p); if (!r.ok) throw new Error(r.status); return r.json(); }
async function apiPost(p, b) {
  const r = await fetch(p, { method:"POST", headers:{"Content-Type":"application/json"}, body:JSON.stringify(b??{}) });
  if (!r.ok) { const m = await r.text().catch(()=>""); throw new Error(`${r.status} ${m}`); }
  try { return await r.json(); } catch { return {}; }
}

function $(id) { return document.getElementById(id); }
function rid(p) { return p+"-"+Math.random().toString(36).slice(2,8); }
function esc(s) { return String(s||"").replace(/&/g,"&amp;").replace(/</g,"&lt;").replace(/>/g,"&gt;").replace(/"/g,"&quot;"); }
function getVal(id) { const e=$(id); return e?e.value:""; }
function setVal(id,v) { const e=$(id); if(e)e.value=v??""; }
function isChk(id) { const e=$(id); return !!(e&&e.checked); }
function setChk(id,v) { const e=$(id); if(e)e.checked=!!v; }

// ── Init ──

function initConfig() { loadConfig().catch(console.error); updateWSStatus(); setInterval(updateWSStatus, 3000); }

async function loadConfig() {
  const c = await apiGet("/api/config");
  const ws = c.wsServer || {};
  setVal("wsServerPath", ws.path||"/ws");
  setVal("wsServerKey", ws.key||"");
  setChk("wsServerEnabled", ws.enabled!==false);
  setChk("wsServerApplySkip", !!ws.applySkip);
  updateWSEndpoint();
  stateUpstreams = (c.upstreams || []).map(u => ({
    id: u.id||rid("up"), name: u.name||"", wsUrl: u.wsUrl||"", wsKey: u.wsKey||"", enabled: !!u.enabled
  }));
  if (!stateUpstreams.length) stateUpstreams = [{ id:rid("up"), name:"", wsUrl:"", wsKey:"", enabled:false }];
  renderUpstreams(stateUpstreams);
  const sel = $("dispatchMode"); if (sel) sel.value = (c.dispatch==="all"||c.dispatch==="random")?c.dispatch:"round-robin";
  stateTasks = normalizeTasks(c);
  renderTasks(stateTasks);
  stateStrategies = normalizeStrategies(c);
  renderStrategies(stateStrategies);
}

function initActions() {
  $("btn-save-config")?.addEventListener("click", async () => {
    try { await apiPost("/api/config", collectPayload()); showToast("配置已保存"); await loadConfig(); }
    catch(e) { showToast("保存失败: "+e.message, "error"); }
  });
  $("btn-import-json")?.addEventListener("click", () => {
    const s = prompt("粘贴 config.json:"); if (!s) return;
    try {
      const c = JSON.parse(s); const ws = c.wsServer||{};
      setVal("wsServerPath", ws.path||"/ws"); setVal("wsServerKey", ws.key||"");
      setChk("wsServerEnabled", ws.enabled!==false); setChk("wsServerApplySkip", !!ws.applySkip); updateWSEndpoint();
      stateUpstreams = (c.upstreams||[]).map(u=>({id:u.id||rid("up"),name:u.name||"",wsUrl:u.wsUrl||"",wsKey:u.wsKey||"",enabled:!!u.enabled}));
      if (!stateUpstreams.length) stateUpstreams = [{id:rid("up"),name:"",wsUrl:"",wsKey:"",enabled:false}];
      renderUpstreams(stateUpstreams);
      const sel = $("dispatchMode"); if (sel) sel.value = (c.dispatch==="all"||c.dispatch==="random")?c.dispatch:"round-robin";
      stateTasks = normalizeTasks(c); renderTasks(stateTasks);
      stateStrategies = normalizeStrategies(c); renderStrategies(stateStrategies);
      log("JSON 已导入");
    } catch(e) { alert("解析失败: "+e.message); }
  });
  $("btn-add-task")?.addEventListener("click", () => { stateTasks = collectTasks(); stateTasks.push(defaultTask()); renderTasks(stateTasks); });
  $("btn-add-account")?.addEventListener("click", () => { stateTasks = collectTasks(); stateTasks.push(defaultTask()); renderTasks(stateTasks); });
  $("btn-add-strategy")?.addEventListener("click", () => { stateStrategies = collectStrategies(); stateStrategies.push(defaultStrategy()); renderStrategies(stateStrategies); });
  $("btn-add-upstream")?.addEventListener("click", () => {
    stateUpstreams.push({ id:rid("up"), name:"", wsUrl:"", wsKey:"", enabled:false });
    renderUpstreams(stateUpstreams);
  });
  $("wsServerPath")?.addEventListener("input", updateWSEndpoint);
  $("wsServerKey")?.addEventListener("input", updateWSEndpoint);
}

// ── Config ──

function collectPayload() {
  const wsServer = { path: getVal("wsServerPath")||"/ws", key: getVal("wsServerKey")||"", enabled: isChk("wsServerEnabled"), applySkip: isChk("wsServerApplySkip") };
  const dispatch = $("dispatchMode")?.value || "round-robin";
  const upstreams = collectUpstreams();
  const tasks = collectTasks().map(n);
  validate(tasks); stateTasks = tasks;
  const strategies = collectStrategies();
  stateStrategies = strategies;
  return { upstreams, wsServer, dispatch, tasks, strategies };
}

function collectUpstreams() {
  return [...document.querySelectorAll('[data-upstream-card]')].map(card => {
    const id = card.getAttribute("data-upstream-id");
    return {
      id: id || rid("up"),
      name: card.querySelector('[data-field="name"]')?.value || "",
      wsUrl: card.querySelector('[data-field="wsUrl"]')?.value || "",
      wsKey: card.querySelector('[data-field="wsKey"]')?.value || "",
      enabled: card.querySelector('[data-field="enabled"]')?.checked || false
    };
  });
}

function normalizeTasks(c) {
  const t = Array.isArray(c?.tasks) ? c.tasks : [];
  return t.length > 0 ? t.map(n) : [n({ id:"default", name:"Account 1", enabled:true, apiUrl:"https://www.binance.com/bapi/futures/v2/private/future/event-contract/place-order", method:"POST", headers:"Content-Type: application/json\nclienttype: web", body:'{"orderAmount":"{{amount}}","timeIncrements":"{{unit}}","symbolName":"BTCUSDT","payoutRatio":"0.80","direction":"{{action}}"}', valueBuy:"LONG", valueSell:"SHORT" })];
}

function n(t) {
  t = t || {};
  const auth = t.auth && typeof t.auth === "object" ? t.auth : {};
  return { id: String(t.id||"").trim()||rid("acct"), name: String(t.name||"").trim()||"Account", enabled: t.enabled!==false,
    type: String(t.type||"binance").trim(), auth,
    symbols: t.symbols && typeof t.symbols === "object" ? t.symbols : {},
    timeRanges: ctr(t.timeRanges),
    expiresAt: +t.expiresAt||0,
    apiUrl: String(t.apiUrl||""), method: String(t.method||"POST").toUpperCase(),
    headers: String(t.headers||""), body: String(t.body||""),
    valueBuy: String(t.valueBuy||""), valueSell: String(t.valueSell||"") };
}

function defaultTask() { return n({ id:rid("acct"), name:"New Account", enabled:true, valueBuy:"LONG", valueSell:"SHORT" }); }

function platformDefaults(type) {
  if (type === "turboflow") return { valueBuy: "1", valueSell: "3" };
  if (type === "hibt") return { valueBuy: "1", valueSell: "-1" };
  if (type === "raw") return { valueBuy: "buy", valueSell: "sell" };
  return { valueBuy: "LONG", valueSell: "SHORT" };
}

window.onAccountTypeChange = function(sel) {
  const card = sel.closest('[data-task-card="1"]'); if (!card) return;
  const d = platformDefaults(sel.value);
  const set = (f, v) => { const e = card.querySelector(`[data-field="${f}"]`); if (e && !e.value) e.value = v; };
  set("valueBuy", d.valueBuy);
  set("valueSell", d.valueSell);
};

window.syncBinanceAuthFields = function(input) {
  const card = input.closest('[data-task-card="1"]'); if (!card) return;
  const csrf = card.querySelector('[data-field="auth-csrftoken"]')?.value || "";
  const p20t = card.querySelector('[data-field="auth-p20t"]')?.value || "";
  const authField = card.querySelector('[data-field="auth"]');
  const auth = { csrftoken: csrf.trim(), p20t: p20t.trim() };
  if (authField) authField.value = JSON.stringify(auth, null, 2);
  const tid = card.getAttribute("data-task-id");
  const st = stateTasks.find(t => t.id === tid);
  if (st) st.auth = auth;
};

// ── Upstream Render ──

function renderUpstreams(us) {
  const c = $("upstreams-container"); if (!c) return;
  if (!us?.length) { c.innerHTML = '<div class="text-[11px] text-gray-400">暂无上游，点击"+ 上游"添加</div>'; return; }
  c.innerHTML = us.map(u => upstreamCard(u)).join("\n");
  // Bind delete buttons
  c.querySelectorAll('[data-action="delete-upstream"]').forEach(btn => {
    btn.addEventListener("click", () => {
      const id = btn.getAttribute("data-upstream-id");
      stateUpstreams = stateUpstreams.filter(u => u.id !== id);
      renderUpstreams(stateUpstreams);
    });
  });
}

function upstreamCard(u) {
  const id = u.id;
  return `<div class="border rounded-lg p-3 bg-white" data-upstream-card data-upstream-id="${id}">
    <div class="flex items-center gap-2 flex-wrap">
      <input class="border rounded px-2 py-1 text-xs font-semibold" style="width:7rem" data-field="name" value="${esc(u.name)}" placeholder="名称" />
      <input class="border rounded px-2 py-1 text-xs flex-1" style="min-width:14rem" data-field="wsUrl" value="${esc(u.wsUrl)}" placeholder="wss://host:port/ws" />
      <input class="border rounded px-2 py-1 text-xs" style="width:7rem" data-field="wsKey" value="${esc(u.wsKey)}" placeholder="密钥(可选)" />
      <label class="switch-label"><span class="text-[11px] text-gray-500">启用</span><span class="switch"><input type="checkbox" data-field="enabled" ${u.enabled?"checked":""} /><span class="switch-track"></span></span></label>
      <span class="inline-flex items-center text-[11px] px-2 py-0.5 rounded-full border bg-gray-100 text-gray-500" data-upstream-status="${id}">-</span>
      <button class="border border-red-200 rounded px-2 py-1 text-[11px] bg-white text-red-600 hover:bg-red-50" data-action="delete-upstream" data-upstream-id="${id}">✕</button>
    </div>
  </div>`;
}

// ── Render ──

function renderTasks(ts) {
  const c = $("tasks-container"); if (!c) return;
  if (!ts?.length) { c.innerHTML = '<div class="bg-white border rounded-lg p-6 text-center text-xs text-gray-400">暂无账号，点击"+ 账号"添加</div>'; return; }
  c.innerHTML = ts.map((t,i) => card(t,i+1)).join("\n");
  initCountdowns(); bind(c);
}

function authPlaceholder(type) {
  if (type === "binance") return '{"csrftoken":"...","p20t":"..."}';
  if (type === "hibt") return '{"v":"...","x-auth-token":"...","authorization":"..."}';
  if (type === "turboflow") return '{"authorization":"...","uid":"...","biz-pf":"..."}';
  return '{}';
}

function authDisplayValue(t) {
  const auth = t.auth || {};
  return JSON.stringify(auth, null, 2);
}

function platformBadge(type) {
  const map = {
    binance: "bg-yellow-50 text-yellow-700 border-yellow-200",
    hibt: "bg-blue-50 text-blue-700 border-blue-200",
    turboflow: "bg-purple-50 text-purple-700 border-purple-200"
  };
  const cls = map[type] || "bg-gray-50 text-gray-600 border-gray-200";
  return `<span class="inline-flex items-center text-[10px] px-1.5 py-0.5 rounded-full border ${cls}">${esc(type)}</span>`;
}

function card(t, idx) {
  const id = t.id;
  return `<div class="bg-white border rounded-lg p-3 space-y-2.5" data-task-card="1" data-task-id="${id}">
    <!-- Header -->
    <div class="flex items-center justify-between gap-2 pb-2 border-b" style="border-color:var(--gl)">
      <div class="flex items-center gap-2">
        <span class="inline-flex items-center justify-center w-6 h-6 text-[11px] font-bold rounded" style="background:var(--gl);color:var(--gt)">#${idx}</span>
        <input class="border rounded px-2 py-1 text-xs font-semibold w-28" data-field="name" value="${esc(t.name)}" />
        ${platformBadge(t.type)}
        <span id="countdown-${id}" class="countdown hidden"></span>
        <label class="switch-label"><span class="switch"><input type="checkbox" data-field="enabled" ${t.enabled?"checked":""} /><span class="switch-track"></span></span></label>
      </div>
      <div class="flex items-center gap-1 flex-wrap">
        <button class="border rounded px-2 py-1 text-[10px] bg-yellow-50 text-yellow-700 hover:bg-yellow-100" onclick="promptImportPlatformToken('${id}','binance')">币安Token</button>
        <button class="border rounded px-2 py-1 text-[10px] bg-blue-50 text-blue-700 hover:bg-blue-100" onclick="promptImportPlatformToken('${id}','hibt')">HIBT Token</button>
        <button class="border rounded px-2 py-1 text-[10px] bg-purple-50 text-purple-700 hover:bg-purple-100" onclick="promptImportPlatformToken('${id}','turboflow')">TurboFlow Token</button>
        <button class="rounded px-2 py-1 text-[11px] font-medium text-white" style="background:var(--g)" data-action="test-buy" data-task-id="${id}">BUY</button>
        <button class="border rounded px-2 py-1 text-[11px] bg-white hover:bg-gray-50" data-action="test-sell" data-task-id="${id}">SELL</button>
        <button class="border border-red-200 rounded px-2 py-1 text-[11px] bg-white text-red-600 hover:bg-red-50" data-action="delete" data-task-id="${id}">✕</button>
      </div>
    </div>

    <!-- Row: Platform & Auth -->
    <div class="flex gap-2 items-end">
      <div style="width:8rem"><label class="block text-[11px] text-gray-500 mb-0.5">平台</label><select class="border rounded w-full px-2 py-1 text-xs bg-white" data-field="type" onchange="onAccountTypeChange(this)">${["binance","hibt","turboflow","raw"].map(m=>`<option ${t.type===m?"selected":""}>${m}</option>`).join("")}</select></div>
      <div class="flex-1"><label class="block text-[11px] text-gray-500 mb-0.5">Auth</label><textarea class="border rounded w-full px-2 py-1 text-xs font-mono resize-y" rows="3" data-field="auth" placeholder="${esc(authPlaceholder(t.type))}">${esc(authDisplayValue(t))}</textarea></div>
    </div>

    ${t.type === "binance" ? `<div class="flex gap-2 items-end">
      <div class="flex-1"><label class="block text-[11px] text-gray-500 mb-0.5">csrftoken</label><input class="border rounded w-full px-2 py-1 text-xs font-mono" data-field="auth-csrftoken" value="${esc(t.auth?.csrftoken||"")}" oninput="syncBinanceAuthFields(this)" /></div>
      <div class="flex-1"><label class="block text-[11px] text-gray-500 mb-0.5">p20t</label><input class="border rounded w-full px-2 py-1 text-xs font-mono" data-field="auth-p20t" value="${esc(t.auth?.p20t||"")}" oninput="syncBinanceAuthFields(this)" /></div>
    </div>` : ""}

    ${t.type === "raw" ? `<div class="grid gap-2 sm:grid-cols-2">
      <div><label class="block text-[11px] text-gray-500 mb-0.5">API URL</label><input class="border rounded w-full px-2 py-1 text-xs font-mono" data-field="apiUrl" value="${esc(t.apiUrl)}" placeholder="https://..." /></div>
      <div><label class="block text-[11px] text-gray-500 mb-0.5">Method</label><select class="border rounded w-full px-2 py-1 text-xs bg-white" data-field="method">${["GET","POST","PUT","DELETE"].map(m=>`<option ${t.method===m?"selected":""}>${m}</option>`).join("")}</select></div>
      <div class="sm:col-span-2"><label class="block text-[11px] text-gray-500 mb-0.5">Headers</label><textarea class="border rounded w-full px-2 py-1 text-xs font-mono resize-y" rows="3" data-field="headers" placeholder="Key: Value">${esc(t.headers)}</textarea></div>
      <div class="sm:col-span-2"><label class="block text-[11px] text-gray-500 mb-0.5">Body</label><textarea class="border rounded w-full px-2 py-1 text-xs font-mono resize-y" rows="3" data-field="body" placeholder='{"key":"{{action}}"}'>${esc(t.body)}</textarea></div>
    </div>` : ""}

    <!-- Row: Time ranges -->
    <div class="flex gap-2 items-start">
      <div class="flex-1">
        <label class="block text-[11px] text-gray-500 mb-0.5">⏰ 有效时间段 <span class="text-gray-400">(留空=全天)</span></label>
        <div class="space-y-1" data-time-ranges="1" data-task-id="${id}">${trHtml(id, t.timeRanges)}</div>
        <button class="border rounded px-2 py-0.5 text-[10px] bg-white hover:bg-gray-50 mt-1" data-action="add-time-range" data-task-id="${id}">+ 时段</button>
      </div>
    </div>

    <!-- Row: buy / sell / 过期 -->
    <div class="flex items-center gap-2 flex-wrap">
      <div class="flex items-center gap-1"><span class="text-[11px] text-gray-500 flex-shrink-0">buy→</span><input class="border rounded px-2 py-1 text-xs" style="width:5rem" data-field="valueBuy" value="${esc(t.valueBuy)}" placeholder="LONG" /></div>
      <div class="flex items-center gap-1"><span class="text-[11px] text-gray-500 flex-shrink-0">sell→</span><input class="border rounded px-2 py-1 text-xs" style="width:5rem" data-field="valueSell" value="${esc(t.valueSell)}" placeholder="SHORT" /></div>
      <input type="hidden" data-field="symbols" value="${esc(JSON.stringify(t.symbols||{}))}" />
      <input type="hidden" data-field="expiresAt" value="${t.expiresAt||0}" />
      <div class="expires-row">
        <span id="expires-display-${id}" class="expires-display">${fmtDT(t.expiresAt)}</span>
        <div class="expires-btns">
          <button class="expires-btn" onclick="setExpires('${id}',1)">+1d</button>
          <button class="expires-btn" onclick="setExpires('${id}',5)">+5d</button>
        </div>
        <button class="expires-clear" onclick="setExpires('${id}',0)">清除</button>
      </div>
    </div>
  </div>`;
}

// ── Time ranges ──

function ntr(r) { return Array.isArray(r)?r.map(x=>({start:String(x?.start||"").trim(),end:String(x?.end||"").trim()})):[]; }
function ctr(r) { return ntr(r).filter(x=>x.start||x.end); }
function updTR(tid, fn) { const t=stateTasks.find(x=>x.id===tid); if(t) t.timeRanges=ntr(fn(ntr(t.timeRanges))); }
function colTR(card) { return [...card.querySelectorAll("[data-time-range-field='start']")].map(s=>{const i=s.getAttribute("data-index"),e=card.querySelector(`[data-time-range-field='end'][data-index='${cssEscape(i)}']`);return{start:String(s.value||"").trim(),end:String(e?.value||"").trim()}}); }

// ── Collect ──

function collectTasks() {
  return [...document.querySelectorAll('[data-task-card="1"]')].map(card => {
    const id=card.getAttribute("data-task-id")||rid("acct");
    const g=f=>{const e=card.querySelector(`[data-field="${f}"]`);return e?e.value:""};
    const gc=f=>{const e=card.querySelector(`[data-field="${f}"]`);return !!(e&&e.checked)};
    const type = String(g("type")||"binance").trim() || "binance";
    let auth = {};
    if (type === "binance") {
      try { auth = JSON.parse(g("auth")||"{}") || {}; } catch { auth = {}; }
      const csrf = String(g("auth-csrftoken")||"").trim();
      const p20t = String(g("auth-p20t")||"").trim();
      if (csrf || p20t) {
        auth.csrftoken = csrf;
        auth.p20t = p20t;
      }
    } else {
      try { auth = JSON.parse(g("auth")||"{}") || {}; } catch { auth = {}; }
    }
    let symbols = {};
    try { symbols = JSON.parse(g("symbols")||"{}") || {}; } catch { symbols = {}; }
    return { id, name: String(g("name")||"").trim()||id, enabled: gc("enabled"),
      type, auth, symbols,
      timeRanges: colTR(card), expiresAt:+g("expiresAt")||0,
      apiUrl: String(g("apiUrl")||"").trim(), method: String(g("method")||"POST").trim().toUpperCase(),
      headers: String(g("headers")||""), body: String(g("body")||""),
      valueBuy: String(g("valueBuy")||"").trim(), valueSell: String(g("valueSell")||"").trim() };
  });
}

// ── Strategies ──

function normalizeStrategies(c) {
  const s = Array.isArray(c?.strategies) ? c.strategies : [];
  return s.map(ns);
}

function ns(s) {
  s = s || {};
  // name/id 统一为 id，页面直接编辑 id
  const id = String(s.id || s.name || "").trim() || rid("strategy");
  return {
    id,
    name: id,
    enabled: s.enabled!==false,
    groups: Array.isArray(s.groups) ? s.groups.map(ng) : []
  };
}

function ng(g) {
  g = g || {};
  let accounts = [];
  if (Array.isArray(g.accounts)) {
    accounts = g.accounts.map(a => ({ accountId: String(a.accountId||""), amount: String(a.amount||"").trim() }));
  } else if (Array.isArray(g.accountIds)) {
    accounts = g.accountIds.map(id => ({ accountId: String(id), amount: "" }));
  }
  return {
    id: String(g.id||"").trim()||rid("grp"),
    name: String(g.name||"").trim()||"新分组",
    enabled: g.enabled!==false,
    dispatch: String(g.dispatch||"random").trim() || "random",
    accounts
  };
}

function defaultGroup() { return ng({ id:rid("grp"), name:"新分组", enabled:true, dispatch:"random", accounts:[] }); }
function defaultStrategy() { return ns({ name:rid("strategy"), enabled:true, groups:[defaultGroup()] }); }

function renderStrategies(ss) {
  const c = $("strategies-container"); if (!c) return;
  if (!ss?.length) { c.innerHTML = '<div class="text-[11px] text-gray-400">暂无策略，点击"+ 策略"添加</div>'; return; }
  c.innerHTML = ss.map((s,i) => strategyCard(s,i+1)).join("\n");
  bindStrategies(c);
}

function strategyCard(s, idx) {
  const groupsHtml = s.groups.map((g) => groupHtml(g)).join("\n");
  return `<div class="bg-white border rounded-lg p-3 space-y-2" data-strategy-card data-strategy-id="${s.id}">
    <div class="flex items-center gap-2 flex-wrap">
      <span class="inline-flex items-center justify-center w-6 h-6 text-[11px] font-bold rounded" style="background:var(--gl);color:var(--gt)">#${idx}</span>
      <input class="border rounded px-2 py-1 text-xs font-semibold w-48" data-field="strategy-id" value="${esc(s.id)}" title="策略ID（name/id已统一，直接改这里）" />
      <label class="switch-label"><span class="text-[11px] text-gray-500">启用</span><span class="switch"><input type="checkbox" data-field="strategy-enabled" ${s.enabled?"checked":""} /><span class="switch-track"></span></span></label>
      <div class="flex-1"></div>
      <button class="border rounded px-2 py-1 text-[11px] bg-white hover:bg-gray-50" data-action="add-group" data-strategy-id="${s.id}">+ 分组</button>
      <button class="border border-red-200 rounded px-2 py-1 text-[11px] bg-white text-red-600 hover:bg-red-50" data-action="delete-strategy" data-strategy-id="${s.id}">✕</button>
    </div>
    <div class="space-y-2" data-groups-container>${groupsHtml}</div>
  </div>`;
}

function groupHtml(g) {
  const opts = ["random","round-robin","all"].map(m=>`<option ${g.dispatch===m?"selected":""}>${m}</option>`).join("");
  const rows = stateTasks.length
    ? stateTasks.map(t=>{
        const binding = (g.accounts||[]).find(a => a.accountId === t.id) || { amount: "" };
        const checked = (g.accounts||[]).some(a => a.accountId === t.id) ? "checked" : "";
        return `<div class="flex items-center gap-1.5"><input type="checkbox" data-group-account="${esc(t.id)}" ${checked} /><span class="text-[11px] text-gray-600">${esc(t.name)}</span><input class="border rounded px-1.5 py-0.5 text-[11px]" style="width:4rem" data-group-amount="${esc(t.id)}" value="${esc(binding.amount)}" placeholder="金额" /></div>`;
      }).join("")
    : '<span class="text-[11px] text-gray-400">暂无账号，请先在账号库添加</span>';
  return `<div class="border rounded p-2 bg-gray-50" data-group-card data-group-id="${g.id}">
    <div class="flex items-center gap-2 flex-wrap">
      <input class="border rounded px-2 py-1 text-xs font-medium w-40 bg-white" data-field="group-name" value="${esc(g.name)}" />
      <select class="border rounded px-2 py-1 text-xs bg-white" data-field="group-dispatch">${opts}</select>
      <label class="switch-label"><span class="text-[11px] text-gray-500">启用</span><span class="switch"><input type="checkbox" data-field="group-enabled" ${g.enabled?"checked":""} /><span class="switch-track"></span></span></label>
      <button class="border border-red-200 rounded px-1.5 py-0.5 text-[10px] bg-white text-red-600 hover:bg-red-50" data-action="delete-group" data-group-id="${g.id}">✕</button>
    </div>
    <div class="mt-1 flex flex-wrap gap-x-3 gap-y-1">${rows}</div>
  </div>`;
}

function bindStrategies(container) {
  const sidOf = (el) => {
    const card = el.closest('[data-strategy-card]');
    if (!card) return "";
    const input = card.querySelector('[data-field="strategy-id"]');
    return String(input?.value || "").trim() || card.getAttribute("data-strategy-id") || "";
  };
  container.querySelectorAll('[data-action="delete-strategy"]').forEach(b => {
    b.addEventListener("click", () => {
      const id = sidOf(b);
      stateStrategies = stateStrategies.filter(s => s.id !== id);
      renderStrategies(stateStrategies);
    });
  });
  container.querySelectorAll('[data-action="add-group"]').forEach(b => {
    b.addEventListener("click", () => {
      const id = sidOf(b);
      const st = stateStrategies.find(s => s.id === id);
      if (st) { st.groups.push(defaultGroup()); renderStrategies(stateStrategies); }
    });
  });
  container.querySelectorAll('[data-action="delete-group"]').forEach(b => {
    b.addEventListener("click", () => {
      const gid = b.getAttribute("data-group-id");
      const id = sidOf(b);
      const st = stateStrategies.find(s => s.id === id);
      if (st) { st.groups = st.groups.filter(g => g.id !== gid); renderStrategies(stateStrategies); }
    });
  });
}

function collectStrategies() {
  return [...document.querySelectorAll('[data-strategy-card]')].map(card => {
    const g = f => { const e = card.querySelector(`[data-field="${f}"]`); return e ? e.value : ""; };
    const gc = f => { const e = card.querySelector(`[data-field="${f}"]`); return !!(e && e.checked); };
    const id = String(g("strategy-id")||"").trim() || card.getAttribute("data-strategy-id") || rid("strategy");
    const groups = [...card.querySelectorAll('[data-group-card]')].map(gcrd => {
      const gid = gcrd.getAttribute("data-group-id") || rid("grp");
      const gg = f => { const e = gcrd.querySelector(`[data-field="${f}"]`); return e ? e.value : ""; };
      const gchk = f => { const e = gcrd.querySelector(`[data-field="${f}"]`); return !!(e && e.checked); };
      const accounts = [...gcrd.querySelectorAll('[data-group-account]')].filter(c => c.checked).map(c => {
        const aid = c.getAttribute("data-group-account");
        const amt = gcrd.querySelector(`[data-group-amount="${cssEscape(aid)}"]`);
        return { accountId: aid, amount: String(amt?.value || "").trim() };
      });
      return { id: gid, name: String(gg("group-name")||"").trim()||gid, enabled: gchk("group-enabled"), dispatch: String(gg("group-dispatch")||"random").trim()||"random", accounts };
    });
    return { id, name: id, enabled: gc("strategy-enabled"), groups };
  });
}

function validate(ts) { ts.forEach(t=>{ vtr(t); }); }
function vtr(t) { const r=ctr(t.timeRanges); if(r.length>4) throw new Error(`账号[${t.name}] 最多4段`);
  r.forEach((x,i)=>{ const l=`账号[${t.name}] 时段#${i+1}`; if(!x.start||!x.end) throw new Error(`${l} 起止必填`);
    const sm=pm(x.start,l), em=pm(x.end,l); if(sm===em) throw new Error(`${l} 起止不能相同`); }); }
function pm(v,l) { if(!/^\d{2}:\d{2}$/.test(v)) throw new Error(`${l} 需HH:MM格式`); const [h,m]=v.split(":").map(Number); if(h<0||h>23||(m!==0&&m!==30)) throw new Error(`${l} 仅支持整点或半点`); return h*60+m; }

// ── WS Status ──

async function updateWSStatus() {
  try {
    const s = await apiGet("/api/ws/status");
    const el = $("ws-server-status");
    if (el) {
      const n = s.wsServerConns||0;
      el.innerHTML = `接入 <strong class="ml-0.5">${n}</strong>`;
      el.className = `inline-flex items-center text-[11px] px-2 py-0.5 rounded-full border ${n>0?'bg-green-50 text-green-700 border-green-200':'bg-gray-100 text-gray-500'}`;
    }
    // Update each upstream status
    const items = s.upstream?.items || [];
    items.forEach(u => {
      const st = document.querySelector(`[data-upstream-status="${u.id}"]`);
      if (st) {
        st.textContent = u.connected ? "已连接" : "未连接";
        st.className = `inline-flex items-center text-[11px] px-2 py-0.5 rounded-full border ${u.connected?'bg-green-50 text-green-700 border-green-200':'bg-gray-100 text-gray-500'}`;
      }
    });
  } catch(e) {}
}

function updateWSEndpoint() {
  const el = $("wsServerEndpoint"); if (!el) return;
  const p = getVal("wsServerPath")||"/ws", k = getVal("wsServerKey")||"";
  el.textContent = location.origin + p + (k?"?key="+encodeURIComponent(k):"");
}

function copyWSEndpoint() {
  const t = $("wsServerEndpoint")?.textContent||"";
  navigator.clipboard.writeText(t).then(()=>log("已复制")).catch(()=>prompt("复制:",t));
}

// ── Expires ──

let cdTimer;
function initCountdowns() { if(cdTimer) clearInterval(cdTimer); cdTimer=setInterval(()=>stateTasks.forEach(t=>updateCD(t.id,t.expiresAt)),1000); }
function updateCD(tid, ts) {
  const el = $(`countdown-${tid}`); if (!el) return;
  if (!ts) { el.classList.add("hidden"); return; }
  el.classList.remove("hidden","countdown-normal","countdown-warn","countdown-danger");
  const d=ts-Math.floor(Date.now()/1000);
  if(d<=0){el.textContent="已过期";el.classList.add("countdown-danger");return}
  const h=Math.floor(d/3600),m=Math.floor((d%3600)/60);
  if(h>24){const dd=Math.floor(h/24);el.textContent=`${dd}d${h%24}h`;el.classList.add("countdown-normal")}
  else if(h>=1){el.textContent=`${h}h${m}m`;el.classList.add("countdown-normal")}
  else{el.textContent=`${m}m${d%60}s`;el.classList.add("countdown-warn")}
}
window.updateCD = updateCD;

function setExpires(tid, days) {
  const card = document.querySelector(`[data-task-card="1"][data-task-id="${cssEscape(tid)}"]`);
  if (!card) return;
  const inp = card.querySelector('[data-field="expiresAt"]');
  const disp = $(`expires-display-${tid}`);
  if (!inp||!disp) return;
  let v=0;
  if (days>0) { const n=Math.floor(Date.now()/1000); let c=+inp.value||0; if(c<n)c=n; v=c+days*86400; }
  inp.value=v; disp.textContent=fmtDT(v);
  const t=stateTasks.find(x=>x.id===tid); if(t) t.expiresAt=v;
  updateCD(tid, v);
}
window.setExpires = setExpires;
function fmtDT(u) { if(!u)return"未设置";const d=new Date(u*1000),p=n=>String(n).padStart(2,"0");return`${d.getFullYear()}-${p(d.getMonth()+1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}`; }

// ── Time range HTML ──

function trHtml(tid, ranges) {
  const rs = ntr(ranges);
  if (!rs.length) return '<div class="border rounded px-2 py-1 text-xs text-gray-400 bg-gray-50">全天</div>';
  return rs.map((r,i) => `<div class="flex items-center gap-1.5">
    <input class="border rounded px-2 py-1 text-[11px] w-20" data-time-range-field="start" data-index="${i}" value="${esc(r.start)}" placeholder="09:00" />
    <span class="text-[11px] text-gray-400">→</span>
    <input class="border rounded px-2 py-1 text-[11px] w-20" data-time-range-field="end" data-index="${i}" value="${esc(r.end)}" placeholder="17:00" />
    <button class="border rounded px-1.5 py-0.5 text-[10px] bg-white text-red-600 hover:bg-red-50" data-action="delete-time-range" data-task-id="${tid}" data-index="${i}">✕</button>
  </div>`).join("\n");
}

// ── Bind events ──

function bind(container) {
  container.querySelectorAll('[data-action="delete"]').forEach(b => b.addEventListener("click", () => {
    const id = b.getAttribute("data-task-id");
    stateTasks = stateTasks.filter(t => t.id !== id);
    renderTasks(stateTasks);
  }));
  container.querySelectorAll('[data-action="test-buy"]').forEach(b => b.addEventListener("click", () => testTask(b, "buy")));
  container.querySelectorAll('[data-action="test-sell"]').forEach(b => b.addEventListener("click", () => testTask(b, "sell")));
  container.querySelectorAll('[data-action="add-time-range"]').forEach(b => b.addEventListener("click", () => {
    const tid = b.getAttribute("data-task-id");
    updTR(tid, rs => { rs.push({start:"",end:""}); return rs; });
    renderTasks(stateTasks);
  }));
  container.querySelectorAll('[data-action="delete-time-range"]').forEach(b => b.addEventListener("click", () => {
    const tid = b.getAttribute("data-task-id"), idx = +b.getAttribute("data-index");
    updTR(tid, rs => rs.filter((_,i) => i !== idx));
    renderTasks(stateTasks);
  }));
}

async function testTask(btn, action) {
  const tid = btn.getAttribute("data-task-id");
  const symbol = prompt("Symbol:", "BTCUSDT"); if (!symbol) return;
  try { await apiPost("/api/tasks/test", { taskId: tid, action, symbol }); log(`测试 ${action} 已发送`); }
  catch(e) { log(`测试失败: ${e.message}`, "ERROR"); }
}

function promptImportPlatformToken(tid, type) {
  const s=prompt(`粘贴 ${type} 平台的 curl:`); if(!s) return;
  try {
    const p=parseCurl(s);
    const card=document.querySelector(`[data-task-card="1"][data-task-id="${cssEscape(tid)}"]`);
    if(!card) return;
    const headerObj = {};
    for(const line of (p.headers||"").split("\n")) {
      const m=/^([^:]+):\s*(.*)$/.exec(line.trim()); if(!m) continue;
      headerObj[m[1].trim().toLowerCase()] = m[2].trim();
    }

    const authField = card.querySelector('[data-field="auth"]');
    const typeField = card.querySelector('[data-field="type"]');
    let auth = {};

    if (type === "binance") {
      if (!headerObj["csrftoken"] && !headerObj["cookie"]) { alert("未解析到 csrftoken 或 Cookie"); return; }
      if (headerObj["csrftoken"]) auth.csrftoken = headerObj["csrftoken"];
      const cookie = headerObj["cookie"] || "";
      const pm = /(?:^|;\s*)p20t=([^;\s]+)/.exec(cookie);
      if (pm) auth.p20t = pm[1];
      if (!auth.p20t && headerObj["p20t"]) auth.p20t = headerObj["p20t"];
      if (!auth.csrftoken && !auth.p20t) { alert("未解析到 csrftoken 或 p20t"); return; }
    } else if (type === "hibt") {
      const token = headerObj["x-auth-token"] || headerObj["authorization"];
      if (!token) { alert("未解析到 x-auth-token 或 Authorization"); return; }
      auth["x-auth-token"] = headerObj["x-auth-token"] || token;
      if (headerObj["authorization"]) auth.authorization = headerObj["authorization"];
      const vm = /[?&]v=([^&\s]+)/.exec(p.url || "");
      if (vm) auth.v = decodeURIComponent(vm[1]);
    } else if (type === "turboflow") {
      if (!headerObj["authorization"] && !headerObj["uid"]) { alert("未解析到 authorization 或 uid"); return; }
      if (headerObj["authorization"]) auth.authorization = headerObj["authorization"];
      if (headerObj["uid"]) auth.uid = headerObj["uid"];
      if (headerObj["biz-pf"]) auth["biz-pf"] = headerObj["biz-pf"];
    }

    if (authField) authField.value = authDisplayValue({ type, auth });
    if (typeField) typeField.value = type;
    if (type === "binance") {
      const csrfField = card.querySelector('[data-field="auth-csrftoken"]');
      const p20tField = card.querySelector('[data-field="auth-p20t"]');
      if (csrfField) csrfField.value = auth.csrftoken || "";
      if (p20tField) p20tField.value = auth.p20t || "";
    }
    const st = stateTasks.find(t => t.id === tid);
    if (st) { st.auth = auth; st.type = type; }
    showToast(`${type} Token 已导入`);
  } catch(e) { alert("解析失败: "+e.message); }
}

window.promptImportPlatformToken = promptImportPlatformToken;

function parseCurl(s) {
  s=s.replace(/\\\n/g," ").replace(/\n/g," ");
  const r={url:"",method:"",headers:"",body:""};
  const um=/['"]?(https?:\/\/[^\s'"]+)['"]?/.exec(s); if(um) r.url=um[1];
  const xm=/-X\s+(\w+)/i.exec(s); if(xm) r.method=xm[1].toUpperCase();
  const hs=[],hm=/['"]?([A-Za-z0-9._-]+):\s*([^'"]+)/g; let mh;
  while((mh=hm.exec(s))!==null) hs.push(`${mh[1]}: ${mh[2].replace(/['"]$/,"")}`);
  if(hs.length) r.headers=hs.join("\n");
  const dm=/--data(?:-raw|-binary)?\s+(['"])([\s\S]*?)\1/.exec(s)||/-d\s+(['"])([\s\S]*?)\1/.exec(s);
  if(dm) r.body=dm[2];
  const bm=/-b\s+(['"])([\s\S]*?)\1/.exec(s);
  if(bm&&!/[Cc]ookie:/.test(r.headers)) r.headers=(r.headers?r.headers+"\n":"")+`Cookie: ${bm[2]}`;
  return r;
}

// ── Log ──

function log(msg, level) {
  const container = document.getElementById("log-container");
  if (!container) return;
  appendLog({ time: new Date().toISOString(), level: level||"INFO", source: "app", message: msg });
}

function initLogs() {
  const container = document.getElementById("log-container");
  const es = new EventSource("/api/logs/stream");
  es.onopen = () => { if (container) container.innerHTML = ""; };
  es.onmessage = (ev) => {
    try { const entry = JSON.parse(ev.data); appendLog(entry); }
    catch(e) { console.error("parse log entry error", e, ev.data); }
  };
  es.onerror = () => { console.debug("SSE disconnected, auto-reconnect"); };
}

function appendLog(entry) {
  const container = document.getElementById("log-container");
  if (!container) return;
  const row = document.createElement("div");
  row.className = "flex gap-2 items-start text-[11px] leading-relaxed";
  const timeStr = entry.time ? new Date(entry.time).toLocaleTimeString("zh-CN",{hour12:false}) : new Date().toLocaleTimeString("zh-CN",{hour12:false});
  const level = (entry.level||"INFO").toUpperCase();
  const source = entry.source||"app";
  const msg = entry.message||"";
  const colorClass = level==="ERROR"?"text-red-600":level==="DEBUG"?"text-blue-600":"text-green-700";
  row.innerHTML = `<span class="text-gray-400 shrink-0">${timeStr}</span><span class="shrink-0 ${colorClass}">[${level}]</span><span class="shrink-0 text-gray-400">${source}</span><span class="flex-1 whitespace-pre-wrap break-words text-gray-700">${esc(msg)}</span>`;
  container.appendChild(row);
  while (container.children.length > 500) container.removeChild(container.firstChild);
  container.scrollTop = container.scrollHeight;
}

// ── Toast ──

function showToast(msg, type) {
  const c = document.getElementById("toast-container");
  if (!c) return;
  const el = document.createElement("div");
  el.className = `pointer-events-auto rounded px-4 py-2 text-xs text-white shadow-lg opacity-0 transition-opacity duration-300 ${type==="error"?"bg-red-600":"bg-green-600"}`;
  el.textContent = msg;
  c.appendChild(el);
  requestAnimationFrame(() => el.classList.add("opacity-100"));
  setTimeout(() => { el.classList.remove("opacity-100"); setTimeout(() => el.remove(), 300); }, type==="error"?3500:2500);
}

// ── Helpers ──

function cssEscape(s) { return CSS.escape(String(s)); }
