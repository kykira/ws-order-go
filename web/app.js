document.addEventListener("DOMContentLoaded", () => {
  initConfig();
  initActions();
  initLogs();
});

let stateTasks = [];

async function apiGet(path) {
  const res = await fetch(path);
  if (!res.ok) throw new Error(`GET ${path} ${res.status}`);
  return res.json();
}

async function apiPost(path, body) {
  const res = await fetch(path, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body ?? {}),
  });
  if (!res.ok) {
    let msg = await res.text().catch(() => "");
    throw new Error(`POST ${path} ${res.status} ${msg}`);
  }
  try { return await res.json(); } catch { return {}; }
}

// ── Init ──────────────────────────────────────────

function initConfig() {
  loadConfig().catch(err => console.error(err));
  updateWSStatus();
  setInterval(updateWSStatus, 3000);
}

async function loadConfig() {
  const cfg = await apiGet("/api/config");

  // WS Server
  const ws = cfg.wsServer || {};
  setValue("wsServerPath", ws.path || "/ws");
  setValue("wsServerKey", ws.key || "");
  setChecked("wsServerEnabled", ws.enabled !== false);
  setChecked("wsServerApplySkip", !!ws.applySkip);
  updateWSEndpoint();

  // Upstream (legacy)
  setValue("wsUrl", cfg.upstream?.wsUrl || "");
  setValue("wsKey", cfg.upstream?.wsKey || "");
  setChecked("wsEnabled", !!cfg.upstream?.enabled);

  // Dispatch
  const sel = document.getElementById("dispatchMode");
  if (sel) sel.value = (cfg.dispatch === "all" || cfg.dispatch === "random") ? cfg.dispatch : "round-robin";

  stateTasks = normalizeTasks(cfg);
  renderTasks(stateTasks);
}

function initActions() {
  document.getElementById("btn-save-config")?.addEventListener("click", async () => {
    try {
      const payload = collectConfigPayload();
      await apiPost("/api/config", payload);
      appendLog({ time: new Date().toISOString(), level: "INFO", source: "ui", message: "配置已保存" });
      await loadConfig();
    } catch (err) {
      appendLog({ time: new Date().toISOString(), level: "ERROR", source: "ui", message: `保存失败: ${err.message}` });
    }
  });

  document.getElementById("btn-import-json")?.addEventListener("click", () => {
    const s = prompt("粘贴 config.json 内容：");
    if (!s) return;
    try {
      const cfg = JSON.parse(s);
      const ws = cfg.wsServer || {};
      setValue("wsServerPath", ws.path || "/ws");
      setValue("wsServerKey", ws.key || "");
      setChecked("wsServerEnabled", ws.enabled !== false);
      setChecked("wsServerApplySkip", !!ws.applySkip);
      updateWSEndpoint();
      setValue("wsUrl", cfg.upstream?.wsUrl || "");
      setValue("wsKey", cfg.upstream?.wsKey || "");
      setChecked("wsEnabled", !!cfg.upstream?.enabled);
      const sel = document.getElementById("dispatchMode");
      if (sel) sel.value = (cfg.dispatch === "all" || cfg.dispatch === "random") ? cfg.dispatch : "round-robin";
      stateTasks = normalizeTasks(cfg);
      renderTasks(stateTasks);
      appendLog({ level: "INFO", source: "ui", message: "JSON 已导入（未保存）" });
    } catch (e) { alert("JSON 解析失败: " + e.message); }
  });

  document.getElementById("btn-add-task")?.addEventListener("click", () => {
    stateTasks = collectTasksFromDom();
    stateTasks.push(buildDefaultTask());
    renderTasks(stateTasks);
  });

  // WS Server field listeners
  document.getElementById("wsServerPath")?.addEventListener("input", updateWSEndpoint);
  document.getElementById("wsServerKey")?.addEventListener("input", updateWSEndpoint);
}

// ── Config collect ───────────────────────────────

