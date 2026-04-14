document.addEventListener("DOMContentLoaded", () => {
  initConfig();
  initActions();
  initLogs();
});

const BINANCE_PLACE_ORDER_URL =
  "https://www.binance.com/bapi/futures/v2/private/future/event-contract/place-order";

let stateTasks = [];

async function apiGet(path) {
  const res = await fetch(path);
  if (!res.ok) throw new Error(`GET ${path} failed: ${res.status}`);
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
    throw new Error(`POST ${path} failed: ${res.status} ${msg}`);
  }
  try {
    return await res.json();
  } catch {
    return {};
  }
}

function initConfig() {
  loadConfig().catch((err) => console.error(err));
  updateWSStatus();
  setInterval(updateWSStatus, 3000);
}

async function loadConfig() {
  try {
    const cfg = await apiGet("/api/config");

    setValue("wsUrl", cfg.upstream?.wsUrl || "");
    setValue("wsKey", cfg.upstream?.wsKey || "");
    setChecked("wsEnabled", !!cfg.upstream?.enabled);

    stateTasks = normalizeTasks(cfg);
    renderTasks(stateTasks);
  } catch (err) {
    console.error("loadConfig error", err);
    appendLog({
      time: new Date().toISOString(),
      level: "ERROR",
      source: "ui",
      message: `加载配置失败: ${err.message}`,
    });
  }
}

function initActions() {
  const btnSave = document.getElementById("btn-save-config");
  if (btnSave) {
    btnSave.addEventListener("click", async () => {
      try {
        const payload = collectConfigPayload();
        await apiPost("/api/config", payload);
        appendLog({
          time: new Date().toISOString(),
          level: "INFO",
          source: "ui",
          message: "配置已保存并应用",
        });
        // Refresh from server (server may normalize tasks)
        await loadConfig();
      } catch (err) {
        appendLog({
          time: new Date().toISOString(),
          level: "ERROR",
          source: "ui",
          message: `保存配置失败: ${err.message}`,
        });
      }
    });
  }

  const btnConn = document.getElementById("btn-ws-connect");
  if (btnConn) {
    btnConn.addEventListener("click", async () => {
      try {
        await apiPost("/api/ws/connect", {});
        appendLog({
          time: new Date().toISOString(),
          level: "INFO",
          source: "ui",
          message: "已请求连接上游 WS",
        });
        updateWSStatus();
      } catch (err) {
        appendLog({
          time: new Date().toISOString(),
          level: "ERROR",
          source: "ui",
          message: `连接上游 WS 失败: ${err.message}`,
        });
      }
    });
  }

  const btnDisc = document.getElementById("btn-ws-disconnect");
  if (btnDisc) {
    btnDisc.addEventListener("click", async () => {
      try {
        await apiPost("/api/ws/disconnect", {});
        appendLog({
          time: new Date().toISOString(),
          level: "INFO",
          source: "ui",
          message: "已请求断开上游 WS",
        });
        updateWSStatus();
      } catch (err) {
        appendLog({
          time: new Date().toISOString(),
          level: "ERROR",
          source: "ui",
          message: `断开上游 WS 失败: ${err.message}`,
        });
      }
    });
  }

  const btnImportJson = document.getElementById("btn-import-json");
  if (btnImportJson) {
    btnImportJson.addEventListener("click", () => {
      const jsonStr = prompt("请粘贴 config.json 的完整内容：");
      if (!jsonStr) return;
      try {
        const cfg = JSON.parse(jsonStr);
        setValue("wsUrl", cfg.upstream?.wsUrl || "");
        setValue("wsKey", cfg.upstream?.wsKey || "");
        setChecked("wsEnabled", !!cfg.upstream?.enabled);
        
        stateTasks = normalizeTasks(cfg);
        renderTasks(stateTasks);
        
        appendLog({
          time: new Date().toISOString(),
          level: "INFO",
          source: "ui",
          message: "JSON 配置导入成功（未保存，请确认无误后点击“保存配置”）",
        });
      } catch (err) {
        alert("JSON 解析失败: " + err.message);
      }
    });
  }

  const btnAdd = document.getElementById("btn-add-task");
  if (btnAdd) {
    btnAdd.addEventListener("click", () => {
      stateTasks.push(buildDefaultTask());
      renderTasks(stateTasks);
    });
  }
  const btnAddApp = document.getElementById("btn-add-task-binance-app");
  if (btnAddApp) {
    btnAddApp.addEventListener("click", () => {
      stateTasks.push(buildBinanceAppTask());
      renderTasks(stateTasks);
    });
  }
}

