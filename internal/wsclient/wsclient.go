package wsclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"wsordergo/internal/config"
	"wsordergo/internal/logs"
	"wsordergo/internal/signals"
)

type Client struct {
	cfg       *config.Manager
	logger    *logs.Logger
	processor *signals.Processor

	mu   sync.Mutex
	conn *websocket.Conn

	ctx    context.Context
	cancel context.CancelFunc

	connected bool
}

func NewClient(cfg *config.Manager, logger *logs.Logger, processor *signals.Processor) *Client {
	ctx, cancel := context.WithCancel(context.Background())
	return &Client{
		cfg:       cfg,
		logger:    logger,
		processor: processor,
		ctx:       ctx,
		cancel:    cancel,
	}
}

func (c *Client) Start() {
	go c.loop()
}

func (c *Client) Stop() {
	c.cancel()
	c.ForceDisconnect()
}

func (c *Client) IsConnected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.connected
}

func (c *Client) ForceDisconnect() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn != nil {
		_ = c.conn.Close()
		c.conn = nil
		c.connected = false
	}
}

func (c *Client) loop() {
	backoff := 2 * time.Second
	for {
		select {
		case <-c.ctx.Done():
			c.logger.Info("wsclient", "stopped")
			return
		default:
		}

		cfg := c.cfg.Get()
		if !cfg.Upstream.Enabled || cfg.Upstream.WSUrl == "" {
			time.Sleep(2 * time.Second)
			continue
		}

		wsURL, err := buildWSURL(cfg.Upstream.WSUrl, cfg.Upstream.WSKey)
		if err != nil {
			c.logger.Error("wsclient", fmt.Sprintf("invalid ws url: %v", err))
			time.Sleep(backoff)
			continue
		}

		c.logger.Info("wsclient", fmt.Sprintf("connecting to %s", wsURL))

		conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
		if err != nil {
			c.logger.Error("wsclient", fmt.Sprintf("dial error: %v", err))
			time.Sleep(backoff)
			if backoff < 30*time.Second {
				backoff *= 2
			}
			continue
		}

		c.mu.Lock()
		c.conn = conn
		c.connected = true
		c.mu.Unlock()

		c.logger.Info("wsclient", "connected")
		backoff = 2 * time.Second

		if err := c.readLoop(conn); err != nil {
			// Don't log normal closure as error
			if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				c.logger.Info("wsclient", "connection closed normally")
			} else {
				c.logger.Error("wsclient", fmt.Sprintf("read error: %v", err))
			}
		}

		c.mu.Lock()
		if c.conn == conn {
			c.conn = nil
			c.connected = false
		}
		c.mu.Unlock()

		c.logger.Info("wsclient", "disconnected, will reconnect")
	}
}

func buildWSURL(raw, key string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	if key != "" {
		q := u.Query()
		q.Set("key", key)
		u.RawQuery = q.Encode()
	}
	return u.String(), nil
}

func (c *Client) readLoop(conn *websocket.Conn) error {
	defer conn.Close()
	conn.SetReadLimit(1024 * 1024)

	// Set a default read deadline and ping handler
	pingPeriod := 60 * time.Second
	pongWait := 90 * time.Second

	conn.SetReadDeadline(time.Now().Add(pongWait))
	conn.SetPingHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(pongWait))
		err := conn.WriteControl(websocket.PongMessage, []byte{}, time.Now().Add(10*time.Second))
		if err != nil && err != websocket.ErrCloseSent {
			return err
		}
		return nil
	})

	// Optional: start a ticker to send Pings proactively if upstream requires it.
	// But usually we just reply to their Pings. If we need to send Pings, we'd do it in a goroutine.
	go func() {
		ticker := time.NewTicker(pingPeriod)
		defer ticker.Stop()
		for {
			select {
			case <-c.ctx.Done():
				return
			case <-ticker.C:
				c.mu.Lock()
				currentConn := c.conn
				c.mu.Unlock()
				
				if currentConn != conn {
					return
				}
				
				if err := conn.WriteControl(websocket.PingMessage, []byte{}, time.Now().Add(10*time.Second)); err != nil {
					return
				}
			}
		}
	}()

	for {
		mt, message, err := conn.ReadMessage()
		if err != nil {
			return err
		}
		conn.SetReadDeadline(time.Now().Add(pongWait))

		if mt != websocket.TextMessage {
			continue
		}

		text := strings.TrimSpace(string(message))
		if text == "ping" {
			c.logger.Debug("wsclient", "received ping, sending pong")
			_ = conn.WriteMessage(websocket.TextMessage, []byte("pong"))
			continue
		}

		var sig signals.Signal
		if err := json.Unmarshal(message, &sig); err != nil {
			c.logger.Error("wsclient", fmt.Sprintf("invalid json: %v", err))
			continue
		}

		if err := c.processor.Handle("upstream", sig, true); err != nil {
			c.logger.Error("wsclient", fmt.Sprintf("handle signal error: %v", err))
		}
	}
}
