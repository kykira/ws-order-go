package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/kykira/ws-order-go/internal/balance"
	"github.com/kykira/ws-order-go/internal/config"
	"github.com/kykira/ws-order-go/internal/logs"
	"github.com/kykira/ws-order-go/internal/order"
	"github.com/kykira/ws-order-go/internal/orders"
	"github.com/kykira/ws-order-go/internal/settle"
	"github.com/kykira/ws-order-go/internal/signals"
	"github.com/kykira/ws-order-go/internal/tfrealtime"
	"github.com/kykira/ws-order-go/internal/wsclient"
	"github.com/kykira/ws-order-go/internal/wsserver"
)

func main() {
	cfgManager, err := config.LoadManager("config.json")
	if err != nil {
		log.Fatalf("load config failed: %v", err)
	}

	logger := logs.NewLogger(500)
	logger.Info("main", "starting ws-order bridge service")

	orderStore := orders.NewPersistentStore(200, filepath.Join("data", "orders.json"))
	orderClient := order.NewClient(logger)
	orderClient.SetRecorder(orderStore)
	balanceSvc := balance.NewService(cfgManager, logger)
	tfRealtime := tfrealtime.NewService(cfgManager, logger)
	settleSvc := settle.NewService(cfgManager, logger, orderStore)
	processor := signals.NewProcessor(cfgManager, logger, orderClient, balanceSvc)
	processor.SetOddsProvider(tfRealtime)
	wsMgr := wsclient.NewManager(cfgManager, logger, processor)
	wsSrv := wsserver.NewServer(cfgManager, logger, processor)

	// 同步上游配置并启动连接
	wsMgr.Sync()
	// 启动币安账号余额拉取（首次立即拉取，之后每分钟一次）
	balanceSvc.Start()
	// 启动 TurboFlow 实时赔率（evt_cfg_*）订阅
	tfRealtime.Start()
	// 启动币安事件合约结算对账（胜负/盈亏）
	settleSvc.Start()

	mux := http.NewServeMux()

	mux.HandleFunc("/api/config", handleConfig(cfgManager, logger, wsMgr, wsSrv, orderClient))
	mux.HandleFunc("/api/balances", handleBalances(balanceSvc))
	mux.HandleFunc("/api/orders", handleOrders(orderStore))
	mux.HandleFunc("/api/settlements", handleSettlements(settleSvc))
	mux.HandleFunc("/api/balances/summary", handleBalancesSummary(balanceSvc))
	mux.HandleFunc("/api/tf/realtime", handleTFRealtime(tfRealtime))
	mux.HandleFunc("/api/ws/connect", handleWSConnect(cfgManager, logger, wsMgr))
	mux.HandleFunc("/api/ws/disconnect", handleWSDisconnect(cfgManager, logger, wsMgr))
	mux.HandleFunc("/api/ws/status", handleWSStatus(wsMgr, wsSrv))
	mux.HandleFunc("/api/tasks/test", handleTestTask(cfgManager, logger, orderClient))
	mux.HandleFunc("/api/logs/stream", handleLogsStream(logger))
	mux.HandleFunc("/api/login", handleLogin(cfgManager))

	// The WS signal endpoint is resolved on every request so toggling
	// wsServer.enabled (or editing its path) takes effect without a restart,
	// and so a path colliding with an existing route can never panic the mux.
	cfg := cfgManager.Get()
	if cfg.WSServer.Enabled {
		wsPath := cfg.WSServer.Path
		if wsPath == "" {
			wsPath = "/ws"
		}
		logger.Info("main", fmt.Sprintf("WS server listening on %s", wsPath))
	}

	fileServer := http.FileServer(http.Dir("web"))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		ws := cfgManager.Get().WSServer
		wsPath := ws.Path
		if wsPath == "" {
			wsPath = "/ws"
		}
		if ws.Enabled && r.URL.Path == wsPath {
			wsSrv.HandleWS(w, r)
			return
		}
		// 静态文件每次都协商缓存：app.js 改动后普通刷新即可生效，不会被
		// 浏览器按启发式规则长期缓存住（未改动时仍然返回 304）。
		w.Header().Set("Cache-Control", "no-cache")
		fileServer.ServeHTTP(w, r)
	})

	port := cfg.Server.Port
	if port == 0 {
		port = 9946
	}
	addr := fmt.Sprintf(":%d", port)

	srv := &http.Server{
		Addr:         addr,
		Handler:      authMiddleware(mux, cfgManager),
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		// Slowloris protection: a stalled header read must not hold a
		// connection (and its file descriptor) indefinitely.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	serverErrCh := make(chan error, 1)
	go func() {
		logger.Info("http", fmt.Sprintf("listening on http://localhost%s", addr))
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("http", fmt.Sprintf("server error: %v", err))
			serverErrCh <- err
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	select {
	case <-quit:
	case err := <-serverErrCh:
		log.Fatalf("server listen failed on %s: %v", addr, err)
	}

	logger.Info("main", "shutting down...")
	// Stop accepting new signals first, then release the background services.
	wsSrv.CloseAll()
	wsMgr.StopAll()
	balanceSvc.Stop()
	tfRealtime.Stop()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)

	// Let order requests that were already submitted finish instead of cutting
	// them off mid-flight.
	processor.Stop()
	settleSvc.Stop()
	orderStore.Close() // final snapshot of the order table
}