function collectConfigPayload() {
  const upstream = {
    wsUrl: getValue("wsUrl"), wsKey: getValue("wsKey"), enabled: isChecked("wsEnabled"),
  };
  const wsServer = {
    path: getValue("wsServerPath") || "/ws",
    key: getValue("wsServerKey") || "",
    enabled: isChecked("wsServerEnabled"),
    applySkip: isChecked("wsServerApplySkip"),
  };
  const el = document.getElementById("dispatchMode");
  const dispatch = el ? el.value : "round-robin";
  const tasks = collectTasksFromDom().map(normalizeTask);
  validateTasks(tasks);
  stateTasks = tasks;
  return { upstream, wsServer, dispatch, tasks };
}

function normalizeTasks(cfg) {
  const tasks = Array.isArray(cfg?.tasks) ? cfg.tasks : [];
  if (tasks.length > 0) return tasks.map(normalizeTask);
  return [normalizeTask({
    id: "default", name: "Account 1", enabled: true, skipSignals: 0, httpProxyUrl: "",
    apiUrl: "https://www.binance.com/bapi/futures/v2/private/future/event-contract/place-order",
    method: "POST",
    headers: "Content-Type: application/json\nclienttype: web",
    body: '{"orderAmount":"{{amount}}","timeIncrements":"{{unit}}","symbolName":"BTCUSDT","payoutRatio":"0.80","direction":"{{action}}"}',
    valueBuy: "LONG", valueSell: "SHORT",
  })];
}

function normalizeTask(t) {
  t = t || {};
  return {
    id: String(t.id || "").trim() || randomId("acct"),
    name: String(t.name || "").trim() || "Account",
    enabled: t.enabled !== false,
    skipSignals: Number(t.skipSignals || 0) || 0,
    timeRanges: compactTimeRanges(t.timeRanges),
    allowedSymbols: String(t.allowedSymbols || ""),
    expiresAt: Number(t.expiresAt || 0) || 0,
    httpProxyUrl: String(t.httpProxyUrl || ""),
    apiUrl: String(t.apiUrl || ""),
    method: String(t.method || "POST").toUpperCase(),
    headers: String(t.headers || ""),
    body: String(t.body || ""),
    valueBuy: String(t.valueBuy || ""),
    valueSell: String(t.valueSell || ""),
  };
}

function buildDefaultTask() {
  return normalizeTask({
    id: randomId("acct"), name: "New Account", enabled: true,
    apiUrl: "https://www.binance.com/bapi/futures/v2/private/future/event-contract/place-order",
    method: "POST",
    headers: "Content-Type: application/json\nclienttype: web",
    body: '{"orderAmount":"{{amount}}","timeIncrements":"{{unit}}","symbolName":"BTCUSDT","payoutRatio":"0.80","direction":"{{action}}"}',
    valueBuy: "LONG", valueSell: "SHORT",
  });
}

// ── Render ────────────────────────────────────────

function renderTasks(tasks) {
  const c = document.getElementById("tasks-container");
  if (!c) return;
  if (!Array.isArray(tasks) || tasks.length === 0) {
    c.innerHTML = '<div class="card text-center text-xs text-gray-400 py-6">暂无账号，点击"+ 账号"添加</div>';
    return;
  }
  c.innerHTML = tasks.map((t, i) => accountCard(t, i + 1)).join("\n");
  initCountdowns();
  bindCardEvents(c);
}

