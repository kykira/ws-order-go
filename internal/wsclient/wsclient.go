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

	ctx      context.Context
	cancel   context.CancelFunc
	running  bool
	loopDone chan struct{} // closed when the current loop goroutine returns
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
			}

			// 根据 enabled 状态启停；Start 内部会等待上一个 loop 退出，
			// 防止 dial 过程中重复 Start 造成双连接；禁用的上游不做无谓拨号。
			if u.Enabled && !existing.IsRunning() {
				existing.Start(m.logger, m.processor)
			} else if !u.Enabled && existing.IsRunning() {
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

func (c *Conn) IsRunning() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.running
}

// snapshot reads the mutable upstream config under the lock; Manager.Sync may
// rewrite these fields while the loop is running.
func (c *Conn) snapshot() (name, wsURL, wsKey string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.Name, c.URL, c.Key
}

const (
	// backoffMin is the reconnect delay after a healthy connection.
	backoffMin = 2 * time.Second
	// backoffMax caps the reconnect delay after repeated short-lived connections.
	backoffMax = 30 * time.Second
	// healthyConnDuration is how long a connection must survive for the backoff
	// to be considered healthy and decayed back to backoffMin.
	healthyConnDuration = 30 * time.Second
	// loopStopWait bounds how long Start waits for a previous loop goroutine to
	// finish before giving up, so one upstream can never have two live loops.
	loopStopWait = 2 * time.Second
)

// sleepCtx waits for d unless ctx is cancelled first. It returns false when the
// context was cancelled, so callers can stop promptly instead of blocking Stop.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (c *Conn) Start(logger *logs.Logger, processor *signals.Processor) {
	// A previous loop may still be winding down after Stop (or after a config
	// change). Wait for it, bounded, before starting a replacement: two live
	// loops for one upstream would handle every signal twice.
	c.mu.Lock()
	prev := c.loopDone
	c.mu.Unlock()
	if prev != nil {
		select {
		case <-prev:
		case <-time.After(loopStopWait):
			logger.Error("wsclient", fmt.Sprintf("[%s] previous loop still stopping, start skipped", c.ID))
			return
		}
	}

	c.mu.Lock()
	if c.running {
		c.mu.Unlock()
		return
	}
	if c.cancel != nil {
		c.cancel()
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	c.ctx, c.cancel = ctx, cancel
	c.running = true
	c.loopDone = done
	c.mu.Unlock()
	go c.loop(ctx, done, logger, processor)
}

func (c *Conn) Stop() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cancel != nil {
		c.cancel()
	}
	if c.conn != nil {
		_ = c.conn.Close()
		c.conn = nil
	}
	c.connected = false
	c.running = false
}

func (c *Conn) loop(ctx context.Context, done chan struct{}, logger *logs.Logger, processor *signals.Processor) {
	defer func() {
		c.mu.Lock()
		if c.loopDone == done {
			c.loopDone = nil
			c.running = false
		}
		c.mu.Unlock()
		close(done)
	}()

	backoff := backoffMin
	for {
		name, wsURL, wsKey := c.snapshot()

		if ctx.Err() != nil {
			logger.Info("wsclient", fmt.Sprintf("upstream [%s] %s stopped", c.ID, name))
			return
		}

		if wsURL == "" {
			// No URL configured yet: stay idle, but keep Stop responsive.
			sleepCtx(ctx, backoffMin)
			continue
		}

		fullURL, err := buildWSURL(wsURL, wsKey)
		if err != nil {
			logger.Error("wsclient", fmt.Sprintf("[%s] invalid ws url: %v", c.ID, err))
			sleepCtx(ctx, backoff)
			continue
		}

		logger.Info("wsclient", fmt.Sprintf("[%s] %s connecting to %s", c.ID, name, fullURL))

		// Dial through the loop context (copy the dialer instead of mutating the
		// package global) so Stop cancels an in-flight dial, and cap the
		// handshake instead of relying on gorilla's 45s default.
		dialer := *websocket.DefaultDialer
		dialer.HandshakeTimeout = 30 * time.Second
		conn, _, err := dialer.DialContext(ctx, fullURL, nil)
		if err != nil {
			logger.Error("wsclient", fmt.Sprintf("[%s] dial error: %v", c.ID, err))
			sleepCtx(ctx, backoff)
			if backoff < backoffMax {
				backoff *= 2
				if backoff > backoffMax {
					backoff = backoffMax
				}
			}
			continue
		}

		connectedAt := time.Now()

		// Stop() cannot see a connection that is still being dialled, so
		// re-check the context and the running flag before publishing it:
		// otherwise a cancelled loop would keep a live, chatty upstream feeding
		// signals (and real orders) after the operator disabled it.
		c.mu.Lock()
		if ctx.Err() != nil || !c.running || c.ctx != ctx {
			c.mu.Unlock()
			_ = conn.Close()
			logger.Info("wsclient", fmt.Sprintf("[%s] stopped during dial, connection dropped", c.ID))
			return
		}
		c.conn = conn
		c.connected = true
		c.mu.Unlock()

		logger.Info("wsclient", fmt.Sprintf("[%s] %s connected", c.ID, name))

		err = func() error {
			connCtx, connCancel := context.WithCancel(ctx)
			defer connCancel()
			return c.readLoop(connCtx, conn, logger, processor)
		}()
		if err != nil {
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

		// Always wait out the backoff before re-dialling. An upstream that
		// completes the handshake and then closes immediately (auth rejection,
		// restart, LB drain) must not turn into a tight dial loop; only a
		// connection that stayed healthy decays the backoff again.
		if time.Since(connectedAt) >= healthyConnDuration {
			backoff = backoffMin
		} else if backoff < backoffMax {
			backoff *= 2
			if backoff > backoffMax {
				backoff = backoffMax
			}
		}
		logger.Info("wsclient", fmt.Sprintf("[%s] disconnected, reconnecting in %s", c.ID, backoff))
		if !sleepCtx(ctx, backoff) {
			continue
		}
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

func (c *Conn) readLoop(ctx context.Context, conn *websocket.Conn, logger *logs.Logger, processor *signals.Processor) error {
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

	// 主动 Ping；ctx 是本次连接独有的，readLoop 返回后立刻退出，避免每次重连泄漏一个 goroutine
	go func() {
		ticker := time.NewTicker(pingPeriod)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
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
