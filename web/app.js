document.addEventListener("DOMContentLoaded", () => { initConfig(); initActions(); initLogs(); });

let stateTasks = [];

async function apiGet(p) { const r = await fetch(p); if (!r.ok) throw new Error(r.status); return r.json(); }
async function apiPost(p, b) {
  const r = await fetch(p, { method:"POST", headers:{"Content-Type":"application/json"}, body:JSON.stringify(b??{}) });
  if (!r.ok) { const m = await r.text().catch(()=>""); throw new Error(`${r.status} ${m}`); }
  try { return await r.json(); } catch { return {}; }
}

function $(id) { return document.getElementById(id); }

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
      const sel = $("dispatchMode"); if (sel) sel.value = (c.dispatch==="all"||c.dispatch==="random")?c.dispatch:"round-robin";
      stateTasks = normalizeTasks(c); renderTasks(stateTasks); log("JSON 已导入");
    } catch(e) { alert("解析失败: "+e.message); }
  });
  $("btn-add-task")?.addEventListener("click", () => { stateTasks = collectTasks(); stateTasks.push(defaultTask()); renderTasks(stateTasks); });
  $("wsServerPath")?.addEventListener("input", updateWSEndpoint);
  $("wsServerKey")?.addEventListener("input", updateWSEndpoint);
}

// ── Config ──

function collectPayload() {
  const upstream = { wsUrl: "", wsKey: "", enabled: false };
  const wsServer = { path: getVal("wsServerPath")||"/ws", key: getVal("wsServerKey")||"", enabled: isChk("wsServerEnabled"), applySkip: isChk("wsServerApplySkip") };
  const dispatch = $("dispatchMode")?.value || "round-robin";
  const tasks = collectTasks().map(n);
  validate(tasks); stateTasks = tasks;
  return { upstream, wsServer, dispatch, tasks };
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
    valueBuy: String(t.valueBuy||""), valueSell: String(t.valueSell||"") };
}