function accountCard(t, index) {
  const id = t.id;
  return `
  <div class="acct-card" data-task-card="1" data-task-id="${id}">
    <!-- Header -->
    <div class="acct-header">
      <div class="flex items-center gap-2 min-w-0">
        <span class="acct-index">#${index}</span>
        <input class="input" style="max-width:10rem;font-weight:600" data-field="name" value="${esc(t.name)}" placeholder="账号名称" />
        <span class="text-[10px] font-mono text-gray-300 truncate hidden sm:inline">${id}</span>
        <label class="switch-label" title="启用">
          <span class="switch"><input type="checkbox" class="switch-input" data-field="enabled" ${t.enabled?"checked":""} /><span class="switch-track"></span></span>
        </label>
      </div>
      <div class="flex items-center gap-1 flex-shrink-0">
        <button class="btn btn-primary text-[11px]" data-action="test-buy" data-task-id="${id}">BUY</button>
        <button class="btn btn-ghost text-[11px]" data-action="test-sell" data-task-id="${id}">SELL</button>
        <button class="btn btn-ghost text-[11px]" onclick="promptImportCurl('${id}')">cURL</button>
        <button class="btn btn-danger text-[11px]" data-action="delete" data-task-id="${id}">✕</button>
      </div>
    </div>

    <!-- Row 1: API URL + Method -->
    <div class="acct-row">
      <div class="flex-1">
        <label class="label">API URL</label>
        <input class="input" data-field="apiUrl" value="${esc(t.apiUrl)}" placeholder="https://..." />
      </div>
      <div style="width:5rem">
        <label class="label">Method</label>
        <select class="input" data-field="method">
          ${["GET","POST","PUT","DELETE"].map(m => `<option ${t.method===m?"selected":""}>${m}</option>`).join("")}
        </select>
      </div>
      <div style="width:8rem">
        <label class="label">代理</label>
        <input class="input" data-field="httpProxyUrl" value="${esc(t.httpProxyUrl)}" placeholder="可选" />
      </div>
    </div>

    <!-- Row 2: Time Ranges (highlighted) -->
    <div class="acct-row items-start">
      <div class="flex-1">
        <label class="label">⏰ 有效时间段 <span class="tag tag-muted">留空=全天</span></label>
        <div class="time-ranges" data-time-ranges="1" data-task-id="${id}">
          ${renderTimeRanges(id, t.timeRanges)}
        </div>
        <button class="btn btn-ghost text-[10px] mt-1" data-action="add-time-range" data-task-id="${id}">+ 时段</button>
      </div>
      <div style="width:10rem">
        <label class="label">Symbol过滤</label>
        <input class="input" data-field="allowedSymbols" value="${esc(t.allowedSymbols)}" placeholder="BTCUSDT,ETHUSDT" />
      </div>
    </div>

    <!-- Row 3: Headers + Body (collapsible by default if empty) -->
    <details class="acct-details" ${t.headers || t.body ? "open" : ""}>
      <summary class="text-[11px] text-gray-500 cursor-pointer select-none">Headers & Body</summary>
      <div class="grid gap-2 sm:grid-cols-2 mt-1.5">
        <div>
          <label class="label">Headers <span class="tag tag-muted">一行一个</span></label>
          <textarea class="input" rows="3" data-field="headers" placeholder="Key: Value">${esc(t.headers)}</textarea>
        </div>
        <div>
          <label class="label">Body <span class="tag tag-muted">{{action}} {{amount}} {{unit}} {{symbol}}</span></label>
          <textarea class="input" rows="3" data-field="body" placeholder='{"key":"{{action}}"}'>${esc(t.body)}</textarea>
        </div>
      </div>
    </details>

    <!-- Row 4: valueBuy/Sell + skip + expires -->
    <div class="acct-row">
      <div style="width:6rem">
        <label class="label">buy→</label>
        <input class="input" data-field="valueBuy" value="${esc(t.valueBuy)}" placeholder="LONG" />
      </div>
      <div style="width:6rem">
        <label class="label">sell→</label>
        <input class="input" data-field="valueSell" value="${esc(t.valueSell)}" placeholder="SHORT" />
      </div>
      <div style="width:5rem">
        <label class="label">跳过N次</label>
        <input type="number" min="0" class="input" data-field="skipSignals" value="${t.skipSignals||0}" />
      </div>
      <div class="flex-1">
        <label class="label">过期提醒</label>
        <input type="hidden" data-field="expiresAt" value="${t.expiresAt||0}" />
        <div class="expires-row">
          <span class="expires-display" id="expires-display-${id}">${fmtDT(t.expiresAt)}</span>
          <div class="expires-btns">
            <button class="expires-btn" onclick="setExpiresDays('${id}',1)">+1d</button>
            <button class="expires-btn" onclick="setExpiresDays('${id}',3)">+3d</button>
            <button class="expires-btn" onclick="setExpiresDays('${id}',7)">+7d</button>
          </div>
          <button class="expires-clear" onclick="setExpiresDays('${id}',0)">清除</button>
        </div>
      </div>
    </div>
  </div>`;
}

