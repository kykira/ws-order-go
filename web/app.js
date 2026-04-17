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
    el.className = "status-badge " + (connected ? "status-on" : "status-off");
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
        expiresAt: Number(task.expiresAt || 0) || 0,
        allowedSymbols: String(task.allowedSymbols || ""),
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
    expiresAt: 0,
    httpProxyUrl: "",
    apiUrl: "https://www.binance.com/bapi/futures/v2/private/future/event-contract/place-order",
    method: "POST",
    headers: "Content-Type: application/json\nclienttype: web",
    body: '{"orderAmount":"{{amount}}","timeIncrements":"{{unit}}","symbolName":"BTCUSDT","payoutRatio":"0.80","direction":"{{action}}"}',
    valueBuy: "LONG",
    valueSell: "SHORT",
  });
}


function renderTasks(tasks) {
  const container = document.getElementById("tasks-container");
  if (!container) return;
  if (!Array.isArray(tasks) || tasks.length === 0) {
    container.innerHTML =
      '<div class="card text-center text-xs text-gray-400 py-6">暂无任务，点击上方"+ 添加任务"。</div>';
    return;
  }

  container.innerHTML = tasks.map((t) => taskCardHtml(t)).join("\n");

  // 初始化倒计时
  initCountdowns();

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

function formatDateTimeLocal(unixSec) {
  if (!unixSec) return "未设置";
  const d = new Date(unixSec * 1000);
  const pad = (n) => String(n).padStart(2, "0");
  return `${d.getFullYear()}-${pad(d.getMonth()+1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

function taskCardHtml(t) {
  const id = escapeHtml(t.id);
  const name = escapeHtml(t.name);
  const enabledChecked = t.enabled ? "checked" : "";
  const dtLocal = formatDateTimeLocal(t.expiresAt);

  return `
  <div class="task-card" data-task-card="1" data-task-id="${id}">
    <!-- Header -->
    <div class="task-header">
      <div class="task-meta">
        <input class="input" style="max-width:12rem" data-field="name" value="${name}" placeholder="任务名称" />
        <span class="text-[11px] font-mono text-gray-400">${id}</span>
        <div id="countdown-${id}" class="countdown hidden"></div>
        <label class="switch-label" title="启用任务">
          <span class="text-xs text-gray-500">启用</span>
          <span class="switch">
            <input type="checkbox" class="switch-input" data-field="enabled" ${enabledChecked} />
            <span class="switch-track"></span>
          </span>
        </label>
      </div>
      <div class="task-actions">
        <button type="button" class="btn btn-primary" data-action="test-buy" data-task-id="${id}">测试 BUY</button>
        <button type="button" class="btn btn-ghost" data-action="test-sell" data-task-id="${id}">测试 SELL</button>
        <button type="button" class="btn btn-ghost" onclick="promptImportCurl('${id}')">导入 cURL</button>
        <button type="button" class="btn btn-danger" data-action="delete" data-task-id="${id}">删除任务</button>
      </div>
    </div>

    <!-- Request config -->
    <div class="grid gap-3 mt-1" style="grid-template-columns: 7rem 1fr">
      <div>
        <label class="label">Method</label>
        <select class="input" data-field="method">
          <option value="GET" ${t.method === 'GET' ? 'selected' : ''}>GET</option>
          <option value="POST" ${t.method === 'POST' ? 'selected' : ''}>POST</option>
          <option value="PUT" ${t.method === 'PUT' ? 'selected' : ''}>PUT</option>
          <option value="DELETE" ${t.method === 'DELETE' ? 'selected' : ''}>DELETE</option>
        </select>
      </div>
      <div>
        <label class="label">API URL <span class="tag">必填</span></label>
        <input class="input" data-field="apiUrl" placeholder="https://..." value="${escapeHtml(t.apiUrl)}" />
      </div>
    </div>

    <div class="grid gap-3 sm:grid-cols-2 mt-3">
      <div>
        <label class="label">Headers</label>
        <p class="hint">一行一个 Key: Value</p>
        <textarea rows="5" class="input" data-field="headers" placeholder="Content-Type: application/json">${escapeHtml(t.headers)}</textarea>
      </div>
      <div>
        <label class="label">Body</label>
        <p class="hint">变量: {{amount}}, {{unit}}, {{action}}, {{symbol}}, {{tickerType}}</p>
        <textarea rows="5" class="input" data-field="body" placeholder='{"amount": "{{amount}}"}'>${escapeHtml(t.body)}</textarea>
      </div>
    </div>

    <div class="grid gap-3 sm:grid-cols-2 mt-3">
      <div>
        <label class="label">action=buy 替换为 <span class="tag tag-muted">可选</span></label>
        <input class="input" data-field="valueBuy" value="${escapeHtml(t.valueBuy)}" placeholder="LONG" />
      </div>
      <div>
        <label class="label">action=sell 替换为 <span class="tag tag-muted">可选</span></label>
        <input class="input" data-field="valueSell" value="${escapeHtml(t.valueSell)}" placeholder="SHORT" />
      </div>
    </div>

    <div class="grid gap-3 sm:grid-cols-2 mt-3">
      <div>
        <label class="label">跳过前 N 次信号</label>
        <input type="number" min="0" class="input" data-field="skipSignals" value="${t.skipSignals || 0}" />
      </div>
      <div>
        <label class="label">代理 <span class="tag tag-muted">可选</span></label>
        <input class="input" data-field="httpProxyUrl" value="${escapeHtml(t.httpProxyUrl)}" placeholder="http://127.0.0.1:7890" />
      </div>
    </div>

    <div class="grid gap-3 sm:grid-cols-2 mt-3">
      <div>
        <label class="label">允许交易对 <span class="tag tag-muted">可选</span></label>
        <input class="input" data-field="allowedSymbols" value="${escapeHtml(t.allowedSymbols)}" placeholder="BTCUSDT,ETHUSDT (留空=全部)" />
      </div>
      <div>
        <label class="label">过期提醒 <span class="tag tag-muted">可选</span></label>
        <input type="hidden" data-field="expiresAt" value="${t.expiresAt || 0}" />
        <div class="expires-row">
          <span class="expires-display" id="expires-display-${id}">${dtLocal}</span>
          <div class="expires-btns">
            <button type="button" class="expires-btn" onclick="setExpiresDays('${id}', 1)">+1d</button>
            <button type="button" class="expires-btn" onclick="setExpiresDays('${id}', 3)">+3d</button>
            <button type="button" class="expires-btn" onclick="setExpiresDays('${id}', 7)">+7d</button>
          </div>
          <button type="button" class="expires-clear" onclick="setExpiresDays('${id}', 0)">清除</button>
        </div>
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
    
    let expiresSec = 0;
    const expVal = get("expiresAt");
    if (expVal) {
      expiresSec = Number(expVal) || 0;
    }

    tasks.push({
      id: taskId,
      name: String(get("name") || "").trim() || taskId,
      enabled: getChecked("enabled"),
      skipSignals: Number(get("skipSignals") || 0) || 0,
      expiresAt: expiresSec,
      allowedSymbols: String(get("allowedSymbols") || "").trim(),
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
  // Replace line continuations
  let str = curlStr.replace(/\\\r?\n/g, ' ');

  // Extract URL
  const urlMatch = str.match(/https?:\/\/[^\s'"]+/i);
  if (urlMatch) {
    result.url = urlMatch[0].replace(/[`]/g, '');
  }

  // Extract Method
  const methodMatch = str.match(/(?:-X|--request)\s+['"]?([A-Za-z]+)['"]?/);
  if (methodMatch) {
    result.method = methodMatch[1].toUpperCase();
  }

  let headers = [];
  
  // Extract Headers (-H, --header)
  const headerRegex = /(?:-H|--header)\s+('([^'\\]*(?:\\.[^'\\]*)*)'|"([^"\\]*(?:\\.[^"\\]*)*)")/gi;
  let match;
  while ((match = headerRegex.exec(str)) !== null) {
    let h = match[2] || match[3] || "";
    headers.push(h.replace(/`/g, '').trim());
  }

  // Extract Cookies (-b, --cookie) and append as Header
  const cookieRegex = /(?:-b|--cookie)\s+('([^'\\]*(?:\\.[^'\\]*)*)'|"([^"\\]*(?:\\.[^"\\]*)*)")/gi;
  while ((match = cookieRegex.exec(str)) !== null) {
    let c = match[2] || match[3] || "";
    c = c.replace(/`/g, '').trim();
    if (c) {
      headers.push(`Cookie: ${c}`);
    }
  }

  result.headers = headers.join('\n');

  // Extract Body (-d, --data, --data-raw, --data-binary)
  const bodyRegex = /(?:-d|--data|--data-raw|--data-binary)\s+('([^'\\]*(?:\\.[^'\\]*)*)'|"([^"\\]*(?:\\.[^"\\]*)*)")/i;
  const bodyMatch = str.match(bodyRegex);
  if (bodyMatch) {
    result.body = bodyMatch[2] || bodyMatch[3] || "";
    if (!methodMatch) result.method = "POST";
  }

  return result;
}

function setExpiresDays(taskId, days) {
  const card = document.querySelector(`[data-task-card="1"][data-task-id="${cssEscape(taskId)}"]`);
  if (!card) return;
  const input = card.querySelector(`[data-field="expiresAt"]`);
  const display = document.getElementById(`expires-display-${taskId}`);
  if (!input || !display) return;

  let newExpires = 0;
  if (days > 0) {
    const nowSec = Math.floor(Date.now() / 1000);
    let currentExpires = Number(input.value) || 0;
    
    // 如果当前没有设置，或者已经过期，则以当前时间为基准累加
    if (currentExpires < nowSec) {
      currentExpires = nowSec;
    }
    
    newExpires = currentExpires + (days * 24 * 3600);
  }

  input.value = newExpires;
  display.textContent = formatDateTimeLocal(newExpires);

  // Update state task so interval works immediately
  const t = stateTasks.find(x => x.id === taskId);
  if (t) t.expiresAt = newExpires;
  
  updateCountdown(taskId, null, newExpires);
}

window.setExpiresDays = setExpiresDays;

let countdownInterval = null;

function initCountdowns() {
  if (countdownInterval) {
    clearInterval(countdownInterval);
  }
  countdownInterval = setInterval(() => {
    stateTasks.forEach(task => {
      updateCountdown(task.id, null, task.expiresAt);
    });
  }, 1000);
}

function updateCountdown(taskId, _unused, timestamp) {
  const el = document.getElementById(`countdown-${taskId}`);
  if (!el) return;

  let expiresSec = timestamp || 0;
  if (!expiresSec) {
    el.classList.add("hidden");
    return;
  }

  el.classList.remove("hidden");
  const now = Math.floor(Date.now() / 1000);
  const diff = expiresSec - now;

  el.classList.remove("countdown-normal", "countdown-warn", "countdown-danger", "animate-pulse");

  if (diff <= 0) {
    el.textContent = "已过期";
    el.classList.add("countdown-danger", "animate-pulse");
  } else {
    const h = Math.floor(diff / 3600);
    const m = Math.floor((diff % 3600) / 60);
    const s = diff % 60;
    
    if (h > 24) {
      const d = Math.floor(h / 24);
      el.textContent = `${d}天 ${h%24}时`;
      el.classList.add("countdown-normal");
    } else if (h >= 1) {
      el.textContent = `${h}时 ${m}分`;
      el.classList.add("countdown-normal");
    } else {
      el.textContent = `${m}分 ${s}秒`;
      el.classList.add("countdown-warn");
      if (diff < 300) {
        el.classList.replace("countdown-warn", "countdown-danger");
        el.classList.add("animate-pulse");
      }
    }
  }
}

window.updateCountdown = updateCountdown;


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
  row.className = "flex gap-2 items-start text-[11px] leading-relaxed";

  const timeStr = entry.time
    ? new Date(entry.time).toLocaleTimeString("zh-CN", { hour12: false })
    : new Date().toLocaleTimeString("zh-CN", { hour12: false });

  const level = (entry.level || "INFO").toUpperCase();
  const source = entry.source || "app";
  const msg = entry.message || "";

  const colorClass =
    level === "ERROR"
      ? "text-red-600"
      : level === "DEBUG"
      ? "text-blue-600"
      : "text-green-700";

  row.innerHTML = `
    <span class="text-gray-400 shrink-0">${timeStr}</span>
    <span class="shrink-0 ${colorClass}">[${level}]</span>
    <span class="shrink-0 text-gray-400">${escapeHtml(source)}</span>
    <span class="flex-1 whitespace-pre-wrap break-words text-gray-700">${escapeHtml(msg)}</span>
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
