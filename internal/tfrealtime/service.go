package tfrealtime

import (
	"context"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/kykira/ws-order-go/internal/config"
	"github.com/kykira/ws-order-go/internal/logs"
)

const (
	realtimeBase = "wss://apis.turboflow.xyz/realtime"
	accessKeyHex = "381a0f33aa70a496ed5e3f1cf8d0bcde59445fd1ea26259bb67a97f35a1ab283"
	secretHex    = "7f5816755df8336051e1a45555713b22340402ac95bb2ea4621bdf3df488a1f9"
)

// Heartbeat / reconnect cadence, mirroring internal/wsclient and
// internal/wsserver.
const (
	pongWait              = 90 * time.Second
	pingPeriod            = 60 * time.Second
	writeWait             = 10 * time.Second
	defaultReconnectDelay = 10 * time.Second
)

var pairSymbols = map[string]string{
	"5": "ETHUSDT",
	"6": "BTCUSDT",
}

var symbolPairs = map[string]string{
	"ETHUSDT": "5",
	"BTCUSDT": "6",
}

// Option is one event-contract duration (TF) option for a pair.
type Option struct {
	Duration     int    `json:"duration"`
	UpRate       string `json:"upRate"`
	DownRate     string `json:"downRate"`
	MinAmount    string `json:"minAmount"`
	MaxAmount    string `json:"maxAmount"`
	QuickEnabled bool   `json:"quickEnabled"`
	Status       string `json:"status"`
}

