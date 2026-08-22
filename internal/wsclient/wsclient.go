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

	"github.com/kykira/ws-order-go/internal/config"
	"github.com/kykira/ws-order-go/internal/logs"
	"github.com/kykira/ws-order-go/internal/signals"
)

// Conn 代表一条到上游的 WS 连接
type Conn struct {
	ID   string
	Name string
	URL  string
	Key  string

	mu        sync.Mutex
	conn      *websocket.Conn
	connected bool

	ctx    context.Context
	cancel context.CancelFunc
}

// Manager 管理所有上游 WS 连接
type Manager struct {
	cfg       *config.Manager
	logger    *logs.Logger
	processor *signals.Processor

	mu    sync.Mutex
	conns map[string]*Conn // id → Conn
}

func NewManager(cfg *config.Manager, logger *logs.Logger, processor *signals.Processor) *Manager {
	return &Manager{
		cfg:       cfg,
		logger:    logger,
		processor: processor,
		conns:     make(map[string]*Conn),
	}
}

// Sync 同步配置，启动新增的连接，停止移除的连接
func (m *Manager) Sync() {
	cfg := m.cfg.Get()

	m.mu.Lock()
	defer m.mu.Unlock()

	// 构建配置中的上游 ID 集合
	want := make(map[string]config.UpstreamConfig)
	for _, u := range cfg.Upstreams {
		if u.ID == "" {
			continue
		}
		want[u.ID] = u
	}

	// 停止已移除的上游
	for id, c := range m.conns {
		if _, ok := want[id]; !ok {
			m.logger.Info("wsclient", fmt.Sprintf("removing upstream [%s] %s", id, c.Name))
			c.Stop()
			delete(m.conns, id)
		}
	}

	// 启动新增的或更新配置的上游
	for id, u := range want {
		existing, exists := m.conns[id]
		if exists {
			// 更新现有连接
			existing.mu.Lock()
			changed := existing.URL != u.WSUrl || existing.Key != u.WSKey || existing.Name != u.Name
			existing.Name = u.Name
			existing.URL = u.WSUrl
			existing.Key = u.WSKey
			existing.mu.Unlock()

			if changed {
				m.logger.Info("wsclient", fmt.Sprintf("upstream config changed, reconnecting [%s] %s", id, u.Name))
				existing.Stop()
				existing.Start(m.logger, m.processor)
			}

			// 根据 enabled 状态启停
			if u.Enabled && !existing.IsConnected() {
				existing.Start(m.logger, m.processor)
			} else if !u.Enabled && existing.IsConnected() {
				existing.Stop()
			}
		} else {
			// 新建连接
			c := &Conn{
				ID:   u.ID,
				Name: u.Name,
				URL:  u.WSUrl,
				Key:  u.WSKey,
			}
			m.conns[id] = c
			if u.Enabled && u.WSUrl != "" {
				m.logger.Info("wsclient", fmt.Sprintf("new upstream [%s] %s → %s", id, u.Name, u.WSUrl))
				c.Start(m.logger, m.processor)
			}
		}
	}
}

// StopAll 停止所有连接
func (m *Manager) StopAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, c := range m.conns {
		c.Stop()
		delete(m.conns, id)
	}
}

// Status 返回所有上游状态
func (m *Manager) Status() map[string]any {
	m.mu.Lock()
	defer m.mu.Unlock()

	items := make([]map[string]any, 0, len(m.conns))
	connected := 0
	for _, c := range m.conns {
		items = append(items, map[string]any{
			"id":        c.ID,
			"name":      c.Name,
			"url":       c.URL,
			"connected": c.IsConnected(),
		})
		if c.IsConnected() {
			connected++
		}
	}
	return map[string]any{
		"total":     len(m.conns),
		"connected": connected,
		"items":     items,
	}
}

// IsConnected 是否有上游已连接
func (m *Manager) IsConnected() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, c := range m.conns {
		if c.IsConnected() {
			return true
		}
	}
	return false
}

