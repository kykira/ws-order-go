package wsserver

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/gorilla/websocket"

	"wsordergo/internal/logs"
	"wsordergo/internal/signals"
)

type Server struct {
	logger    *logs.Logger
	processor *signals.Processor

	upgrader websocket.Upgrader
}

func NewServer(logger *logs.Logger, processor *signals.Processor) *Server {
	return &Server{
		logger:    logger,
		processor: processor,
		upgrader: websocket.Upgrader{
			ReadBufferSize:  1024,
			WriteBufferSize: 1024,
			CheckOrigin: func(r *http.Request) bool {
				return true
			},
		},
	}
}

func (s *Server) HandleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		s.logger.Error("wsserver", fmt.Sprintf("upgrade error: %v", err))
		return
	}

	s.logger.Info("wsserver", fmt.Sprintf("client connected from %s", r.RemoteAddr))
	defer func() {
		_ = conn.Close()
		s.logger.Info("wsserver", "client disconnected")
	}()

	conn.SetReadLimit(1024 * 1024)

	for {
		mt, message, err := conn.ReadMessage()
		if err != nil {
			return
		}
		if mt != websocket.TextMessage {
			continue
		}

		text := strings.TrimSpace(string(message))
		if text == "ping" {
			s.logger.Debug("wsserver", "received ping, sending pong")
			_ = conn.WriteMessage(websocket.TextMessage, []byte("pong"))
			continue
		}

		var sig signals.Signal
		if err := json.Unmarshal(message, &sig); err != nil {
			s.logger.Error("wsserver", fmt.Sprintf("invalid json: %v", err))
			continue
		}

		if err := s.processor.Handle("ws-server", sig, false); err != nil {
			s.logger.Error("wsserver", fmt.Sprintf("handle signal error: %v", err))
		}
	}
}
