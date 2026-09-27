package tfrealtime

import (
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
}

func NewService(cfg *config.Manager, logger *logs.Logger) *Service {
	return &Service{
		cfg:    cfg,
		logger: logger,
		snapshot: Snapshot{
			Pairs: map[string]Pair{},
		},
		updateCh: make(chan struct{}),
	}
}

// Start keeps a TurboFlow realtime WebSocket alive and caches evt_cfg_* data.
func (s *Service) Start() {
	go func() {
		for {
			s.runOnce()
			time.Sleep(10 * time.Second)
		}
	}()
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
		out.Pairs[k] = v
	}
	return out
}

// GetPair returns one symbol's latest config without copying the whole map.
func (s *Service) GetPair(symbol string) (Pair, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.snapshot.Pairs[strings.ToUpper(strings.TrimSpace(symbol))]
	if !ok {
		return Pair{}, false
	}
	return p, true
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

func (s *Service) runOnce() {
	token := s.firstEnabledAuth()
	if token == "" {
		s.setError("no enabled TurboFlow account authorization")
		return
	}

	wsURL, err := s.signedURL(token)
	if err != nil {
		s.setError(fmt.Sprintf("sign realtime url: %v", err))
		return
	}

	conn, _, err := websocket.DefaultDialer.Dial(wsURL, http.Header{
		"User-Agent": {"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/153.0.0.0 Safari/537.36"},
	})
	if err != nil {
		s.setError(fmt.Sprintf("dial realtime: %v", err))
		return
	}
	defer conn.Close()

	s.setConnected(true)

	subscribe := `{"action":"subscribe","args":["evt_cfg_5","evt_cfg_6"]}`
	if err := conn.WriteMessage(websocket.TextMessage, []byte(subscribe)); err != nil {
		s.setError(fmt.Sprintf("subscribe evt_cfg: %v", err))
		return
	}

	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			s.setError(fmt.Sprintf("read realtime: %v", err))
			return
		}
		s.handleMessage(msg)
	}
}

func (s *Service) firstEnabledAuth() string {
	cfg := s.cfg.Get()
	for _, task := range cfg.Tasks {
		if task.Type != "turboflow" || !task.Enabled {
			continue
		}
		if token := strings.TrimSpace(task.Auth["authorization"]); token != "" {
			return token
		}
	}
	return ""
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
	return realtimeBase + "?" + q.Encode(), nil
}

type realtimeMessage struct {
	Action string          `json:"action"`
	Status bool            `json:"status"`
	Data   json.RawMessage `json:"data"`
	Group  string          `json:"group"`
}

func (s *Service) handleMessage(raw []byte) {
	var msg realtimeMessage
	if err := json.Unmarshal(raw, &msg); err != nil {
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

	var payload struct {
		PC struct {
			PID     string `json:"pid"`
			PN      string `json:"pn"`
			Enabled bool   `json:"e"`
			OCS     []struct {
				DU     int    `json:"du"`
				ARR    string `json:"arr"`
				BRR    string `json:"brr"`
				MinAmt string `json:"minAmt"`
				MaxAmt string `json:"maxAmt"`
				QE     bool   `json:"qe"`
				Status string `json:"s"`
			} `json:"ocs"`
		} `json:"pc"`
	}
	if err := json.Unmarshal(msg.Data, &payload); err != nil {
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

func (s *Service) setError(msg string) {
	s.logger.Error("tfrealtime", msg)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snapshot.Connected = false
	s.snapshot.LastError = msg
	s.snapshot.UpdatedAt = time.Now()
}