async function updateWSStatus() {
  try {
    const status = await apiGet("/api/ws/status");
    const el = document.getElementById("ws-status");
    if (!el) return;
    const span = el.querySelector("span");
    if (!span) return;
    const connected = !!status.connected;
    span.textContent = connected ? "已连接" : "未连接";
    el.className =
      "mt-1 inline-flex items-center px-2 py-0.5 rounded-full text-xs " +
      (connected
        ? "bg-emerald-50 text-emerald-700 border border-emerald-200"
        : "bg-slate-100 text-slate-600 border border-slate-200");
  } catch (err) {
    console.error("updateWSStatus error", err);
  }
}

function collectConfigPayload() {
  const upstream = {
    wsUrl: getValue("wsUrl"),
    wsKey: getValue("wsKey"),
    enabled: isChecked("wsEnabled"),
  };

  const tasks = collectTasksFromDom();
  return { upstream, tasks };
}

function normalizeTasks(cfg) {
  const tasks = Array.isArray(cfg?.tasks) ? cfg.tasks : [];
    if (tasks.length > 0) return tasks.map(normalizeTask);

    // Legacy fallback: map old upstream.skipSignals to default task
    const legacySkip = cfg?.upstream?.skipSignals || 0;
    return [
        normalizeTask({
            id: "default",
            name: "Default Task",
            enabled: true,
            skipSignals: legacySkip,
            httpProxyUrl: "",
            apiUrl: "https://www.binance.com/bapi/futures/v2/private/future/event-contract/place-order",
            method: "POST",
            headers: "Content-Type: application/json\nclienttype: web",
            body: '{"orderAmount":"{{amount}}","timeIncrements":"{{unit}}","symbolName":"BTCUSDT","payoutRatio":"0.80","direction":"{{action}}"}',
            valueBuy: "LONG",
            valueSell: "SHORT",
        }),
    ];
}

function normalizeTask(t) {
    const task = t || {};
    return {
        id: String(task.id || "").trim() || randomId("task"),
        name: String(task.name || "").trim() || "Task",
        enabled: task.enabled !== false,
        skipSignals: Number(task.skipSignals || 0) || 0,
        httpProxyUrl: String(task.httpProxyUrl || ""),
        apiUrl: String(task.apiUrl || ""),
        method: String(task.method || "POST").toUpperCase(),
        headers: String(task.headers || ""),
        body: String(task.body || ""),
        valueBuy: String(task.valueBuy || ""),
        valueSell: String(task.valueSell || ""),
    };
}

function buildDefaultTask() {
  return normalizeTask({
    id: randomId("task"),
    name: "New Task",
    enabled: true,
    skipSignals: 0,
    httpProxyUrl: "",
    apiUrl: "https://www.binance.com/bapi/futures/v2/private/future/event-contract/place-order",
    method: "POST",
    headers: "Content-Type: application/json\nclienttype: web",
    body: '{"orderAmount":"{{amount}}","timeIncrements":"{{unit}}","symbolName":"BTCUSDT","payoutRatio":"0.80","direction":"{{action}}"}',
    valueBuy: "LONG",
    valueSell: "SHORT",
  });
}

function buildBinanceAppTask() {
  const t = buildDefaultTask();
  t.name = "Binance App Task";
  t.headers = "Content-Type: application/json\nclienttype: android\nx-token: ";
  return t;
}

