document.addEventListener("DOMContentLoaded", () => { initConfig(); initActions(); initLogs(); });

let stateTasks = [];
let stateUpstreams = [];

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
}

function initActions() {
  $("btn-save-config")?.addEventListener("click", async () => {
    try { await apiPost("/api/config", collectPayload()); log("配置已保存"); await loadConfig(); }
    catch(e) { log("保存失败: "+e.message, "ERROR"); }
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
      stateTasks = normalizeTasks(c); renderTasks(stateTasks); log("JSON 已导入");
    } catch(e) { alert("解析失败: "+e.message); }
  });
  $("btn-add-task")?.addEventListener("click", () => { stateTasks = collectTasks(); stateTasks.push(defaultTask()); renderTasks(stateTasks); });
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
  return { upstreams, wsServer, dispatch, tasks };
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
  return { id: String(t.id||"").trim()||rid("acct"), name: String(t.name||"").trim()||"Account", enabled: t.enabled!==false,
    skipSignals: +t.skipSignals||0, timeRanges: ctr(t.timeRanges), allowedSymbols: String(t.allowedSymbols||""),
    expiresAt: +t.expiresAt||0, httpProxyUrl: String(t.httpProxyUrl||""), apiUrl: String(t.apiUrl||""),
    method: String(t.method||"POST").toUpperCase(), headers: String(t.headers||""), body: String(t.body||""),
    valueBuy: String(t.valueBuy||""), valueSell: String(t.valueSell||""),
    minProba: parseFloat(t.minProba)||0 };
}

function defaultTask() { return n({ id:rid("acct"), name:"New Account", enabled:true, apiUrl:"https://www.binance.com/bapi/futures/v2/private/future/event-contract/place-order", method:"POST", headers:"Content-Type: application/json\nclienttype: web", body:'{"orderAmount":"{{amount}}","timeIncrements":"{{unit}}","symbolName":"BTCUSDT","payoutRatio":"0.80","direction":"{{action}}"}', valueBuy:"LONG", valueSell:"SHORT" }); }

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

