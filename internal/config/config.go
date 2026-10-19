package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
	ID             string                       `json:"id"`
	Name           string                       `json:"name"`
	Type           string                       `json:"type,omitempty"`               // binance | hibt | turboflow | raw
	Auth           map[string]string            `json:"auth,omitempty"`               // 平台 token 字段，例如 csrftoken/cookie/token/account_id
	Symbols        map[string]map[string]string `json:"symbols,omitempty"`            // 平台 symbol 映射，如 turboflow: BTCUSDT -> {pair_id, coin_code}
	MinOdds        string                       `json:"minOdds,omitempty"`            // TurboFlow 最低赔率阈值，达到后立即下单
	RandomDelaySec string                       `json:"randomDelaySeconds,omitempty"` // 币安账号专用：下单前随机延迟秒数，"5"=0~5s，"3-8"=3~8s，留空=不延迟
	Enabled        bool                         `json:"enabled"`
	AllowedSymbols string                       `json:"allowedSymbols,omitempty"` // 兼容旧信号：留空表示全部 symbol
	SkipSignals    int                          `json:"skipSignals"`
	TimeRanges     []TimeRange                  `json:"timeRanges,omitempty"`
	ExpiresAt      int64                        `json:"expiresAt"`         // Unix timestamp (seconds) for cookie/token expiration
	APIUrl         string                       `json:"apiUrl,omitempty"`  // raw 自定义请求 URL
	Method         string                       `json:"method,omitempty"`  // raw 自定义 Method
	Headers        string                       `json:"headers,omitempty"` // raw 自定义 Headers
	Body           string                       `json:"body,omitempty"`    // raw 自定义 Body
	ValueBuy       string                       `json:"valueBuy"`
	ValueSell      string                       `json:"valueSell"`
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
		return cloneAuth(auth)

	default:
		return cloneAuth(auth)
	}
}

// cloneAuth returns a copy so callers can never mutate the map they passed in
// through the normalized result.
func cloneAuth(auth map[string]string) map[string]string {
	out := make(map[string]string, len(auth))
	for k, v := range auth {
		out[k] = v
	}
	return out
}

// StrategyGroupAccountConfig binds one account to a strategy group with the
// amount used for that account in this strategy.
type StrategyGroupAccountConfig struct {
	AccountID string `json:"accountId"`
	Amount    string `json:"amount,omitempty"`
	// ★ 动态下单金额(2026-10-14): 配了 Percent → 金额 = 账户可用额 × Percent%（保留 1 位小数）
	// 未配 Percent → 沿用静态 Amount（向后兼容 ✓）
	Percent   float64 `json:"percent,omitempty"`
	MinAmount float64 `json:"minAmount,omitempty"` // 可选下限(U)
	MaxAmount float64 `json:"maxAmount,omitempty"` // 可选上限(U)

	// Priority 是分组内优先级：数值越大越优先。顺序轮询时只有最高优先级那档
	// 会被使用并在档内轮流，低优先级账号仅作为失败兜底；留空/0 表示普通。
	Priority int `json:"priority,omitempty"`
}

// StrategyGroupConfig is a group of account bindings inside one strategy.
// Dispatch: random | weighted | round-robin | all.
type StrategyGroupConfig struct {
	ID         string                       `json:"id"`
	Name       string                       `json:"name"`
	Enabled    bool                         `json:"enabled"`
	Dispatch   string                       `json:"dispatch"` // random | weighted | round-robin | all
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

	// Defaults, normalization and validation (including Dispatch) all live in
	// Prepare so the load and update paths cannot diverge.
	if err := Prepare(&cfg); err != nil {
		return nil, err
	}

	m := &Manager{cfg: cfg, path: path}
	// Persist the normalized config BEFORE applying environment overrides, so
	// an override (e.g. WSORDER_PORT) is never written into the user's file and
	// cannot silently outlive the environment it came from.
	if err := m.Save(); err != nil {
		// A read-only or host-owned mount must not stop the service from
		// starting: the in-memory config is valid, we simply cannot persist it.
		fmt.Fprintf(os.Stderr, "config: warning: cannot persist %s: %v\n", path, err)
	}
	applyEnvOverrides(&m.cfg)
	return m, nil
}

// Get returns a deep copy of the current configuration. The copy matters: the
// Config value contains slices and nested maps, so returning it directly would
// let a caller mutate live state outside the mutex.
func (m *Manager) Get() Config {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return clone(m.cfg)
}

// clone deep-copies a Config through JSON so nested slices/maps are not shared.
func clone(cfg Config) Config {
	bs, err := json.Marshal(cfg)
	if err != nil {
		return cfg
	}
	var out Config
	if err := json.Unmarshal(bs, &out); err != nil {
		return cfg
	}
	return out
}

// Prepare applies defaults, normalization and validation in one place so the
// load path, the update path and the HTTP API cannot diverge.
func Prepare(cfg *Config) error {
	cfg.Dispatch = NormalizeDispatch(cfg.Dispatch)
	if cfg.Server.Port < 0 || cfg.Server.Port > 65535 {
		return fmt.Errorf("server.port %d out of range 0-65535", cfg.Server.Port)
	}
	if err := PrepareTasks(cfg.Tasks); err != nil {
		return err
	}
	return PrepareStrategies(cfg.Strategies)
}

// Update applies updateFn, validates the result and only then persists it. On a
// validation or write failure the previous configuration is restored so an
// update can never leave memory and disk (or the running service) disagreeing.
func (m *Manager) Update(updateFn func(*Config)) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	prev := clone(m.cfg)
	updateFn(&m.cfg)
	if err := Prepare(&m.cfg); err != nil {
		m.cfg = prev
		return err
	}
	if err := m.saveLocked(); err != nil {
		m.cfg = prev
		return err
	}
	return nil
}

func (m *Manager) Save() error {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.saveLocked()
}

// saveLocked writes the config atomically (temp file + rename) so a crash or a
// full disk can never leave a truncated config.json behind. Caller holds the lock.
func (m *Manager) saveLocked() error {
	bs, err := json.MarshalIndent(m.cfg, "", "  ")
	if err != nil {
		return err
	}

	tmp, err := os.CreateTemp(filepath.Dir(m.path), ".config-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // no-op once the rename succeeded

	if _, err := tmp.Write(bs); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0644); err != nil {
		return err
	}
	return os.Rename(tmpName, m.path)
}

func applyEnvOverrides(cfg *Config) {
	if v := os.Getenv("WSORDER_PORT"); v != "" {
		if p, err := strconv.Atoi(v); err == nil && p > 0 && p < 65536 {
			cfg.Server.Port = p
		}
	}
}
