package config

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
)

type ServerConfig struct {
	Port     int    `json:"port"`
	Password string `json:"password,omitempty"`
}

type UpstreamConfig struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	WSUrl   string `json:"wsUrl"`
	WSKey   string `json:"wsKey"`
	Enabled bool   `json:"enabled"`
}

type WSServerConfig struct {
	Enabled   bool   `json:"enabled"`
	Path      string `json:"path"`
	Key       string `json:"key"`
	ApplySkip bool   `json:"applySkip"`
}

type TaskConfig struct {
	ID          string                       `json:"id"`
	Name        string                       `json:"name"`
	Type        string                       `json:"type,omitempty"`    // binance | hibt | turboflow | raw
	Auth        map[string]string            `json:"auth,omitempty"`    // 平台 token 字段，例如 csrftoken/cookie/token/account_id
	Symbols     map[string]map[string]string `json:"symbols,omitempty"` // 平台 symbol 映射，如 turboflow: BTCUSDT -> {pair_id, coin_code}
	Enabled     bool                         `json:"enabled"`
	SkipSignals int                          `json:"skipSignals"`
	TimeRanges  []TimeRange                  `json:"timeRanges,omitempty"`
	ExpiresAt   int64                        `json:"expiresAt"`         // Unix timestamp (seconds) for cookie/token expiration
	APIUrl      string                       `json:"apiUrl,omitempty"`  // raw 自定义请求 URL
	Method      string                       `json:"method,omitempty"`  // raw 自定义 Method
	Headers     string                       `json:"headers,omitempty"` // raw 自定义 Headers
	Body        string                       `json:"body,omitempty"`    // raw 自定义 Body
	ValueBuy    string                       `json:"valueBuy"`
	ValueSell   string                       `json:"valueSell"`
}

// normalizeAuth keeps only the platform-relevant token fields for each
// account type. Binance keeps csrftoken + p20t; other platforms keep all
// fields for now.
func normalizeAuth(accountType string, auth map[string]string) map[string]string {
	if auth == nil {
		auth = map[string]string{}
	}

	switch accountType {
	case "binance":
		out := map[string]string{}
		if v := strings.TrimSpace(auth["csrftoken"]); v != "" {
			out["csrftoken"] = v
		}
		p20t := strings.TrimSpace(auth["p20t"])
		if p20t == "" {
			if cookie := auth["cookie"]; cookie != "" {
				for _, part := range strings.Split(cookie, ";") {
					part = strings.TrimSpace(part)
					if strings.HasPrefix(part, "p20t=") {
						p20t = strings.TrimPrefix(part, "p20t=")
						break
					}
				}
			}
		}
		if p20t != "" {
			out["p20t"] = p20t
		}
		return out

	case "hibt", "turboflow":
		return auth

	default:
		return auth
	}
}

// StrategyGroupAccountConfig binds one account to a strategy group with the
// amount used for that account in this strategy.
type StrategyGroupAccountConfig struct {
	AccountID string `json:"accountId"`
	Amount    string `json:"amount,omitempty"`
}

// StrategyGroupConfig is a group of account bindings inside one strategy.
// Dispatch: random | round-robin | all.
type StrategyGroupConfig struct {
	ID         string                       `json:"id"`
	Name       string                       `json:"name"`
	Enabled    bool                         `json:"enabled"`
	Dispatch   string                       `json:"dispatch"` // random | round-robin | all
	Accounts   []StrategyGroupAccountConfig `json:"accounts"`
	AccountIDs []string                     `json:"accountIds,omitempty"` // deprecated, kept for old configs
}

// StrategyConfig groups several account groups. A signal with matching
// strategy name/id dispatches to every enabled group of the strategy.
type StrategyConfig struct {
	ID      string                `json:"id"`
	Name    string                `json:"name"`
	Enabled bool                  `json:"enabled"`
	Groups  []StrategyGroupConfig `json:"groups"`
}

type Config struct {
	Server     ServerConfig     `json:"server"`
	Upstreams  []UpstreamConfig `json:"upstreams"`
	WSServer   WSServerConfig   `json:"wsServer"`
	Dispatch   string           `json:"dispatch"`
	Tasks      []TaskConfig     `json:"tasks"`
	Strategies []StrategyConfig `json:"strategies,omitempty"`
}

type Manager struct {
	mu   sync.RWMutex
	cfg  Config
	path string
}

func DefaultConfig() Config {
	return Config{
		Server:    ServerConfig{Port: 9946},
		Upstreams: []UpstreamConfig{},
		WSServer: WSServerConfig{
			Enabled:   true,
			Path:      "/ws",
			Key:       "",
			ApplySkip: false,
		},
		Dispatch: "round-robin",
		Tasks: []TaskConfig{
			{
				ID:          "default",
				Name:        "Default Task",
				Enabled:     true,
				SkipSignals: 0,
			},
		},
	}
}

func LoadManager(path string) (*Manager, error) {
	cfg := DefaultConfig()
	bs, err := os.ReadFile(path)
	if err == nil {
		if err := json.Unmarshal(bs, &cfg); err != nil {
			return nil, fmt.Errorf("failed to parse config file: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}

	// Apply defaults for missing fields
	if cfg.Dispatch == "" {
		cfg.Dispatch = "round-robin"
	}

	if err := PrepareTasks(cfg.Tasks); err != nil {
		return nil, err
	}

	applyEnvOverrides(&cfg)

	m := &Manager{cfg: cfg, path: path}
	if err := m.Save(); err != nil {
		return nil, err
	}
	return m, nil
}

func (m *Manager) Get() Config {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.cfg
}

func (m *Manager) Update(updateFn func(*Config)) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	updateFn(&m.cfg)
	return m.saveLocked()
}

func (m *Manager) Save() error {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.saveLocked()
}

func (m *Manager) saveLocked() error {
	bs, err := json.MarshalIndent(m.cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(m.path, bs, 0644)
}

func applyEnvOverrides(cfg *Config) {
	if v := os.Getenv("WSORDER_PORT"); v != "" {
		if p, err := strconv.Atoi(v); err == nil && p > 0 && p < 65536 {
			cfg.Server.Port = p
		}
	}
}