function bindCardEvents(container) {
  container.querySelectorAll("[data-action]").forEach(el => {
    el.addEventListener("click", async ev => {
      const btn = ev.currentTarget;
      const action = btn.getAttribute("data-action");
      const taskId = btn.getAttribute("data-task-id");
      if (!taskId) return;

      if (action === "delete") {
        stateTasks = collectTasksFromDom().filter(x => x.id !== taskId);
        renderTasks(stateTasks);
        return;
      }
      if (action === "add-time-range") {
        stateTasks = collectTasksFromDom();
        try { updateTaskTimeRanges(taskId, r => r.length>=4 ? (()=>{throw new Error("最多4段")})() : [...r,{start:"",end:""}]); }
        catch(e) { appendLog({level:"ERROR",source:"ui",message:e.message}); return; }
        renderTasks(stateTasks);
        return;
      }
      if (action === "delete-time-range") {
        const idx = Number(btn.getAttribute("data-index")||-1);
        stateTasks = collectTasksFromDom();
        updateTaskTimeRanges(taskId, r => r.filter((_,i) => i!==idx));
        renderTasks(stateTasks);
        return;
      }
      if (action === "test-buy" || action === "test-sell") {
        const act = action === "test-buy" ? "buy" : "sell";
        try {
          await apiPost("/api/tasks/test", { taskId, action: act });
          appendLog({ level:"INFO", source:"ui", message:`测试 ${act} → ${taskId}` });
        } catch(e) {
          appendLog({ level:"ERROR", source:"ui", message:`测试失败: ${e.message}` });
        }
        return;
      }
    });
  });

  container.querySelectorAll("[data-time-range-field]").forEach(el => {
    el.addEventListener("change", ev => {
      const inp = ev.currentTarget;
      const tid = inp.getAttribute("data-task-id");
      const idx = Number(inp.getAttribute("data-index")||-1);
      const field = inp.getAttribute("data-time-range-field");
      if (!tid || idx<0 || !field) return;
      stateTasks = collectTasksFromDom();
      updateTaskTimeRanges(tid, r => r.map((item,i) => i===idx ? {...item,[field]:inp.value} : item));
    });
  });
}

// ── Time ranges ───────────────────────────────────

function renderTimeRanges(taskId, ranges) {
  const list = normalizeTimeRanges(ranges);
  if (list.length === 0) return '<div class="time-range-empty">全天</div>';
  return list.map((item, i) => `
    <div class="time-range-row">
      <select class="input" data-time-range-field="start" data-task-id="${taskId}" data-index="${i}">${hourOpts(item.start)}</select>
      <span class="time-range-sep">→</span>
      <select class="input" data-time-range-field="end" data-task-id="${taskId}" data-index="${i}">${hourOpts(item.end)}</select>
      <button class="btn btn-danger text-[10px]" data-action="delete-time-range" data-task-id="${taskId}" data-index="${i}">✕</button>
    </div>`).join("");
}

function hourOpts(sel) {
  const v = String(sel||"").trim();
  let o = '<option value="">--</option>';
  for (let h=0; h<24; h++) {
    const val = `${String(h).padStart(2,"0")}:00`;
    o += `<option value="${val}" ${val===v?"selected":""}>${val}</option>`;
  }
  return o;
}

function normalizeTimeRanges(ranges) {
  if (!Array.isArray(ranges)) return [];
  return ranges.map(r => ({ start: String(r?.start||"").trim(), end: String(r?.end||"").trim() }));
}

