package wsserver

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"github.com/kykira/ws-order-go/internal/config"
	"github.com/kykira/ws-order-go/internal/logs"
	"github.com/kykira/ws-order-go/internal/signals"
)

type Server struct {
	cfg       *config.Manager
	logger    *logs.Logger
	processor *signals.Processor

	upgrader websocket.Upgrader

	connMu    sync.Mutex
	conns     map[*websocket.Conn]struct{}
	connCount atomic.Int64
}

func NewServer(cfg *config.Manager, logger *logs.Logger, processor *signals.Processor) *Server {
	return &Server{
		cfg:       cfg,
		logger:    logger,
		processor: processor,
		upgrader: websocket.Upgrader{
			ReadBufferSize:  1024,
			WriteBufferSize: 1024,
			CheckOrigin: func(r *http.Request) bool {
				return true
			},
		},
		conns: make(map[*websocket.Conn]struct{}),
	}
}

// ConnCount returns the number of currently connected WS clients.
func (s *Server) ConnCount() int64 {
	return s.connCount.Load()
}

func (s *Server) addConn(conn *websocket.Conn) {
	s.connMu.Lock()
	s.conns[conn] = struct{}{}
	s.connMu.Unlock()
	s.connCount.Add(1)
	s.logger.Info("wsserver", fmt.Sprintf("client connected (total: %d)", s.connCount.Load()))
}

func (s *Server) removeConn(conn *websocket.Conn) {
	s.connMu.Lock()
	delete(s.conns, conn)
	s.connMu.Unlock()
	s.connCount.Add(-1)
	s.logger.Info("wsserver", fmt.Sprintf("client disconnected (total: %d)", s.connCount.Load()))
}

func (s *Server) HandleWS(w http.ResponseWriter, r *http.Request) {
	// Optional key verification
	cfg := s.cfg.Get()
	if cfg.WSServer.Key != "" {
		key := r.URL.Query().Get("key")
		if key != cfg.WSServer.Key {
			s.logger.Error("wsserver", fmt.Sprintf("auth failed from %s: invalid key", r.RemoteAddr))
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte("unauthorized: invalid key"))
			return
		}
	}

	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		s.logger.Error("wsserver", fmt.Sprintf("upgrade error from %s: %v", r.RemoteAddr, err))
		return
	}

	s.addConn(conn)
	defer func() {
		s.removeConn(conn)
		_ = conn.Close()
	}()

	conn.SetReadLimit(1024 * 1024)

	// Heartbeat: read deadline + ping/pong handler
	pongWait := 90 * time.Second
	pingPeriod := (pongWait * 8) / 10 // send ping at 80% of pongWait

	conn.SetReadDeadline(time.Now().Add(pongWait))
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(pongWait))
		return nil
	})

	// Ping ticker goroutine
	pingDone := make(chan struct{})
	defer close(pingDone)
	go func() {
		ticker := time.NewTicker(pingPeriod)
		defer ticker.Stop()
		for {
			select {
			case <-pingDone:
				return
			case <-ticker.C:
				if err := conn.WriteControl(websocket.PingMessage, []byte{}, time.Now().Add(10*time.Second)); err != nil {
					return
				}
			}
		}
	}()

	// Determine applySkip from config
	applySkip := cfg.WSServer.ApplySkip

	// Read loop
	for {
		mt, message, err := conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseNormalClosure) {
				s.logger.Error("wsserver", fmt.Sprintf("read error: %v", err))
			}
			return
		}

		conn.SetReadDeadline(time.Now().Add(pongWait))

		if mt != websocket.TextMessage {
			continue
		}

		text := strings.TrimSpace(string(message))
		if text == "ping" {
			s.logger.Debug("wsserver", "received text ping, sending pong")
			_ = conn.WriteMessage(websocket.TextMessage, []byte("pong"))
			continue
		}

		var sig signals.Signal
		if err := json.Unmarshal(message, &sig); err != nil {
			s.logger.Error("wsserver", fmt.Sprintf("invalid json from %s: %v", r.RemoteAddr, err))
			continue
		}

		s.logger.Info("wsserver", fmt.Sprintf("signal from %s: action=%s symbol=%s orderID=%d", r.RemoteAddr, sig.Action, sig.Symbol, sig.OrderID))

		if err := s.processor.Handle("wsserver", sig, applySkip); err != nil {
			s.logger.Error("wsserver", fmt.Sprintf("handle signal error: %v", err))
		}
	}
}