// authMiddleware 保护 /api/* 接口（登录接口除外）。静态页面与 WS 信号端点不在此列。
func authMiddleware(next http.Handler, cfgMgr *config.Manager) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/") || r.URL.Path == "/api/login" {
			next.ServeHTTP(w, r)
			return
		}
		pwd := cfgMgr.Get().Server.Password
		if pwd == "" {
			next.ServeHTTP(w, r)
			return
		}
		if c, err := r.Cookie("wsorder_auth"); err == nil && c.Value == pwd {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
	})
}

func handleLogin(cfgMgr *config.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var payload struct {
			Password string `json:"password"`
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid json"}`))
			return
		}
		pwd := cfgMgr.Get().Server.Password
		if pwd == "" || payload.Password != pwd {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"wrong password"}`))
			return
		}
		http.SetCookie(w, &http.Cookie{
			Name:     "wsorder_auth",
			Value:    pwd,
			Path:     "/",
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
		})
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}
}

func handleBalances(svc *balance.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		_ = json.NewEncoder(w).Encode(svc.GetBalances())
	}
}

// handleOrders serves the order-record table data, newest first.
func handleOrders(store *orders.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		limit := 200
		if v := strings.TrimSpace(r.URL.Query().Get("limit")); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				limit = n
			}
		}
		_ = json.NewEncoder(w).Encode(store.List(limit))
	}
}

// handleSettlements serves the Binance settlement stats (last 24h).
func handleSettlements(svc *settle.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		_ = json.NewEncoder(w).Encode(svc.Snapshot())
	}
}

func handleBalancesSummary(svc *balance.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		_ = json.NewEncoder(w).Encode(svc.Summary())
	}
}

func handleTFRealtime(svc *tfrealtime.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		_ = json.NewEncoder(w).Encode(svc.GetSnapshot())
	}
}

func handleConfig(cfgMgr *config.Manager, logger *logs.Logger, wsMgr *wsclient.Manager, wsSrv *wsserver.Server, orderClient *order.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")

		switch r.Method {
		case http.MethodGet:
			cfg := cfgMgr.Get()
			_ = json.NewEncoder(w).Encode(cfg)
		case http.MethodPost:
			var payload struct {
				Upstreams  []config.UpstreamConfig `json:"upstreams"`
				WSServer   config.WSServerConfig   `json:"wsServer"`
				Dispatch   string                  `json:"dispatch"`
				Tasks      []config.TaskConfig     `json:"tasks"`
				Strategies []config.StrategyConfig `json:"strategies"`
			}
			r.Body = http.MaxBytesReader(w, r.Body, 4<<20)
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"invalid json"}`))
				return
			}

			if payload.Tasks == nil {
				payload.Tasks = []config.TaskConfig{}
			}
			if payload.Strategies == nil {
				payload.Strategies = []config.StrategyConfig{}
			}

			// Validate and normalize the whole payload (dispatch included)
			// before touching the live config, so a typo can never be
			// persisted and then silently interpreted as "order on all".
			next := config.Config{
				Upstreams:  payload.Upstreams,
				WSServer:   payload.WSServer,
				Dispatch:   payload.Dispatch,
				Tasks:      payload.Tasks,
				Strategies: payload.Strategies,
			}
			if err := config.Prepare(&next); err != nil {
				logger.Error("config", fmt.Sprintf("reject invalid config payload: %v", err))
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(fmt.Sprintf(`{"error":%q}`, err.Error())))
				return
			}
			if err := cfgMgr.Update(func(c *config.Config) {
				c.Upstreams = next.Upstreams
				c.WSServer = next.WSServer
				c.Dispatch = next.Dispatch
				c.Tasks = next.Tasks
				c.Strategies = next.Strategies
			}); err != nil {
				logger.Error("config", fmt.Sprintf("update config error: %v", err))
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"error":"save failed"}`))
				return
			}

			// Flush http client cache when config changes
			orderClient.ClearCache()

			logger.Info("config", "config updated via API")
			wsMgr.Sync()
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}
}

func handleWSConnect(cfgMgr *config.Manager, logger *logs.Logger, wsMgr *wsclient.Manager) http.HandlerFunc {
	return setUpstreamsEnabled(cfgMgr, logger, wsMgr, true, "connect")
}

func handleWSDisconnect(cfgMgr *config.Manager, logger *logs.Logger, wsMgr *wsclient.Manager) http.HandlerFunc {
	return setUpstreamsEnabled(cfgMgr, logger, wsMgr, false, "disconnect")
}

// setUpstreamsEnabled turns one upstream (or every upstream when no id is
// given) on or off, persists the change and resyncs. The previous handlers were
// byte-identical no-ops that rewrote config.json and reported success without
// changing anything, so "disconnect" could never disconnect.
func setUpstreamsEnabled(cfgMgr *config.Manager, logger *logs.Logger, wsMgr *wsclient.Manager, enabled bool, verb string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		var payload struct {
			ID string `json:"id"`
		}
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
			_ = json.NewDecoder(r.Body).Decode(&payload) // body is optional
		}
		id := strings.TrimSpace(payload.ID)

		changed := 0
		err := cfgMgr.Update(func(c *config.Config) {
			for i := range c.Upstreams {
				if id != "" && c.Upstreams[i].ID != id {
					continue
				}
				c.Upstreams[i].Enabled = enabled
				changed++
			}
		})
		if err != nil {
			logger.Error("wsclient", fmt.Sprintf("manual %s failed: %v", verb, err))
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"save failed"}`))
			return
		}
		if id != "" && changed == 0 {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"upstream not found"}`))
			return
		}

		logger.Info("wsclient", fmt.Sprintf("manual %s requested (id=%q, upstreams=%d)", verb, id, changed))
		wsMgr.Sync()
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}
}

func handleWSStatus(wsMgr *wsclient.Manager, wsSrv *wsserver.Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		upstreamStatus := wsMgr.Status()
		status := map[string]any{
			"connected":     wsMgr.IsConnected(),
			"wsServerConns": wsSrv.ConnCount(),
			"upstream":      upstreamStatus,
		}
		_ = json.NewEncoder(w).Encode(status)
	}
}

func handleTestTask(cfgMgr *config.Manager, logger *logs.Logger, orderClient *order.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")

		var payload struct {
			TaskID string `json:"taskId"`
			Action string `json:"action"`
			Symbol string `json:"symbol"`
			Period string `json:"period"`
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid json"}`))
			return
		}
		taskID := strings.TrimSpace(payload.TaskID)
		if taskID == "" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"taskId required"}`))
			return
		}

		action := strings.ToLower(strings.TrimSpace(payload.Action))
		if action != "buy" && action != "sell" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"action must be buy or sell"}`))
			return
		}

		symbol := strings.TrimSpace(payload.Symbol)
		if symbol == "" {
			symbol = "BTCUSDT" // Default for testing if empty
		}
		period := strings.TrimSpace(payload.Period)
		if period == "" {
			period = "5m"
		}

		cfg := cfgMgr.Get()
		task, ok := findTask(cfg.Tasks, taskID)
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"task not found"}`))
			return
		}
		if !task.Enabled {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"task disabled"}`))
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second+order.MaxRandomDelay(task))
		defer cancel()
		if err := orderClient.PlaceOrder(ctx, task, order.PlaceOrderRequest{
			Amount: "5",
			Unit:   "TEN_MINUTE",
			Action: action,
			Symbol: symbol,
			Period: period,
			IsTest: true,
		}); err != nil {
			logger.Error("test", fmt.Sprintf("test task order error task=[%s]: %v", task.Name, err))
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"error":"order request failed"}`))
			return
		}

		logger.Info("test", fmt.Sprintf("test task order sent task=[%s] action=%s", task.Name, action))
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}
}

func findTask(tasks []config.TaskConfig, id string) (config.TaskConfig, bool) {
	for _, t := range tasks {
		if t.ID == id {
			return t, true
		}
	}
	return config.TaskConfig{}, false
}

func handleLogsStream(logger *logs.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("streaming unsupported"))
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")

		// The server's 15s WriteTimeout would otherwise kill this long-lived
		// stream (and make the UI reload the whole log pane every 15s).
		if err := http.NewResponseController(w).SetWriteDeadline(time.Time{}); err != nil {
			logger.Debug("main", fmt.Sprintf("cannot clear write deadline for log stream: %v", err))
		}

		// Subscribe BEFORE replaying history, so an entry logged during the
		// replay cannot fall into the gap between snapshot and subscription.
		ch, cancel := logger.AddListener()
		defer cancel()

		for _, e := range logger.Entries() {
			_, _ = fmt.Fprintf(w, "data: %s\n\n", e.JSON())
		}
		flusher.Flush()

		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()

		ctx := r.Context()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if _, err := fmt.Fprintf(w, ": keepalive\n\n"); err != nil {
					return
				}
				flusher.Flush()
			case e, ok := <-ch:
				if !ok {
					return
				}
				if _, err := fmt.Fprintf(w, "data: %s\n\n", e.JSON()); err != nil {
					return
				}
				flusher.Flush()
			}
		}
	}
}