// Pair is the latest event-contract config for one symbol.
type Pair struct {
	PairID    string    `json:"pairId"`
	Symbol    string    `json:"symbol"`
	Enabled   bool      `json:"enabled"`
	Options   []Option  `json:"options"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// Snapshot is what the page renders.
type Snapshot struct {
	Connected bool            `json:"connected"`
	Revision  uint64          `json:"revision"`
	UpdatedAt time.Time       `json:"updatedAt"`
	LastError string          `json:"lastError,omitempty"`
	Pairs     map[string]Pair `json:"pairs"`
}

type Service struct {
	cfg    *config.Manager
	logger *logs.Logger

	mu       sync.RWMutex
	snapshot Snapshot
	updateCh chan struct{}
	conn     *websocket.Conn // live connection, closed by Stop
	runCtx   context.Context // non-nil while the reconnect loop is running
	cancel   context.CancelFunc

	// authIdx rotates the TurboFlow account used by each reconnect attempt.
	authIdx atomic.Uint64

	// reconnectDelay is the pause between failed attempts (tests shorten it).
	reconnectDelay time.Duration

	// wsBase overrides the realtime endpoint in tests; empty means realtimeBase.
	wsBase string
}

func NewService(cfg *config.Manager, logger *logs.Logger) *Service {
	return &Service{
		cfg:    cfg,
		logger: logger,
		snapshot: Snapshot{
			Pairs: map[string]Pair{},
		},
		updateCh:       make(chan struct{}),
		reconnectDelay: defaultReconnectDelay,
	}
}

// Start keeps a TurboFlow realtime WebSocket alive and caches evt_cfg_* data.
// It is idempotent: a second call while the service is already running is a
// no-op. Shut the service down with Stop.
func (s *Service) Start() {
	s.mu.Lock()
	if s.runCtx != nil {
		s.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.runCtx = ctx
	s.cancel = cancel
	delay := s.reconnectDelay
	if delay <= 0 {
		delay = defaultReconnectDelay
	}
	s.mu.Unlock()

	go func() {
		// Only the run that still owns runCtx may clear it, so a
		// Stop-then-Start restart is not clobbered by the old goroutine.
		defer func() {
			s.mu.Lock()
			if s.runCtx == ctx {
				s.runCtx = nil
				s.cancel = nil
			}
			s.mu.Unlock()
		}()

		for {
			s.runOnce(ctx)
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	}()
}

// Stop cancels the reconnect loop and closes the live connection. It is safe
// to call when the service was never started or was already stopped.
func (s *Service) Stop() {
	s.mu.Lock()
	cancel, conn := s.cancel, s.conn
	s.runCtx = nil
	s.cancel = nil
	s.conn = nil
	if s.snapshot.Connected {
		s.snapshot.Connected = false
		s.snapshot.UpdatedAt = time.Now()
	}
	s.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if conn != nil {
		_ = conn.Close()
	}
}

func (s *Service) GetSnapshot() Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := Snapshot{
		Connected: s.snapshot.Connected,
		Revision:  s.snapshot.Revision,
		UpdatedAt: s.snapshot.UpdatedAt,
		LastError: s.snapshot.LastError,
		Pairs:     make(map[string]Pair, len(s.snapshot.Pairs)),
	}
	for k, v := range s.snapshot.Pairs {
		out.Pairs[k] = clonePair(v)
	}
	return out
}

// clonePair copies the Pair and its Options backing array so callers cannot
// mutate the live cache through the returned slice.
func clonePair(p Pair) Pair {
	if p.Options != nil {
		options := make([]Option, len(p.Options))
		copy(options, p.Options)
		p.Options = options
	}
	return p
}

// GetPair returns one symbol's latest config without copying the whole map.
func (s *Service) GetPair(symbol string) (Pair, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.snapshot.Pairs[strings.ToUpper(strings.TrimSpace(symbol))]
	if !ok {
		return Pair{}, false
	}
	return clonePair(p), true
}

// Updates returns a channel that is closed whenever evt_cfg data changes.
func (s *Service) Updates() <-chan struct{} {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.updateCh
}

// notifyLocked bumps revision and wakes all odds waiters. Caller must hold mu.
func (s *Service) notifyLocked() {
	s.snapshot.Revision++
	old := s.updateCh
	s.updateCh = make(chan struct{})
	close(old)
}

func (s *Service) runOnce(ctx context.Context) {
	token, account := s.nextEnabledAuth()
	if token == "" {
		s.setError("no enabled TurboFlow account authorization")
		return
	}
	s.logf(logs.LevelInfo, fmt.Sprintf("using TurboFlow account %s", account))

	wsURL, err := s.signedURL(token)
	if err != nil {
		s.setError(fmt.Sprintf("sign realtime url: %v", err))
		return
	}

	conn, _, err := websocket.DefaultDialer.Dial(wsURL, http.Header{
		"User-Agent": {"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/153.0.0.0 Safari/537.36"},
	})
	if err != nil {
		if ctx.Err() != nil { // stopped while dialing
			return
		}
		s.setError(fmt.Sprintf("dial realtime: %v", err))
		return
	}
	defer conn.Close()

	s.mu.Lock()
	if ctx.Err() != nil { // stopped while dialing
		s.mu.Unlock()
		return
	}
	s.conn = conn
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		if s.conn == conn {
			s.conn = nil
		}
		s.mu.Unlock()
	}()

	s.setConnected(true)

	subscribe := `{"action":"subscribe","args":["evt_cfg_5","evt_cfg_6"]}`
	if err := conn.WriteMessage(websocket.TextMessage, []byte(subscribe)); err != nil {
		if ctx.Err() != nil {
			return
		}
		s.setError(fmt.Sprintf("subscribe evt_cfg: %v", err))
		return
	}

	// Heartbeat: without a read deadline + ping/pong a half-open TCP
	// connection is never detected, so Connected stays true and stale odds
	// keep being served as current.
	conn.SetReadDeadline(time.Now().Add(pongWait))
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(pongWait))
		return nil
	})

	pingDone := make(chan struct{})
	defer close(pingDone)
	go func() {
		ticker := time.NewTicker(pingPeriod)
		defer ticker.Stop()
		for {
			select {
			case <-pingDone:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				// WriteControl is safe to call concurrently with other writers.
				if err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(writeWait)); err != nil {
					return
				}
			}
		}
	}()

	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			if ctx.Err() != nil {
				s.logf(logs.LevelInfo, "realtime connection closed by Stop")
				return
			}
			s.setError(fmt.Sprintf("read realtime: %v", err))
			return
		}
		conn.SetReadDeadline(time.Now().Add(pongWait))
		s.handleMessage(msg)
	}
}

// tfAccount is one enabled TurboFlow account usable for the realtime feed.
type tfAccount struct {
	label string // account name/id, for logging only
	token string
}

// enabledAuths lists enabled TurboFlow accounts carrying a non-empty
// authorization token, in configuration order.
func (s *Service) enabledAuths() []tfAccount {
	if s.cfg == nil {
		return nil
	}
	var accounts []tfAccount
	for _, task := range s.cfg.Get().Tasks {
		if task.Type != "turboflow" || !task.Enabled {
			continue
		}
		token := strings.TrimSpace(task.Auth["authorization"])
		if token == "" {
			continue
		}
		label := strings.TrimSpace(task.Name)
		if label == "" {
			label = strings.TrimSpace(task.ID)
		}
		if label == "" {
			label = fmt.Sprintf("account-%d", len(accounts)+1)
		}
		accounts = append(accounts, tfAccount{label: label, token: token})
	}
	return accounts
}

// firstEnabledAuth returns the first enabled TurboFlow authorization token.
func (s *Service) firstEnabledAuth() string {
	accounts := s.enabledAuths()
	if len(accounts) == 0 {
		return ""
	}
	return accounts[0].token
}

// nextEnabledAuth returns the next enabled TurboFlow account, rotating on
// every call so that an expired/invalid token cannot pin the reconnect loop to
// the same account forever. The label is for logging; the token is never logged.
func (s *Service) nextEnabledAuth() (token, label string) {
	accounts := s.enabledAuths()
	if len(accounts) == 0 {
		return "", ""
	}
	i := (s.authIdx.Add(1) - 1) % uint64(len(accounts))
	a := accounts[i]
	return a.token, a.label
}

func (s *Service) signedURL(token string) (string, error) {
	ts := time.Now().Unix()
	msg := fmt.Sprintf("method=GET&path=/realtime&timestamp=%d&access-key=%s", ts, accessKeyHex)

	key, err := hex.DecodeString(accessKeyHex)
	if err != nil {
		return "", err
	}
	seed, err := hex.DecodeString(secretHex)
	if err != nil {
		return "", err
	}

	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(msg))
	digest := mac.Sum(nil)
	sig := ed25519.Sign(ed25519.NewKeyFromSeed(seed), digest)

	q := url.Values{}
	q.Set("isDex", "true")
	q.Set("PLATFORM", "web")
	q.Set("Authorization", token)
	q.Set("API-KEY", accessKeyHex)
	q.Set("TIMESTAMP", strconv.FormatInt(ts, 10))
	q.Set("SIGN", hex.EncodeToString(sig))

	base := realtimeBase
	s.mu.RLock()
	if s.wsBase != "" {
		base = s.wsBase
	}
	s.mu.RUnlock()
	return base + "?" + q.Encode(), nil
}

type realtimeMessage struct {
	Action string          `json:"action"`
	Status bool            `json:"status"`
	Data   json.RawMessage `json:"data"`
	Group  string          `json:"group"`
}

// cfgOption is one entry of the TurboFlow "ocs" options list.
type cfgOption struct {
	DU     int    `json:"du"`
	ARR    string `json:"arr"`
	BRR    string `json:"brr"`
	MinAmt string `json:"minAmt"`
	MaxAmt string `json:"maxAmt"`
	QE     bool   `json:"qe"`
	Status string `json:"s"`
}

// cfgPayload is the subset of an evt_cfg_* "pc" payload this service consumes.
type cfgPayload struct {
	PC struct {
		PID     string      `json:"pid"`
		PN      string      `json:"pn"`
		Enabled bool        `json:"e"`
		OCS     []cfgOption `json:"ocs"`
	} `json:"pc"`
}

// usable reports whether a parsed evt_cfg frame actually carries odds data for
// the pair: a non-empty symbol (pn) and/or a non-empty options list (ocs).
//
// The feed also emits status-only / schema-variant frames whose payload is
// empty, e.g. {"group":"evt_cfg_6","data":{"pc":{}}}. Those must not replace a
// previously good snapshot, so they are ignored.
func (p cfgPayload) usable() bool {
	return strings.TrimSpace(p.PC.PN) != "" || len(p.PC.OCS) > 0
}

func (s *Service) handleMessage(raw []byte) {
	var msg realtimeMessage
	if err := json.Unmarshal(raw, &msg); err != nil {
		s.logf(logs.LevelDebug, fmt.Sprintf("ignore unparsable realtime frame: %v", err))
		return
	}

	if msg.Action == "subscribe" {
		if !msg.Status {
			var group string
			if err := json.Unmarshal(msg.Data, &group); err == nil {
				s.setError(fmt.Sprintf("subscribe rejected: %s", group))
			}
		}
		return
	}

	if !strings.HasPrefix(msg.Group, "evt_cfg_") {
		return
	}
	pid := strings.TrimPrefix(msg.Group, "evt_cfg_")
	symbol := pairSymbols[pid]
	if symbol == "" {
		return
	}

	var payload cfgPayload
	if err := json.Unmarshal(msg.Data, &payload); err != nil {
		s.logf(logs.LevelDebug, fmt.Sprintf("ignore evt_cfg_%s frame with unparsable data: %v", pid, err))
		return
	}
	if !payload.usable() {
		s.logf(logs.LevelDebug, fmt.Sprintf("ignore evt_cfg_%s frame without usable odds payload", pid))
		return
	}

	options := make([]Option, 0, len(payload.PC.OCS))
	for _, o := range payload.PC.OCS {
		// TurboFlow 网页里 bid_return_rate(brr) 对应"更高/买涨"，
		// ask_return_rate(arr) 对应"更低/买跌"。
		options = append(options, Option{
			Duration:     o.DU,
			UpRate:       o.BRR,
			DownRate:     o.ARR,
			MinAmount:    o.MinAmt,
			MaxAmount:    o.MaxAmt,
			QuickEnabled: o.QE,
			Status:       o.Status,
		})
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.snapshot.Pairs[symbol] = Pair{
		PairID:    pid,
		Symbol:    symbol,
		Enabled:   payload.PC.Enabled,
		Options:   options,
		UpdatedAt: time.Now(),
	}
	s.snapshot.UpdatedAt = time.Now()
	s.snapshot.Connected = true
	s.snapshot.LastError = ""
	s.notifyLocked()
}

func (s *Service) setConnected(connected bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snapshot.Connected = connected
	s.snapshot.UpdatedAt = time.Now()
	if connected {
		s.snapshot.LastError = ""
	}
}

// logf writes a service log entry. A nil logger (used by some tests) is a no-op.
func (s *Service) logf(level logs.Level, msg string) {
	if s.logger == nil {
		return
	}
	switch level {
	case logs.LevelDebug:
		s.logger.Debug("tfrealtime", msg)
	case logs.LevelError:
		s.logger.Error("tfrealtime", msg)
	default:
		s.logger.Info("tfrealtime", msg)
	}
}

func (s *Service) setError(msg string) {
	s.logf(logs.LevelError, msg)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snapshot.Connected = false
	s.snapshot.LastError = msg
	s.snapshot.UpdatedAt = time.Now()
}
