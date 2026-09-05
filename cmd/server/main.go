package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/kykira/ws-order-go/internal/config"
	"github.com/kykira/ws-order-go/internal/logs"
	"github.com/kykira/ws-order-go/internal/order"
	"github.com/kykira/ws-order-go/internal/signals"
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

	orderClient := order.NewClient(logger)
	processor := signals.NewProcessor(cfgManager, logger, orderClient)
	wsMgr := wsclient.NewManager(cfgManager, logger, processor)
	wsSrv := wsserver.NewServer(cfgManager, logger, processor)

	// 同步上游配置并启动连接
	wsMgr.Sync()

	mux := http.NewServeMux()

	mux.HandleFunc("/api/config", handleConfig(cfgManager, logger, wsMgr, wsSrv, orderClient))
	mux.HandleFunc("/api/ws/connect", handleWSConnect(cfgManager, logger, wsMgr))
	mux.HandleFunc("/api/ws/disconnect", handleWSDisconnect(cfgManager, logger, wsMgr))
	mux.HandleFunc("/api/ws/status", handleWSStatus(wsMgr, wsSrv))
	mux.HandleFunc("/api/tasks/test", handleTestTask(cfgManager, logger, orderClient))
	mux.HandleFunc("/api/logs/stream", handleLogsStream(logger))
	mux.HandleFunc("/api/login", handleLogin(cfgManager))

	// WS server endpoint — path is configurable
	cfg := cfgManager.Get()
	wsPath := cfg.WSServer.Path
	if wsPath == "" {
		wsPath = "/ws"
	}
	if cfg.WSServer.Enabled {
		mux.HandleFunc(wsPath, wsSrv.HandleWS)
		logger.Info("main", fmt.Sprintf("WS server listening on %s", wsPath))
	}

	fileServer := http.FileServer(http.Dir("web"))
	mux.Handle("/", fileServer)

	addr := fmt.Sprintf(":%d", cfg.Server.Port)

	srv := &http.Server{
		Addr:         addr,
		Handler:      authMiddleware(mux, cfgManager),
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
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
	wsMgr.StopAll()
	processor.Stop()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
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
			if err := config.PrepareTasks(payload.Tasks); err != nil {
				logger.Error("config", fmt.Sprintf("reject invalid config payload: %v", err))
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(fmt.Sprintf(`{"error":%q}`, err.Error())))
				return
			}
			if err := cfgMgr.Update(func(c *config.Config) {
				c.Upstreams = payload.Upstreams
				c.WSServer = payload.WSServer
				c.Dispatch = payload.Dispatch
				c.Tasks = payload.Tasks
				c.Strategies = payload.Strategies
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
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		_ = cfgMgr.Update(func(c *config.Config) {
			// Multi-upstream: Sync will handle
		})
		logger.Info("wsclient", "manual connect requested")
		wsMgr.Sync()
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}
}

func handleWSDisconnect(cfgMgr *config.Manager, logger *logs.Logger, wsMgr *wsclient.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		_ = cfgMgr.Update(func(c *config.Config) {
			// Multi-upstream: Sync will handle
		})
		logger.Info("wsclient", "manual disconnect requested")
		wsMgr.Sync()
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
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

		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
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

		for _, e := range logger.Entries() {
			_, _ = fmt.Fprintf(w, "data: %s\n\n", e.JSON())
		}
		flusher.Flush()

		ch, cancel := logger.AddListener()
		defer cancel()

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