// ── Conn 方法 ──

func (c *Conn) IsConnected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.connected
}

func (c *Conn) Start(logger *logs.Logger, processor *signals.Processor) {
	if c.cancel != nil {
		c.cancel()
	}
	c.ctx, c.cancel = context.WithCancel(context.Background())
	go c.loop(logger, processor)
}

func (c *Conn) Stop() {
	if c.cancel != nil {
		c.cancel()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn != nil {
		_ = c.conn.Close()
		c.conn = nil
	}
	c.connected = false
}

func (c *Conn) ForceDisconnect() {
	c.Stop()
}

func (c *Conn) loop(logger *logs.Logger, processor *signals.Processor) {
	backoff := 2 * time.Second
	for {
		select {
		case <-c.ctx.Done():
			logger.Info("wsclient", fmt.Sprintf("upstream [%s] %s stopped", c.ID, c.Name))
			return
		default:
		}

		c.mu.Lock()
		wsURL := c.URL
		wsKey := c.Key
		c.mu.Unlock()

		if wsURL == "" {
			time.Sleep(2 * time.Second)
			continue
		}

		fullURL, err := buildWSURL(wsURL, wsKey)
		if err != nil {
			logger.Error("wsclient", fmt.Sprintf("[%s] invalid ws url: %v", c.ID, err))
			time.Sleep(backoff)
			continue
		}

		logger.Info("wsclient", fmt.Sprintf("[%s] %s connecting to %s", c.ID, c.Name, fullURL))

		conn, _, err := websocket.DefaultDialer.Dial(fullURL, nil)
		if err != nil {
			logger.Error("wsclient", fmt.Sprintf("[%s] dial error: %v", c.ID, err))
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

		logger.Info("wsclient", fmt.Sprintf("[%s] %s connected", c.ID, c.Name))
		backoff = 2 * time.Second

		if err := c.readLoop(conn, logger, processor); err != nil {
			if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				logger.Info("wsclient", fmt.Sprintf("[%s] connection closed normally", c.ID))
			} else {
				logger.Error("wsclient", fmt.Sprintf("[%s] read error: %v", c.ID, err))
			}
		}

		c.mu.Lock()
		if c.conn == conn {
			c.conn = nil
			c.connected = false
		}
		c.mu.Unlock()

		logger.Info("wsclient", fmt.Sprintf("[%s] disconnected, will reconnect", c.ID))
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

func (c *Conn) readLoop(conn *websocket.Conn, logger *logs.Logger, processor *signals.Processor) error {
	defer conn.Close()
	conn.SetReadLimit(1024 * 1024)

	pongWait := 90 * time.Second
	pingPeriod := 60 * time.Second

	conn.SetReadDeadline(time.Now().Add(pongWait))
	conn.SetPingHandler(func(data string) error {
		conn.SetReadDeadline(time.Now().Add(pongWait))
		err := conn.WriteControl(websocket.PongMessage, []byte(data), time.Now().Add(10*time.Second))
		if err != nil && err != websocket.ErrCloseSent {
			return err
		}
		return nil
	})

	// 主动 Ping
	go func() {
		ticker := time.NewTicker(pingPeriod)
		defer ticker.Stop()
		for {
			select {
			case <-c.ctx.Done():
				return
			case <-ticker.C:
				c.mu.Lock()
				current := c.conn
				c.mu.Unlock()
				if current != conn {
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
			_ = conn.WriteMessage(websocket.TextMessage, []byte("pong"))
			continue
		}

		var sig signals.Signal
		if err := json.Unmarshal(message, &sig); err != nil {
			logger.Error("wsclient", fmt.Sprintf("[%s] invalid json: %v", c.ID, err))
			continue
		}

		if err := processor.Handle("upstream", sig, true); err != nil {
			logger.Error("wsclient", fmt.Sprintf("[%s] handle signal error: %v", c.ID, err))
		}
	}
}