function defaultTask() { return n({ id:rid("acct"), name:"New Account", enabled:true, apiUrl:"https://www.binance.com/bapi/futures/v2/private/future/event-contract/place-order", method:"POST", headers:"Content-Type: application/json\nclienttype: web", body:'{"orderAmount":"{{amount}}","timeIncrements":"{{unit}}","symbolName":"BTCUSDT","payoutRatio":"0.80","direction":"{{action}}"}', valueBuy:"LONG", valueSell:"SHORT" }); }

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
      <div class="flex-1">
        <label class="block text-[11px] text-gray-500 mb-0.5">过期</label>
        <input type="hidden" data-field="expiresAt" value="${t.expiresAt||0}" />
        <div class="flex items-center gap-1">
          <span class="inline-flex items-center h-7 px-2 text-xs font-mono rounded border" style="background:var(--gl);color:var(--gt);border-color:var(--gb);min-width:8rem" id="expires-display-${id}">${fmtDT(t.expiresAt)}</span>
          <span class="inline-flex border rounded overflow-hidden" style="border-color:var(--gb)">
            <button class="h-7 px-2 text-xs border-r bg-white hover:bg-green-50" style="border-color:var(--gb);color:var(--gt)" onclick="setExpires('${id}',1)">+1d</button>
            <button class="h-7 px-2 text-xs border-r bg-white hover:bg-green-50" style="border-color:var(--gb);color:var(--gt)" onclick="setExpires('${id}',3)">+3d</button>
            <button class="h-7 px-2 text-xs bg-white hover:bg-green-50" style="color:var(--gt)" onclick="setExpires('${id}',7)">+7d</button>
          </span>
          <button class="h-7 px-2 text-xs border border-red-200 rounded bg-white text-red-600 hover:bg-red-50" onclick="setExpires('${id}',0)">清除</button>
        </div>
      </div>
    </div>
  </div>`;
}

function trHtml(taskId, ranges) {
  const list = ntr(ranges);
  if (!list.length) return '<div class="time-range-empty">全天</div>';
  return list.map((r,i) => `<div class="flex gap-1 items-center">
    <select class="border rounded px-1 py-0.5 text-[11px] bg-white flex-1" data-time-range-field="start" data-task-id="${taskId}" data-index="${i}">${hopts(r.start)}</select>
    <span class="text-[11px] text-gray-400">→</span>
    <select class="border rounded px-1 py-0.5 text-[11px] bg-white flex-1" data-time-range-field="end" data-task-id="${taskId}" data-index="${i}">${hopts(r.end)}</select>
    <button class="text-red-500 text-[11px] px-1" data-action="delete-time-range" data-task-id="${taskId}" data-index="${i}">✕</button></div>`).join("");
}

function hopts(sel) { let o='<option value="">--</option>'; for(let h=0;h<24;h++){const v=`${String(h).padStart(2,"0")}:00`;o+=`<option value="${v}" ${v===String(sel||"").trim()?"selected":""}>${v}</option>`;} return o; }

function bind(c) {
  c.querySelectorAll("[data-action]").forEach(el => el.addEventListener("click", async ev => {
    const b = ev.currentTarget, a = b.getAttribute("data-action"), tid = b.getAttribute("data-task-id");
    if (!tid) return;
    if (a==="delete") { stateTasks = collectTasks().filter(x=>x.id!==tid); renderTasks(stateTasks); return; }
    if (a==="add-time-range") { stateTasks = collectTasks(); try { updTR(tid, r=>r.length>=4?(()=>{throw new Error("最多4段")})():[...r,{start:"",end:""}]); } catch(e) { log(e.message,"ERROR"); return; } renderTasks(stateTasks); return; }
    if (a==="delete-time-range") { const i=+b.getAttribute("data-index")||0; stateTasks=collectTasks(); updTR(tid, r=>r.filter((_,j)=>j!==i)); renderTasks(stateTasks); return; }
    if (a==="test-buy"||a==="test-sell") { const act=a==="test-buy"?"buy":"sell"; try { await apiPost("/api/tasks/test",{taskId:tid,action:act}); log(`测试 ${act} → ${tid}`); } catch(e) { log("测试失败: "+e.message,"ERROR"); } }
  }));
  c.querySelectorAll("[data-time-range-field]").forEach(el => el.addEventListener("change", ev => {
    const inp=ev.currentTarget, tid=inp.getAttribute("data-task-id"), idx=+inp.getAttribute("data-index")||0, f=inp.getAttribute("data-time-range-field");
    if (!tid||!f) return; stateTasks=collectTasks(); updTR(tid, r=>r.map((item,i)=>i===idx?{...item,[f]:inp.value}:item));
  }));
}

// ── Time ranges ──

function ntr(r) { return Array.isArray(r)?r.map(x=>({start:String(x?.start||"").trim(),end:String(x?.end||"").trim()})):[]; }
function ctr(r) { return ntr(r).filter(x=>x.start||x.end); }
function updTR(tid, fn) { const t=stateTasks.find(x=>x.id===tid); if(t) t.timeRanges=ntr(fn(ntr(t.timeRanges))); }
function colTR(card) { return [...card.querySelectorAll("[data-time-range-field='start']")].map(s=>{const i=s.getAttribute("data-index"),e=card.querySelector(`[data-time-range-field="end"][data-index="${cssEscape(i)}"]`);return{start:String(s.value||"").trim(),end:String(e?.value||"").trim()}}); }

// ── Collect ──

function collectTasks() {
  return [...document.querySelectorAll('[data-task-card="1"]')].map(card => {
    const id=card.getAttribute("data-task-id")||rid("acct");
    const g=f=>{const e=card.querySelector(`[data-field="${f}"]`);return e?e.value:""};
    const gc=f=>{const e=card.querySelector(`[data-field="${f}"]`);return!!(e&&e.checked)};
    return { id, name: String(g("name")||"").trim()||id, enabled: gc("enabled"), skipSignals:+g("skipSignals")||0,
      timeRanges: colTR(card), allowedSymbols: String(g("allowedSymbols")||"").trim(), expiresAt:+g("expiresAt")||0,
      httpProxyUrl: String(g("httpProxyUrl")||"").trim(), apiUrl: String(g("apiUrl")||"").trim(),
      method: String(g("method")||"POST").trim().toUpperCase(), headers: String(g("headers")||""), body: String(g("body")||""),
      valueBuy: String(g("valueBuy")||"").trim(), valueSell: String(g("valueSell")||"").trim() };
  });
}

function validate(ts) { ts.forEach(t=>{ if(!t.apiUrl) throw new Error(`账号[${t.name}] API URL 为空`); vtr(t); }); }
function vtr(t) { const r=ctr(t.timeRanges); if(r.length>4) throw new Error(`账号[${t.name}] 最多4段`);
  r.forEach((x,i)=>{ const l=`账号[${t.name}] 时段#${i+1}`; if(!x.start||!x.end) throw new Error(`${l} 起止必填`);
    const sm=pm(x.start,l), em=pm(x.end,l); if(sm===em) throw new Error(`${l} 起止不能相同`); }); }