function renderTasks(tasks) {
  const container = document.getElementById("tasks-container");
  if (!container) return;
  if (!Array.isArray(tasks) || tasks.length === 0) {
    container.innerHTML =
      '<div class="rounded-lg border border-dashed border-slate-200 bg-slate-50/80 px-3 py-2 text-xs text-slate-500">暂无任务，请点击“添加任务”。</div>';
    return;
  }

  container.innerHTML = tasks.map((t) => taskCardHtml(t)).join("\n");

  container.querySelectorAll("[data-action]").forEach((el) => {
    el.addEventListener("click", async (ev) => {
      const btn = ev.currentTarget;
      const action = btn.getAttribute("data-action");
      const taskId = btn.getAttribute("data-task-id");
      if (!taskId) return;

      if (action === "delete") {
        stateTasks = stateTasks.filter((x) => x.id !== taskId);
        renderTasks(stateTasks);
        return;
      }

      if (action === "test-buy" || action === "test-sell") {
        const testAction = action === "test-buy" ? "buy" : "sell";
        try {
          await apiPost("/api/tasks/test", { taskId, action: testAction });
          appendLog({
            time: new Date().toISOString(),
            level: "INFO",
            source: "ui",
            message: `任务测试下单已发送 task=${taskId} action=${testAction}`,
          });
        } catch (err) {
          appendLog({
            time: new Date().toISOString(),
            level: "ERROR",
            source: "ui",
            message: `任务测试下单失败 task=${taskId}: ${err.message}`,
          });
        }
        return;
      }


    });
  });
}

function taskCardHtml(t) {
  const id = escapeHtml(t.id);
  const name = escapeHtml(t.name);
  const enabledChecked = t.enabled ? "checked" : "";

  return `
  <div class="rounded-xl border border-slate-200 bg-white p-4 space-y-4" data-task-card="1" data-task-id="${id}">
    <div class="flex items-start justify-between gap-3">
      <div class="flex-1 space-y-2">
        <div class="flex items-center gap-2">
          <div class="text-xs font-semibold text-slate-600">任务</div>
          <div class="text-[11px] font-mono text-slate-400">${id}</div>
        </div>
        <input class="input" data-field="name" value="${name}" placeholder="任务名称" />
        <label class="toggle-card" style="padding: 0.75rem 0.9rem;" title="启用后，上游信号到来会执行该任务">
          <div>
            <div class="field-label">启用任务</div>
            <div class="field-hint">上游 WS 信号到来时会触发所有启用任务。</div>
          </div>
          <input type="checkbox" class="toggle-checkbox" data-field="enabled" ${enabledChecked} />
        </label>
      </div>
      <div class="flex flex-col gap-2 w-[11.5rem]">
        <button type="button" class="btn-primary" data-action="test-buy" data-task-id="${id}">测试 BUY</button>
        <button type="button" class="btn-ghost" data-action="test-sell" data-task-id="${id}">测试 SELL</button>
        <button type="button" class="btn-ghost" data-action="delete" data-task-id="${id}">删除任务</button>
      </div>
    </div>

    <div class="flex items-center justify-between mt-4">
      <h4 class="text-sm font-semibold text-slate-700">请求配置</h4>
      <button type="button" class="btn-ghost text-xs py-1 px-2" onclick="promptImportCurl('${id}')">
        <svg class="w-3 h-3 mr-1 inline" fill="none" stroke="currentColor" viewBox="0 0 24 24"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M8 16H6a2 2 0 01-2-2V6a2 2 0 012-2h8a2 2 0 012 2v2m-6 12h8a2 2 0 002-2v-8a2 2 0 00-2-2h-8a2 2 0 00-2 2v8a2 2 0 002 2z"></path></svg>
        导入 cURL
      </button>
    </div>

    <div class="grid gap-4 md:grid-cols-4 mt-3">
      <div class="md:col-span-1">
        <label class="field-label mb-1">Method</label>
        <select class="input" data-field="method">
          <option value="GET" ${t.method === 'GET' ? 'selected' : ''}>GET</option>
          <option value="POST" ${t.method === 'POST' ? 'selected' : ''}>POST</option>
          <option value="PUT" ${t.method === 'PUT' ? 'selected' : ''}>PUT</option>
          <option value="DELETE" ${t.method === 'DELETE' ? 'selected' : ''}>DELETE</option>
        </select>
      </div>
      <div class="md:col-span-3">
        <label class="field-label mb-1">API URL <span class="field-tag">必填</span></label>
        <input class="input" data-field="apiUrl" placeholder="https://..." value="${escapeHtml(t.apiUrl)}" />
      </div>
    </div>

    <div class="grid gap-4 md:grid-cols-2 mt-3">
      <div>
        <label class="field-label mb-1">Headers</label>
        <p class="field-hint mb-2">一行一个，格式为 Key: Value</p>
        <textarea rows="6" class="input font-mono text-xs" data-field="headers" placeholder="Content-Type: application/json">${escapeHtml(t.headers)}</textarea>
      </div>
      <div>
        <label class="field-label mb-1">Body</label>
        <p class="field-hint mb-2">支持变量: {{amount}}, {{unit}}, {{action}} (或 {{direction}})</p>
        <textarea rows="6" class="input font-mono text-xs" data-field="body" placeholder='{"amount": "{{amount}}"}'>${escapeHtml(t.body)}</textarea>
      </div>
    </div>

    <div class="grid gap-4 md:grid-cols-2 mt-3">
      <div class="field">
        <label class="field-label mb-1">当 action=buy 时替换为 <span class="field-tag field-tag-muted">可选</span></label>
        <input class="input" data-field="valueBuy" value="${escapeHtml(t.valueBuy)}" placeholder="如: 1 或 LONG" />
      </div>
      <div class="field">
        <label class="field-label mb-1">当 action=sell 时替换为 <span class="field-tag field-tag-muted">可选</span></label>
        <input class="input" data-field="valueSell" value="${escapeHtml(t.valueSell)}" placeholder="如: 0 或 SHORT" />
      </div>
    </div>

    <div class="grid gap-4 md:grid-cols-2 mt-3">
      <div class="field">
        <label class="field-label mb-1">跳过前 N 次信号</label>
        <input type="number" min="0" class="input" data-field="skipSignals" value="${t.skipSignals || 0}" />
      </div>
      <div class="field">
        <label class="field-label mb-1">代理 (httpProxyUrl) <span class="field-tag field-tag-muted">可选</span></label>
        <input class="input" data-field="httpProxyUrl" value="${escapeHtml(t.httpProxyUrl)}" placeholder="http://127.0.0.1:7890" />
      </div>
    </div>
  </div>
  `;
}