function card(t, idx) {
  const id = t.id;
  return `<div class="bg-white border rounded-lg p-3" data-task-card="1" data-task-id="${id}">
    <div class="flex items-center justify-between gap-2 pb-2 mb-2 border-b" style="border-color:var(--gl)">
      <div class="flex items-center gap-2 min-w-0">
        <span class="inline-flex items-center justify-center w-6 h-6 text-[11px] font-bold rounded" style="background:var(--gl);color:var(--gt)">#${idx}</span>
        <input class="border rounded px-2 py-1 text-xs font-semibold" style="max-width:9rem" data-field="name" value="${esc(t.name)}" />
        <span id="countdown-${id}" class="countdown hidden"></span>
        <label class="switch-label"><span class="switch"><input type="checkbox" data-field="enabled" ${t.enabled?"checked":""} /><span class="switch-track"></span></span></label>
      </div>
      <div class="flex items-center gap-1 flex-shrink-0">
        <button class="rounded px-2 py-1 text-[11px] font-medium text-white" style="background:var(--g)" data-action="test-buy" data-task-id="${id}">BUY</button>
        <button class="border rounded px-2 py-1 text-[11px] bg-white" data-action="test-sell" data-task-id="${id}">SELL</button>
        <button class="border rounded px-2 py-1 text-[11px] bg-white" onclick="promptImportCurl('${id}')">cURL</button>
        <button class="border border-red-200 rounded px-2 py-1 text-[11px] bg-white text-red-600 hover:bg-red-50" data-action="delete" data-task-id="${id}">✕</button>
      </div>
    </div>
    <div class="flex gap-2 items-end">
      <div class="flex-1"><label class="block text-[11px] text-gray-500 mb-0.5">API URL</label><input class="border rounded w-full px-2 py-1 text-xs" data-field="apiUrl" value="${esc(t.apiUrl)}" placeholder="https://..." /></div>
      <div style="width:5rem"><label class="block text-[11px] text-gray-500 mb-0.5">Method</label><select class="border rounded w-full px-2 py-1 text-xs bg-white" data-field="method">${["GET","POST","PUT","DELETE"].map(m=>`<option ${t.method===m?"selected":""}>${m}</option>`).join("")}</select></div>
      <div style="width:7rem"><label class="block text-[11px] text-gray-500 mb-0.5">代理</label><input class="border rounded w-full px-2 py-1 text-xs" data-field="httpProxyUrl" value="${esc(t.httpProxyUrl)}" placeholder="可选" /></div>
    </div>
    <div class="flex gap-2 items-start mt-2">
      <div class="flex-1">
        <label class="block text-[11px] text-gray-500 mb-0.5">⏰ 有效时间段 <span class="text-gray-400">留空=全天</span></label>
        <div class="space-y-1" data-time-ranges="1" data-task-id="${id}">${trHtml(id, t.timeRanges)}</div>
        <button class="border rounded px-2 py-0.5 text-[10px] bg-white mt-1" data-action="add-time-range" data-task-id="${id}">+ 时段</button>
      </div>
      <div style="width:9rem"><label class="block text-[11px] text-gray-500 mb-0.5">Symbol过滤</label><input class="border rounded w-full px-2 py-1 text-xs" data-field="allowedSymbols" value="${esc(t.allowedSymbols)}" placeholder="BTCUSDT,ETHUSDT" /></div>
    </div>
    <div class="mt-1.5">
      <span class="text-[11px] text-gray-500 cursor-pointer select-none" onclick="this.nextElementSibling.classList.toggle('hidden')">▸ Headers & Body</span>
      <div class="hidden grid gap-2 sm:grid-cols-2 mt-1">
        <div><textarea class="border rounded w-full px-2 py-1 text-[11px] font-mono" rows="3" data-field="headers" placeholder="Key: Value">${esc(t.headers)}</textarea></div>
        <div><textarea class="border rounded w-full px-2 py-1 text-[11px] font-mono" rows="3" data-field="body" placeholder='{"key":"{{action}}"}'>${esc(t.body)}</textarea></div>
      </div>
    </div>
    <div class="flex gap-2 items-end mt-2">
      <div style="width:5rem"><label class="block text-[11px] text-gray-500 mb-0.5">buy→</label><input class="border rounded w-full px-2 py-1 text-xs" data-field="valueBuy" value="${esc(t.valueBuy)}" placeholder="LONG" /></div>
      <div style="width:5rem"><label class="block text-[11px] text-gray-500 mb-0.5">sell→</label><input class="border rounded w-full px-2 py-1 text-xs" data-field="valueSell" value="${esc(t.valueSell)}" placeholder="SHORT" /></div>
      <div style="width:4rem"><label class="block text-[11px] text-gray-500 mb-0.5">跳过</label><input type="number" min="0" class="border rounded w-full px-2 py-1 text-xs" data-field="skipSignals" value="${t.skipSignals||0}" /></div>
      <div style="width:5rem"><label class="block text-[11px] text-gray-500 mb-0.5">proba≥</label><input type="number" step="0.01" min="0" max="1" class="border rounded w-full px-2 py-1 text-xs" data-field="minProba" value="${t.minProba||0}" placeholder="0" /></div>
      <div class="flex-1">
        <label class="block text-[11px] text-gray-500 mb-0.5">过期</label>
        <input type="hidden" data-field="expiresAt" value="${t.expiresAt||0}" />
        <div class="flex items-center gap-1">
          <button class="border rounded px-2 py-0.5 text-[10px] bg-white" onclick="setExpires('${id}',1)">1d</button>
          <button class="border rounded px-2 py-0.5 text-[10px] bg-white" onclick="setExpires('${id}',7)">7d</button>
          <button class="border rounded px-2 py-0.5 text-[10px] bg-white" onclick="setExpires('${id}',30)">30d</button>
          <button class="border rounded px-2 py-0.5 text-[10px] bg-white border-red-200 text-red-600" onclick="setExpires('${id}',0)">清除</button>
        </div>
        <span id="expires-display-${id}" class="text-[10px] text-gray-400">${fmtDT(t.expiresAt)}</span>
      </div>
    </div>
  </div>`;
}

// ── Time ranges ──

function ntr(r) { return Array.isArray(r)?r.map(x=>({start:String(x?.start||"").trim(),end:String(x?.end||"").trim()})):[]; }
function ctr(r) { return ntr(r).filter(x=>x.start||x.end); }
function updTR(tid, fn) { const t=stateTasks.find(x=>x.id===tid); if(t) t.timeRanges=ntr(fn(ntr(t.timeRanges))); }
function colTR(card) { return [...card.querySelectorAll("[data-time-range-field='start']")].map(s=>{const i=s.getAttribute("data-index"),e=card.querySelector(`[data-time-range-field=\"end\"][data-index=\"${cssEscape(i)}\"]`);return{start:String(s.value||"").trim(),end:String(e?.value||"").trim()}}); }

// ── Collect ──