function compactTimeRanges(ranges) {
  return normalizeTimeRanges(ranges).filter(r => r.start || r.end);
}

function collectTimeRangesFromCard(card) {
  const rows = card.querySelectorAll("[data-time-range-field='start']");
  const out = [];
  rows.forEach(s => {
    const i = s.getAttribute("data-index");
    const e = card.querySelector(`[data-time-range-field="end"][data-index="${cssEscape(i)}"]`);
    out.push({ start: String(s.value||"").trim(), end: String(e?.value||"").trim() });
  });
  return out;
}

function updateTaskTimeRanges(taskId, updater) {
  const t = stateTasks.find(x => x.id === taskId);
  if (!t) return;
  t.timeRanges = normalizeTimeRanges(updater(normalizeTimeRanges(t.timeRanges)));
}

// ── Collect from DOM ──────────────────────────────

function collectTasksFromDom() {
  return [...document.querySelectorAll('[data-task-card="1"]')].map(card => {
    const id = card.getAttribute("data-task-id") || randomId("acct");
    const g = f => { const e = card.querySelector(`[data-field="${f}"]`); return e ? e.value : ""; };
    const gc = f => { const e = card.querySelector(`[data-field="${f}"]`); return !!(e && e.checked); };
    let exp = Number(g("expiresAt")||0)||0;
    return {
      id, name: String(g("name")||"").trim()||id,
      enabled: gc("enabled"),
      skipSignals: Number(g("skipSignals")||0)||0,
      timeRanges: collectTimeRangesFromCard(card),
      allowedSymbols: String(g("allowedSymbols")||"").trim(),
      expiresAt: exp,
      httpProxyUrl: String(g("httpProxyUrl")||"").trim(),
      apiUrl: String(g("apiUrl")||"").trim(),
      method: String(g("method")||"POST").trim().toUpperCase(),
      headers: String(g("headers")||""),
      body: String(g("body")||""),
      valueBuy: String(g("valueBuy")||"").trim(),
      valueSell: String(g("valueSell")||"").trim(),
    };
  });
}

// ── Validation ────────────────────────────────────

function validateTasks(tasks) {
  tasks.forEach(t => {
    if (!t.apiUrl) throw new Error(`账号[${t.name}] API URL 为空`);
    validateTimeRanges(t);
  });
}

function validateTimeRanges(task) {
  const r = compactTimeRanges(task.timeRanges);
  if (r.length > 4) throw new Error(`账号[${task.name}] 最多4个时间段`);
  r.forEach((range, i) => {
    const lbl = `账号[${task.name}] 时段#${i+1}`;
    if (!range.start || !range.end) throw new Error(`${lbl} 起止时间必填`);
    const sm = parseMin(range.start, lbl), em = parseMin(range.end, lbl);
    if (sm === em) throw new Error(`${lbl} 起止时间不能相同`);
  });
}

function parseMin(v, lbl) {
  if (!/^\d{2}:\d{2}$/.test(v)) throw new Error(`${lbl} 需HH:00格式`);
  const [h,m] = v.split(":").map(Number);
  if (h<0||h>23||m!==0) throw new Error(`${lbl} 仅支持整点`);
  return h*60+m;
}

// ── WS Status ─────────────────────────────────────

async function updateWSStatus() {
  try {
    const s = await apiGet("/api/ws/status");
    // Server conns
    const se = document.getElementById("ws-server-status");
    if (se) {
      const sp = se.querySelector("span");
      const n = s.wsServerConns||0;
      if (sp) sp.textContent = n;
      se.className = "status-badge "+(n>0?"status-on":"status-off")+" text-[11px]";
    }
    // Upstream status
    const ue = document.getElementById("ws-status");
    if (ue && s.connected !== undefined) {
      ue.style.display = "";
      const up = ue.querySelector("span");
      if (up) up.textContent = s.connected ? "已连接" : "未连接";
      ue.className = "status-badge "+(s.connected?"status-on":"status-off")+" text-[11px]";
    }
  } catch(e) { console.error("status", e); }
}