function collectTasksFromDom() {
  const cards = document.querySelectorAll('[data-task-card="1"]');
  const tasks = [];
  cards.forEach((card) => {
    const taskId = card.getAttribute("data-task-id") || randomId("task");
    const get = (field) => {
      const el = card.querySelector(`[data-field="${field}"]`);
      return el ? el.value : "";
    };
    const getChecked = (field) => {
      const el = card.querySelector(`[data-field="${field}"]`);
      return !!(el && el.checked);
    };

    tasks.push({
      id: taskId,
      name: String(get("name") || "").trim() || taskId,
      enabled: getChecked("enabled"),
      skipSignals: Number(get("skipSignals") || 0) || 0,
      httpProxyUrl: String(get("httpProxyUrl") || "").trim(),
      apiUrl: String(get("apiUrl") || "").trim(),
      method: String(get("method") || "POST").trim().toUpperCase(),
      headers: String(get("headers") || ""),
      body: String(get("body") || ""),
      valueBuy: String(get("valueBuy") || "").trim(),
      valueSell: String(get("valueSell") || "").trim(),
    });
  });
  return tasks;
}

function promptImportCurl(taskId) {
  const curlStr = prompt("请粘贴 curl 命令 (支持从浏览器网络面板 Copy as cURL)");
  if (!curlStr) return;

  try {
    const parsed = parseCurl(curlStr);
    const card = document.querySelector(`[data-task-card="1"][data-task-id="${cssEscape(taskId)}"]`);
    if (!card) return;
    const set = (field, value) => {
      const el = card.querySelector(`[data-field="${field}"]`);
      if (el) el.value = value == null ? "" : String(value);
    };

    if (parsed.url) set("apiUrl", parsed.url);
    if (parsed.method) set("method", parsed.method);
    if (parsed.headers) set("headers", parsed.headers);
    if (parsed.body) set("body", parsed.body);
    
    appendLog({ level: "INFO", source: "ui", message: `已成功导入 cURL 到任务[${taskId}]，请检查并保存。` });
  } catch (e) {
    alert("解析 cURL 失败: " + e.message);
  }
}

