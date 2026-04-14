# WS 下单桥接服务

这是一个独立、高性能的 Golang 服务，用于监听上游 WebSocket 信号，并自动化地向多个下游（如各大加密货币交易所的 API）发起 HTTP 下单请求。

## 核心特性

- **多任务并发执行**：收到一条上游信号，可同时触发多个下游 API 任务。
- **完全自定义的请求模板**：不再局限于任何特定交易所，支持完全自定义 `URL`、`Method`、`Headers` 和 `Body`。
- **一键导入 cURL**：在页面上直接粘贴你在浏览器网络面板复制的 `curl` 命令，系统会自动解析并填充配置。
- **动态变量替换**：支持在请求体内使用 `{{action}}`、`{{amount}}`、`{{unit}}` 等变量，收到信号时自动替换为真实值。
- **独立代理支持**：每个任务可以配置独立的 `httpProxyUrl`。
- **纯净的文件日志**：支持在终端和 Web 页面实时查看日志，同时真实的下单流水会持久化到 `data/order.log`，并自动过滤测试干扰。
- **优雅的前端面板**：提供现代化的左右分栏 Web 控制台，实时监控运行状态。

## 目录结构

项目位于当前工作目录下的 `wsorder-go` 子目录：

```text
wsorder-go/
  cmd/
    server/
      main.go              # 程序入口
  internal/
    config/
      config.go            # 配置结构体与多任务模型、JSON 持久化
    logs/
      logs.go              # 日志 ring buffer + SSE 订阅 + 文件持久化
    order/
      order.go             # 核心下单模块与 HTTP 客户端连接池
    signals/
      processor.go         # 信号处理、变量映射与任务分发
    wsclient/
      wsclient.go          # 上游 WebSocket 客户端（含自动重连与心跳）
    wsserver/
      wsserver.go          # 本地 WebSocket 服务器（用于接受外部推送）
  web/
    index.html             # 前端控制台页面
    app.js                 # 页面逻辑（表单、任务管理、SSE 监听）
    styles.css             # 额外样式（基于 Tailwind）
  go.mod
  config.json              # 运行时配置（首次启动会自动生成）
  data/
    order.log              # 真实的下单流水日志（自动生成）
  README.md
```

## Docker 部署

项目支持 Docker 部署，且配置了 GitHub Actions 进行自动化构建。

### 本地构建与运行

```bash
# 构建镜像
docker build -t wsorder-go .

# 运行容器 (映射 9946 端口，并挂载数据目录持久化配置和日志)
docker run -d \
  -p 9946:9946 \
  -v $(pwd)/data:/app/data \
  -v $(pwd)/config.json:/app/config.json \
  --name wsorder \
  wsorder-go
```

### 使用 Docker Compose

推荐使用 `docker-compose` 来管理服务，这样配置更直观，数据持久化更稳定。

1. 在项目根目录下确保已生成空的 `config.json`（或手动创建）：
   ```bash
   touch config.json
   ```

2. 启动服务：
   ```bash
   docker-compose up -d
   ```

3. 停止服务：
   ```bash
   docker-compose down
   ```

### GitHub Actions 自动构建

当推送代码到 `dev-next` 分支或打上版本 Tag 时，`.github/workflows/docker.yml` 会自动触发，将镜像构建并推送到 GitHub Container Registry (`ghcr.io`)。

```bash
cd wsorder-go
```

2. 运行服务（任选其一）：

```bash
# 直接运行
go run ./cmd/server

# 或先编译再运行
go build ./cmd/server && ./server
```

3. 启动后在浏览器访问：

```text
http://localhost:9946/
```

默认监听端口为 `9946`；如需修改，可：
- 在启动前设置环境变量：`export WSORDER_PORT=9101`
- 或者在生成的 `config.json` 中修改 `server.port` 字段（修改后重启服务生效）。

## 配置页面说明

打开 Web 页面后，你会看到**左侧实时日志区**和**右侧配置区**。

### 1. 上游 WebSocket

- **wsUrl**：上游信号源地址，例如 `wss://example.com/ws/stream`
- **wsKey**：可选，会作为 query 参数附加到 URL 上。
- **启用上游 WS**：勾选后服务会自动连接并在断开时重连。
- **连接/断开按钮**：可以手动控制连接状态。

### 2. 任务列表与一键导入

配置面板中可以添加多条“下单任务”。每条任务独立生效：
- 点击任务卡片右上角的 **“导入 cURL”**，粘贴你的目标 API 请求。
- 导入后，**API URL**、**Method**、**Headers** 和 **Body** 会被自动填好。

### 3. 变量模板替换

在任务的 `Body` 或 `API URL` 中，你可以使用双大括号变量。当上游 WS 发来如下信号时：
```json
{"type":"signal","orderID":123,"action":"buy","timestamp":"2026-04-14 15:04:05"}
```

系统会提取这些字段并替换你配置中的变量：
- `{{action}}`：会被替换为上游传来的动作（如 `buy`、`sell`）。
- `{{orderID}}`：会被替换为信号中的订单号（如 `123`）。
- `{{amount}}` / `{{unit}}`：也会从信号中提取并替换。

**自定义 action 映射值：**
如果你的下游 API 要求 `buy` 时传 `1`，`sell` 时传 `2`，你只需在任务底部的映射框中配置：
- `当 action=buy 时替换为`: **1**
- `当 action=sell 时替换为`: **2**

### 4. 单任务独立测试

配置好任务后，你可以直接点击任务卡片下方的 **“测试 LONG”** 或 **“测试 SHORT”**。
系统会使用该任务的配置（包含特定的代理和 Headers）发起一次真实的 HTTP 请求，方便你验证鉴权 Token 是否过期或参数格式是否正确。

*(注：单任务测试的日志只会在控制台和 Web 面板显示，带有 `[TEST]` 标记，不会污染 `data/order.log` 审计文件)*

## 信号 JSON 格式与处理逻辑

### 上游信号格式

```json
{
  "type": "signal",
  "orderID": 123456,
  "action": "buy",
  "timestamp": "2026-04-14 15:04:05"
}
```

### 处理逻辑
1. 收到信号后，系统遍历所有**已启用**的任务。
2. 如果该任务配置了 `跳过前 N 次信号`，且仍在跳过配额内，则忽略本次执行（**30 分钟内无信号则重置跳过计数**）。
3. 否则，将信号中的 `action` 和 `orderID` 注入到任务的 Body/URL 模板中。
4. 从复用连接池中获取 HTTP Client（若配有代理则走代理），并发起最终的请求。

## 注意事项

- 本服务引入了 `http.Client` 连接池机制，极大降低了高频下单时的 TCP 握手延迟并避免了端口泄漏。
- SSE 日志流包含了自动清理机制，页面在断线重连时会自动清空陈旧日志，确保监控面板的流畅与准确。
- 本项目涉及自动化交易与资产操作，请在投入生产环境前，使用测试账号或小额资金进行充分的验证。
