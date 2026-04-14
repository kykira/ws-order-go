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
	WSUrl   string `json:"wsUrl"`
	WSKey   string `json:"wsKey"`
	Enabled bool   `json:"enabled"`
}

type TaskConfig struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Enabled      bool   `json:"enabled"`
	SkipSignals  int    `json:"skipSignals"`
	HTTPProxyURL string `json:"httpProxyUrl"`
	APIUrl       string `json:"apiUrl"`
	Method       string `json:"method"`
	Headers      string `json:"headers"`
	Body         string `json:"body"`
	ValueBuy     string `json:"valueBuy"`
	ValueSell    string `json:"valueSell"`
}

type Config struct {
	Server   ServerConfig   `json:"server"`
	Upstream UpstreamConfig `json:"upstream"`
	Tasks    []TaskConfig   `json:"tasks"`
}

type Manager struct {
	mu   sync.RWMutex
	cfg  Config
	path string
}

func DefaultConfig() Config {
	return Config{
		Server: ServerConfig{Port: 9000},
		Upstream: UpstreamConfig{
			WSUrl:   "",
			WSKey:   "",
			Enabled: false,
		},
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

	// Ensure defaults
	for i := range cfg.Tasks {
		if cfg.Tasks[i].ID == "" {
			cfg.Tasks[i].ID = fmt.Sprintf("task-%d", i+1)
		}
		if cfg.Tasks[i].Name == "" {
			cfg.Tasks[i].Name = cfg.Tasks[i].ID
		}
		if cfg.Tasks[i].Method == "" {
			cfg.Tasks[i].Method = "POST"
		}
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
