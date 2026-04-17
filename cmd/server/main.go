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
	wsCli := wsclient.NewClient(cfgManager, logger, processor)
	wsSrv := wsserver.NewServer(logger, processor)

	wsCli.Start()

	mux := http.NewServeMux()

	mux.HandleFunc("/api/config", handleConfig(cfgManager, logger, wsCli, orderClient))
	mux.HandleFunc("/api/ws/connect", handleWSConnect(cfgManager, logger, wsCli))
	mux.HandleFunc("/api/ws/disconnect", handleWSDisconnect(cfgManager, logger, wsCli))
	mux.HandleFunc("/api/ws/status", handleWSStatus(wsCli))
	mux.HandleFunc("/api/tasks/test", handleTestTask(cfgManager, logger, orderClient))
	mux.HandleFunc("/api/logs/stream", handleLogsStream(logger))
	mux.HandleFunc("/ws/connect", wsSrv.HandleWS)

	fileServer := http.FileServer(http.Dir("web"))
	mux.Handle("/", fileServer)

	cfg := cfgManager.Get()
	addr := fmt.Sprintf(":%d", cfg.Server.Port)

	srv := &http.Server{
		Addr:         addr,
		Handler:      mux,
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
	wsCli.Stop()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
}

func handleConfig(cfgMgr *config.Manager, logger *logs.Logger, wsCli *wsclient.Client, orderClient *order.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")

		switch r.Method {
		case http.MethodGet:
			cfg := cfgMgr.Get()
			_ = json.NewEncoder(w).Encode(cfg)
		case http.MethodPost:
			var payload struct {
				Upstream config.UpstreamConfig `json:"upstream"`
				Tasks    []config.TaskConfig   `json:"tasks"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"invalid json"}`))
				return
			}

			if payload.Tasks == nil {
				payload.Tasks = []config.TaskConfig{}
			}
			if err := cfgMgr.Update(func(c *config.Config) {
				c.Upstream = payload.Upstream
				c.Tasks = payload.Tasks
			}); err != nil {
				logger.Error("config", fmt.Sprintf("update config error: %v", err))
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"error":"save failed"}`))
				return
			}

			// Flush http client cache when config changes
			orderClient.ClearCache()

			logger.Info("config", "config updated via API")
			wsCli.ForceDisconnect()
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}
}

func handleWSConnect(cfgMgr *config.Manager, logger *logs.Logger, wsCli *wsclient.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		_ = cfgMgr.Update(func(c *config.Config) {
			c.Upstream.Enabled = true
		})
		logger.Info("wsclient", "manual connect requested")
		wsCli.ForceDisconnect()
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}
}

func handleWSDisconnect(cfgMgr *config.Manager, logger *logs.Logger, wsCli *wsclient.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		_ = cfgMgr.Update(func(c *config.Config) {
			c.Upstream.Enabled = false
		})
		logger.Info("wsclient", "manual disconnect requested")
		wsCli.ForceDisconnect()
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}
}

func handleWSStatus(wsCli *wsclient.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		status := map[string]any{
			"connected": wsCli.IsConnected(),
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
