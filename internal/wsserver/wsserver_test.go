package wsserver

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/kykira/ws-order-go/internal/config"
	"github.com/kykira/ws-order-go/internal/logs"
	"github.com/kykira/ws-order-go/internal/order"
	"github.com/kykira/ws-order-go/internal/signals"
)

func TestServerConnCount(t *testing.T) {
	logger := logs.NewLogger(50)
	cfgMgr := createTestConfigManager(t)
	// Use real order client to avoid nil pointer in async PlaceOrder goroutine
	orderClient := order.NewClient(logger)
	processor := signals.NewProcessor(cfgMgr, logger, orderClient)
	srv := NewServer(cfgMgr, logger, processor)

	if srv.ConnCount() != 0 {
		t.Fatalf("initial ConnCount should be 0, got %d", srv.ConnCount())
	}

	ts := httptest.NewServer(http.HandlerFunc(srv.HandleWS))
	defer ts.Close()

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"

	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	time.Sleep(50 * time.Millisecond)

	if count := srv.ConnCount(); count != 1 {
		t.Fatalf("expected ConnCount=1 after connect, got %d", count)
	}

	conn.Close()
	time.Sleep(50 * time.Millisecond)

	if count := srv.ConnCount(); count != 0 {
		t.Fatalf("expected ConnCount=0 after disconnect, got %d", count)
	}
}

func TestServerPingPong(t *testing.T) {
	logger := logs.NewLogger(50)
	cfgMgr := createTestConfigManager(t)
	orderClient := order.NewClient(logger)
	processor := signals.NewProcessor(cfgMgr, logger, orderClient)
	srv := NewServer(cfgMgr, logger, processor)

	ts := httptest.NewServer(http.HandlerFunc(srv.HandleWS))
	defer ts.Close()

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"

	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// Send text "ping"
	if err := conn.WriteMessage(websocket.TextMessage, []byte("ping")); err != nil {
		t.Fatalf("write ping: %v", err)
	}

	// Read "pong"
	_, pong, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read pong: %v", err)
	}
	if strings.TrimSpace(string(pong)) != "pong" {
		t.Fatalf("expected 'pong', got %q", string(pong))
	}
}

func TestServerAuth(t *testing.T) {
	logger := logs.NewLogger(50)
	cfgMgr := createTestConfigManager(t)
	_ = cfgMgr.Update(func(c *config.Config) {
		c.WSServer.Key = "secret123"
	})

	orderClient := order.NewClient(logger)
	processor := signals.NewProcessor(cfgMgr, logger, orderClient)
	srv := NewServer(cfgMgr, logger, processor)

	ts := httptest.NewServer(http.HandlerFunc(srv.HandleWS))
	defer ts.Close()

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"

	// Without key — should fail
	_, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err == nil {
		t.Fatal("expected auth failure without key")
	}

	// With key — should succeed
	conn, _, err := websocket.DefaultDialer.Dial(wsURL+"?key=secret123", nil)
	if err != nil {
		t.Fatalf("dial with key: %v", err)
	}
	conn.Close()
}

func createTestConfigManager(t *testing.T) *config.Manager {
	t.Helper()
	dir := t.TempDir()
	mgr, err := config.LoadManager(dir + "/config.json")
	if err != nil {
		t.Fatalf("create config manager: %v", err)
	}
	return mgr
}
