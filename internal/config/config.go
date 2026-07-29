package config

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"sync"
)

type ServerConfig struct {
	Port int `json:"port"`
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
	ID             string      `json:"id"`
	Name           string      `json:"name"`
	Enabled        bool        `json:"enabled"`
	SkipSignals    int         `json:"skipSignals"`
	TimeRanges     []TimeRange `json:"timeRanges,omitempty"`
	AllowedSymbols string      `json:"allowedSymbols"` // e.g. "BTCUSDT,ETHUSDT" or empty for all
	ExpiresAt      int64       `json:"expiresAt"`      // Unix timestamp (seconds) for cookie/token expiration
	HTTPProxyURL   string      `json:"httpProxyUrl"`
	APIUrl         string      `json:"apiUrl"`
	Method         string      `json:"method"`
	Headers        string      `json:"headers"`
	Body           string      `json:"body"`
	ValueBuy       string      `json:"valueBuy"`
	ValueSell      string      `json:"valueSell"`
	MinProba       float64     `json:"minProba"` // 0=不校验，非0=proba低于此值的信号跳过该账号
}

type Config struct {
	Server    ServerConfig     `json:"server"`
	Upstreams []UpstreamConfig `json:"upstreams"`
	WSServer  WSServerConfig   `json:"wsServer"`
	Dispatch  string           `json:"dispatch"`
	Tasks     []TaskConfig     `json:"tasks"`
}

type Manager struct {
	mu   sync.RWMutex
	cfg  Config
	path string
}

func DefaultConfig() Config {
	return Config{
		Server: ServerConfig{Port: 9946},
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
				ID:           "default",
				Name:         "Default Task",
				Enabled:      true,
				SkipSignals:  0,
				HTTPProxyURL: "",
				APIUrl:       "https://www.binance.com/bapi/futures/v2/private/future/event-contract/place-order",
				Method:       "POST",
				Headers:      "Content-Type: application/json\nclienttype: web",
				Body:         "{\"orderAmount\":\"{{amount}}\",\"timeIncrements\":\"{{unit}}\",\"symbolName\":\"BTCUSDT\",\"payoutRatio\":\"0.80\",\"direction\":\"{{direction}}\"}",
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