function collectTasks() {
  return [...document.querySelectorAll('[data-task-card="1"]')].map(card => {
    const id=card.getAttribute("data-task-id")||rid("acct");
    const g=f=>{const e=card.querySelector(`[data-field="${f}"]`);return e?e.value:""};
    const gc=f=>{const e=card.querySelector(`[data-field="${f}"]`);return !!(e&&e.checked)};
    return { id, name: String(g("name")||"").trim()||id, enabled: gc("enabled"), skipSignals:+g("skipSignals")||0,
      timeRanges: colTR(card), allowedSymbols: String(g("allowedSymbols")||"").trim(), expiresAt:+g("expiresAt")||0,
      httpProxyUrl: String(g("httpProxyUrl")||"").trim(), apiUrl: String(g("apiUrl")||"").trim(),
      method: String(g("method")||"POST").trim().toUpperCase(), headers: String(g("headers")||""), body: String(g("body")||""),
      valueBuy: String(g("valueBuy")||"").trim(), valueSell: String(g("valueSell")||"").trim(),
      minProba: parseFloat(g("minProba"))||0 };
  });
}

function validate(ts) { ts.forEach(t=>{ if(!t.apiUrl) throw new Error(`账号[${t.name}] API URL 为空`); vtr(t); }); }
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
  if (!rs.length) return '<div class="time-range-empty">全天</div>';
  return rs.map((r,i) => `<div class="flex items-center gap-1">
    <input class="border rounded px-2 py-1 text-[11px]" style="width:5rem" data-time-range-field="start" data-index="${i}" value="${esc(r.start)}" placeholder="09:00" />
    <span class="text-[11px] text-gray-500">→</span>
    <input class="border rounded px-2 py-1 text-[11px]" style="width:5rem" data-time-range-field="end" data-index="${i}" value="${esc(r.end)}" placeholder="17:00" />
    <button class="border rounded px-1.5 py-0.5 text-[10px] bg-white text-red-600" data-action="delete-time-range" data-task-id="${tid}" data-index="${i}">✕</button>
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

// ── cURL ──

function promptImportCurl(tid) {
  const s=prompt("粘贴 curl:");if(!s)return;
  try{const p=parseCurl(s);const c=document.querySelector(`[data-task-card="1"][data-task-id="${cssEscape(tid)}"]`);if(!c)return;
    const set=(f,v)=>{const e=c.querySelector(`[data-field="${f}"]`);if(e)e.value=v??"";};
    if(p.url)set("apiUrl",p.url);if(p.method)set("method",p.method);if(p.headers)set("headers",p.headers);if(p.body)set("body",p.body);
    log("cURL 已导入");}catch(e){alert("解析失败: "+e.message);}
}

function parseCurl(s) {
  s=s.replace(/\\\n/g," ").replace(/\n/g," ");
  const r={url:"",method:"GET",headers:"",body:""};
  const um=/['"]?(https?:\/\/[^\s'"]+)['"]?/.exec(s); if(um) r.url=um[1];
  const xm=/-X\s+(\w+)/i.exec(s); if(xm) r.method=xm[1].toUpperCase();
  const hs=[],hm=/['"]?([A-Za-z0-9._-]+):\s*([^'"]+)/g; let mh;
  while((mh=hm.exec(s))!==null) hs.push(`${mh[1]}: ${mh[2].replace(/['"]$/,"")}`);
  if(hs.length) r.headers=hs.join("\n");
  const dm=/--data(?:-raw|-binary)?\s+['"]([^'"]+)['"]/.exec(s)||/-d\s+['"]([^'"]+)['"]/.exec(s);
  if(dm) r.body=dm[1]||dm[2];
  const cm=/['"]?([A-Za-z._-]+=[^;]+)/.exec(s); if(cm&&r.headers.indexOf("Cookie")<0) r.headers=(r.headers?r.headers+"\n":"")+`Cookie: ${cm[1]}`;
  return r;
}

// ── Log ──

function initLogs() {
  const src = new EventSource("/api/logs/stream");
  src.onmessage = e => {
    try { const d=JSON.parse(e.data); log(`${d.level||"INFO"} ${d.tag||""} ${d.msg||""}`, d.level); }
    catch { log(e.data, "INFO"); }
  };
  src.onerror = () => { /* retry built-in */ };
}

function log(msg, level) {
  const c = $("log-container"); if (!c) return;
  const t = new Date().toLocaleTimeString();
  const cls = level==="ERROR"?"text-red-600":level==="WARNING"?"text-amber-600":"text-gray-700";
  const div = document.createElement("div");
  div.className = `py-0.5 ${cls}`;
  div.textContent = `[${t}] ${msg}`;
  c.appendChild(div);
  c.scrollTop = c.scrollHeight;
  // Keep max 500 entries
  while (c.children.length > 500) c.firstChild.remove();
}

// ── Helpers ──

function cssEscape(s) { return CSS.escape(String(s)); }