function updateWSEndpoint() {
  const el = document.getElementById("wsServerEndpoint");
  if (!el) return;
  const path = getValue("wsServerPath") || "/ws";
  const key = getValue("wsServerKey") || "";
  let url = location.origin + path;
  if (key) url += "?key=" + encodeURIComponent(key);
  el.textContent = url;
}

function copyWSEndpoint() {
  const t = document.getElementById("wsServerEndpoint")?.textContent || "";
  navigator.clipboard.writeText(t).then(() => {
    appendLog({ level:"INFO", source:"ui", message:"地址已复制" });
  }).catch(() => prompt("复制:", t));
}

// ── Countdown ─────────────────────────────────────

let cdInterval = null;

function initCountdowns() {
  if (cdInterval) clearInterval(cdInterval);
  cdInterval = setInterval(() => {
    stateTasks.forEach(t => updateCountdown(t.id, t.expiresAt));
  }, 1000);
}

function updateCountdown(taskId, ts) {
  const el = document.getElementById(`countdown-${taskId}`);
  if (!el) return;
  const sec = ts || 0;
  if (!sec) { el.classList.add("hidden"); return; }
  el.classList.remove("hidden","countdown-normal","countdown-warn","countdown-danger","animate-pulse");
  const diff = sec - Math.floor(Date.now()/1000);
  if (diff <= 0) { el.textContent="已过期"; el.classList.add("countdown-danger","animate-pulse"); return; }
  const h=Math.floor(diff/3600), m=Math.floor((diff%3600)/60);
  if (h>24) { const d=Math.floor(h/24); el.textContent=`${d}d${h%24}h`; el.classList.add("countdown-normal"); }
  else if (h>=1) { el.textContent=`${h}h${m}m`; el.classList.add("countdown-normal"); }
  else { el.textContent=`${m}m${diff%60}s`; el.classList.add("countdown-warn"); }
}

window.updateCountdown = updateCountdown;

// ── Expires ───────────────────────────────────────

function setExpiresDays(taskId, days) {
  const card = document.querySelector(`[data-task-card="1"][data-task-id="${cssEscape(taskId)}"]`);
  if (!card) return;
  const inp = card.querySelector('[data-field="expiresAt"]');
  const disp = document.getElementById(`expires-display-${taskId}`);
  if (!inp || !disp) return;
  let v = 0;
  if (days > 0) {
    const now = Math.floor(Date.now()/1000);
    let cur = Number(inp.value)||0;
    if (cur < now) cur = now;
    v = cur + days*86400;
  }
  inp.value = v;
  disp.textContent = fmtDT(v);
  const t = stateTasks.find(x => x.id===taskId);
  if (t) t.expiresAt = v;
  updateCountdown(taskId, v);
}

window.setExpiresDays = setExpiresDays;

function fmtDT(unix) {
  if (!unix) return "未设置";
  const d = new Date(unix*1000);
  const p = n => String(n).padStart(2,"0");
  return `${d.getFullYear()}-${p(d.getMonth()+1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}`;
}

// ── cURL import ───────────────────────────────────

function promptImportCurl(taskId) {
  const s = prompt("粘贴 curl 命令:");
  if (!s) return;
  try {
    const p = parseCurl(s);
    const card = document.querySelector(`[data-task-card="1"][data-task-id="${cssEscape(taskId)}"]`);
    if (!card) return;
    const set = (f,v) => { const e=card.querySelector(`[data-field="${f}"]`); if(e)e.value=v==null?"":String(v); };
    if (p.url) set("apiUrl", p.url);
    if (p.method) set("method", p.method);
    if (p.headers) set("headers", p.headers);
    if (p.body) set("body", p.body);
    appendLog({ level:"INFO", source:"ui", message:`cURL → ${taskId}` });
  } catch(e) { alert("解析失败: "+e.message); }
}