function parseCurl(curlStr) {
  const result = { method: "GET", url: "", headers: "", body: "" };
  let str = curlStr.replace(/\\\r?\n/g, ' ');

  const urlMatch = str.match(/https?:\/\/[^\s'"]+/i);
  if (urlMatch) {
    result.url = urlMatch[0].replace(/[`]/g, '');
  }

  const methodMatch = str.match(/(?:-X|--request)\s+['"]?([A-Za-z]+)['"]?/);
  if (methodMatch) {
    result.method = methodMatch[1].toUpperCase();
  }

  const headerRegex = /(?:-H|--header)\s+('([^'\\]*(?:\\.[^'\\]*)*)'|"([^"\\]*(?:\\.[^"\\]*)*)")/gi;
  let headers = [];
  let match;
  while ((match = headerRegex.exec(str)) !== null) {
    let h = match[2] || match[3] || "";
    headers.push(h.replace(/`/g, '').trim());
  }
  result.headers = headers.join('\n');

  const bodyRegex = /(?:-d|--data|--data-raw|--data-binary)\s+('([^'\\]*(?:\\.[^'\\]*)*)'|"([^"\\]*(?:\\.[^"\\]*)*)")/i;
  const bodyMatch = str.match(bodyRegex);
  if (bodyMatch) {
    result.body = bodyMatch[2] || bodyMatch[3] || "";
    if (!methodMatch) result.method = "POST";
  }

  return result;
}

window.promptImportCurl = promptImportCurl;


function randomId(prefix) {
  try {
    if (crypto && crypto.randomUUID) return crypto.randomUUID();
  } catch {}
  return `${prefix}-${Date.now()}-${Math.floor(Math.random() * 1e6)}`;
}

function cssEscape(text) {
  // Minimal CSS escape for attribute selectors.
  return String(text).replace(/"/g, '\\"');
}

function selectOption(value, current) {
  return `<option value="${value}" ${value === current ? "selected" : ""}>${value}</option>`;
}

function setValue(id, value) {
  const el = document.getElementById(id);
  if (el) el.value = value == null ? "" : String(value);
}

function getValue(id) {
  const el = document.getElementById(id);
  return el ? el.value : "";
}

function setChecked(id, checked) {
  const el = document.getElementById(id);
  if (el) el.checked = !!checked;
}

function isChecked(id) {
  const el = document.getElementById(id);
  return !!(el && el.checked);
}

function initLogs() {
  const container = document.getElementById("log-container");
  const es = new EventSource("/api/logs/stream");
  es.onopen = () => {
    // 每次连接成功（包括重连），清空一次界面日志，防止后端重复推送历史数据
    if (container) container.innerHTML = "";
  };
  es.onmessage = (ev) => {
    try {
      const entry = JSON.parse(ev.data);
      appendLog(entry);
    } catch (e) {
      console.error("parse log entry error", e, ev.data);
    }
  };
  es.onerror = () => {
    // 浏览器会自动重连，这里只在控制台打印，不打扰用户
    console.debug("SSE logs stream disconnected, browser will auto-reconnect.");
  };
}

function appendLog(entry) {
  const container = document.getElementById("log-container");
  if (!container) return;

  const row = document.createElement("div");
  row.className = "flex gap-2 items-start text-[11px] text-slate-100";

  const timeStr = entry.time
    ? new Date(entry.time).toLocaleTimeString("zh-CN", { hour12: false })
    : new Date().toLocaleTimeString("zh-CN", { hour12: false });

  const level = (entry.level || "INFO").toUpperCase();
  const source = entry.source || "app";
  const msg = entry.message || "";

  const colorClass =
    level === "ERROR"
      ? "text-rose-400"
      : level === "DEBUG"
      ? "text-sky-300"
      : "text-emerald-300";

  row.innerHTML = `
    <span class="text-slate-500 shrink-0">${timeStr}</span>
    <span class="shrink-0 ${colorClass}">[${level}]</span>
    <span class="shrink-0 text-slate-400">${escapeHtml(source)}</span>
    <span class="flex-1 whitespace-pre-wrap break-words">${escapeHtml(msg)}</span>
  `;

  container.appendChild(row);
  while (container.children.length > 500) {
    container.removeChild(container.firstChild);
  }
  container.scrollTop = container.scrollHeight;
}

function escapeHtml(str) {
  return String(str)
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;")
    .replace(/"/g, "&quot;")
    .replace(/'/g, "&#39;");
}