function pm(v,l) { if(!/^\d{2}:\d{2}$/.test(v)) throw new Error(`${l} 需HH:00`); const [h,m]=v.split(":").map(Number); if(h<0||h>23||m!==0) throw new Error(`${l} 仅整点`); return h*60+m; }

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

// ── cURL ──

function promptImportCurl(tid) {
  const s=prompt("粘贴 curl:");if(!s)return;
  try{const p=parseCurl(s);const c=document.querySelector(`[data-task-card="1"][data-task-id="${cssEscape(tid)}"]`);if(!c)return;
    const set=(f,v)=>{const e=c.querySelector(`[data-field="${f}"]`);if(e)e.value=v??"";};
    if(p.url)set("apiUrl",p.url);if(p.method)set("method",p.method);if(p.headers)set("headers",p.headers);if(p.body)set("body",p.body);
    log(`cURL → ${tid}`);}catch(e){alert("解析失败: "+e.message);}
}
function parseCurl(s){
  const r={method:"GET",url:"",headers:"",body:""};s=s.replace(/\\\r?\n/g," ");
  const u=s.match(/https?:\/\/[^\s'"]+/i);if(u)r.url=u[0].replace(/`/g,"");
  const m=s.match(/(?:-X|--request)\s+['"]?([A-Za-z]+)['"]?/);if(m)r.method=m[1].toUpperCase();
  let hs=[];const hr=/(?:-H|--header)\s+('([^'\\]*(?:\\.[^'\\]*)*)'|"([^"\\]*(?:\\.[^'\\]*)*)")/gi;let h;
  while((h=hr.exec(s))!==null) hs.push((h[2]||h[3]||"").replace(/`/g,"").trim());
  const cr=/(?:-b|--cookie)\s+('([^'\\]*(?:\\.[^'\\]*)*)'|"([^"\\]*(?:\\.[^'\\]*)*)")/gi;
  while((h=cr.exec(s))!==null){const c=(h[2]||h[3]||"").replace(/`/g,"").trim();if(c)hs.push("Cookie: "+c);}
  r.headers=hs.join("\n");
  const br=/(?:-d|--data|--data-raw|--data-binary)\s+('([^'\\]*(?:\\.[^'\\]*)*)'|"([^"\\]*(?:\\.[^'\\]*)*)")/i;
  const bm=s.match(br);if(bm){r.body=bm[2]||bm[3]||"";if(!m)r.method="POST";}
  return r;
}

// ── Logs ──

function initLogs() {
  const c=$("log-container");const es=new EventSource("/api/logs/stream");
  es.onopen=()=>{if(c)c.innerHTML="";};
  es.onmessage=ev=>{try{appendLog(JSON.parse(ev.data));}catch(e){console.error(e);}};
  es.onerror=()=>{};
}
function appendLog(e) {
  const c=$("log-container");if(!c)return;
  const r=document.createElement("div");r.className="flex gap-2 items-start text-[11px] leading-relaxed";
  const t=e.time?new Date(e.time).toLocaleTimeString("zh-CN",{hour12:false}):new Date().toLocaleTimeString("zh-CN",{hour12:false});
  const l=(e.level||"INFO").toUpperCase(),s=e.source||"",m=e.message||"";
  const cl=l==="ERROR"?"text-red-600":l==="DEBUG"?"text-blue-600":"text-green-700";
  r.innerHTML=`<span class="text-gray-400 shrink-0">${t}</span><span class="shrink-0 ${cl}">[${l}]</span><span class="shrink-0 text-gray-400">${esc(s)}</span><span class="flex-1 whitespace-pre-wrap break-words text-gray-700">${esc(m)}</span>`;
  c.appendChild(r);while(c.children.length>500)c.removeChild(c.firstChild);c.scrollTop=c.scrollHeight;
}
function log(msg, lv) { appendLog({ time: new Date().toISOString(), level: lv||"INFO", source: "ui", message: msg }); }

// ── Helpers ──

function rid(p) { try{return crypto?.randomUUID?.()||`${p}-${Date.now()}-${Math.floor(Math.random()*1e6)}`;}catch{return p+"-"+Date.now();} }
function cssEscape(s) { return String(s).replace(/"/g,'\\"'); }
function esc(s) { return String(s).replace(/&/g,"&amp;").replace(/</g,"&lt;").replace(/>/g,"&gt;").replace(/"/g,"&quot;"); }
function setVal(id,v) { const e=$(id); if(e) e.value = v??""; }
function getVal(id) { const e=$(id); return e?e.value:""; }
function setChk(id,v) { const e=$(id); if(e) e.checked=!!v; }
function isChk(id) { const e=$(id); return !!e?.checked; }