function parseCurl(s) {
  const r = { method:"GET", url:"", headers:"", body:"" };
  s = s.replace(/\\\r?\n/g," ");
  const u = s.match(/https?:\/\/[^\s'"]+/i);
  if (u) r.url = u[0].replace(/`/g,"");
  const m = s.match(/(?:-X|--request)\s+['"]?([A-Za-z]+)['"]?/);
  if (m) r.method = m[1].toUpperCase();
  let hs = [];
  const hr = /(?:-H|--header)\s+('([^'\\]*(?:\\.[^'\\]*)*)'|"([^"\\]*(?:\\.[^'\\]*)*)")/gi;
  let h;
  while ((h=hr.exec(s))!==null) hs.push((h[2]||h[3]||"").replace(/`/g,"").trim());
  const cr = /(?:-b|--cookie)\s+('([^'\\]*(?:\\.[^'\\]*)*)'|"([^"\\]*(?:\\.[^'\\]*)*)")/gi;
  while ((h=cr.exec(s))!==null) { const c=(h[2]||h[3]||"").replace(/`/g,"").trim(); if(c) hs.push("Cookie: "+c); }
  r.headers = hs.join("\n");
  const br = /(?:-d|--data|--data-raw|--data-binary)\s+('([^'\\]*(?:\\.[^'\\]*)*)'|"([^"\\]*(?:\\.[^'\\]*)*)")/i;
  const bm = s.match(br);
  if (bm) { r.body = bm[2]||bm[3]||""; if (!m) r.method="POST"; }
  return r;
}

// ── Logs ──────────────────────────────────────────

function initLogs() {
  const c = document.getElementById("log-container");
  const es = new EventSource("/api/logs/stream");
  es.onopen = () => { if (c) c.innerHTML = ""; };
  es.onmessage = ev => {
    try { appendLog(JSON.parse(ev.data)); } catch(e) { console.error("log parse", e); }
  };
  es.onerror = () => console.debug("SSE reconnect");
}

function appendLog(entry) {
  const c = document.getElementById("log-container");
  if (!c) return;
  const row = document.createElement("div");
  row.className = "flex gap-2 items-start text-[11px] leading-relaxed";
  const ts = entry.time ? new Date(entry.time).toLocaleTimeString("zh-CN",{hour12:false}) : new Date().toLocaleTimeString("zh-CN",{hour12:false});
  const lv = (entry.level||"INFO").toUpperCase();
  const src = entry.source||"";
  const msg = entry.message||"";
  const cls = lv==="ERROR"?"text-red-600":lv==="DEBUG"?"text-blue-600":"text-green-700";
  row.innerHTML = `<span class="text-gray-400 shrink-0">${ts}</span><span class="shrink-0 ${cls}">[${lv}]</span><span class="shrink-0 text-gray-400">${esc(src)}</span><span class="flex-1 whitespace-pre-wrap break-words text-gray-700">${esc(msg)}</span>`;
  c.appendChild(row);
  while (c.children.length > 500) c.removeChild(c.firstChild);
  c.scrollTop = c.scrollHeight;
}

// ── Helpers ───────────────────────────────────────

function randomId(p) {
  try { if (crypto?.randomUUID) return crypto.randomUUID(); } catch {}
  return `${p}-${Date.now()}-${Math.floor(Math.random()*1e6)}`;
}

function cssEscape(s) { return String(s).replace(/"/g,'\\"'); }
function esc(s) { return String(s).replace(/&/g,"&amp;").replace(/</g,"&lt;").replace(/>/g,"&gt;").replace(/"/g,"&quot;").replace(/'/g,"&#39;"); }
function setValue(id,v) { const e=document.getElementById(id); if(e) e.value = v==null?"":String(v); }
function getValue(id) { const e=document.getElementById(id); return e?e.value:""; }
function setChecked(id,v) { const e=document.getElementById(id); if(e) e.checked=!!v; }
function isChecked(id) { const e=document.getElementById(id); return !!(e&&e.checked); }
