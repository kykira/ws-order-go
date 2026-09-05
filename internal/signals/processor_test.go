package signals

import (
	"testing"

	"github.com/kykira/ws-order-go/internal/config"
	"github.com/kykira/ws-order-go/internal/logs"
	"github.com/kykira/ws-order-go/internal/order"
)

func TestDispatchRandomPicksOne(t *testing.T) {
	dir := t.TempDir()
	cfgPath := dir + "/config.json"

	cfg := config.Config{
		Server:   config.ServerConfig{Port: 0},
		Dispatch: "random",
		Tasks: []config.TaskConfig{
			{ID: "a1", Name: "Account-A", Enabled: true},
			{ID: "a2", Name: "Account-B", Enabled: true},
			{ID: "a3", Name: "Account-C", Enabled: true},
		},
	}
	mgr, err := config.LoadManager(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	_ = mgr.Update(func(c *config.Config) { *c = cfg })

	logger := logs.NewLogger(100)
	orderClient := order.NewClient(logger)
	proc := NewProcessor(mgr, logger, orderClient)

	sig := Signal{Action: "buy", Symbol: "BTCUSDT", OrderID: 1}
	_ = proc.Handle("test", sig, false)

	found := false
	for _, e := range logger.Entries() {
		if e.Level == "INFO" && e.Source == "signal" {
			t.Logf("[%s] %s", e.Level, e.Message)
			found = true
		}
	}
	if !found {
		t.Error("no signal log entries — dispatch failed")
	}
}

func TestDispatchAllExecutesAll(t *testing.T) {
	dir := t.TempDir()
	cfgPath := dir + "/config.json"

	cfg := config.Config{
		Server:   config.ServerConfig{Port: 0},
		Dispatch: "all",
		Tasks: []config.TaskConfig{
			{ID: "a1", Name: "Account-A", Enabled: true},
			{ID: "a2", Name: "Account-B", Enabled: true},
		},
	}
	mgr, err := config.LoadManager(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	_ = mgr.Update(func(c *config.Config) { *c = cfg })

	logger := logs.NewLogger(100)
	orderClient := order.NewClient(logger)
	proc := NewProcessor(mgr, logger, orderClient)

	sig := Signal{Action: "sell", Symbol: "ETHUSDT", OrderID: 2}
	_ = proc.Handle("test", sig, false)

	signalCount := 0
	for _, e := range logger.Entries() {
		if e.Level == "INFO" && e.Source == "signal" {
			t.Logf("[%s] %s", e.Level, e.Message)
			signalCount++
		}
	}
	if signalCount < 2 {
		t.Errorf("expected >=2 signal log lines for all dispatch, got %d", signalCount)
	}
}

func TestDispatchDefaultToRoundRobin(t *testing.T) {
	dir := t.TempDir()
	cfgPath := dir + "/config.json"

	mgr, err := config.LoadManager(cfgPath)
	if err != nil {
		t.Fatal(err)
	}

	got := mgr.Get().Dispatch
	if got != "round-robin" {
		t.Errorf("dispatch should default to 'round-robin', got %q", got)
	} else {
		t.Logf("default dispatch = %q ✓", got)
	}
}

func TestDispatchRoundRobinOrder(t *testing.T) {
	dir := t.TempDir()
	cfgPath := dir + "/config.json"

	cfg := config.Config{
		Server:   config.ServerConfig{Port: 0},
		Dispatch: "round-robin",
		Tasks: []config.TaskConfig{
			{ID: "a1", Name: "Account-A", Enabled: true},
			{ID: "a2", Name: "Account-B", Enabled: true},
		},
	}
	mgr, err := config.LoadManager(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	_ = mgr.Update(func(c *config.Config) { *c = cfg })

	logger := logs.NewLogger(100)
	orderClient := order.NewClient(logger)
	proc := NewProcessor(mgr, logger, orderClient)

	sig := Signal{Action: "buy", Symbol: "BTCUSDT", OrderID: 1}
	_ = proc.Handle("test", sig, false)

	foundA := false
	for _, e := range logger.Entries() {
		t.Logf("[%s] %s", e.Level, e.Message)
		if e.Source == "signal" && contains(e.Message, "account=[Account-A]") {
			foundA = true
		}
	}
	if !foundA {
		t.Error("first signal should go to Account-A")
	}
}

func TestStrategyGroupAllDispatch(t *testing.T) {
	dir := t.TempDir()
	cfgPath := dir + "/config.json"

	cfg := config.Config{
		Server: config.ServerConfig{Port: 0},
		Tasks: []config.TaskConfig{
			{ID: "a1", Name: "A1", Enabled: true},
			{ID: "a2", Name: "A2", Enabled: true},
		},
		Strategies: []config.StrategyConfig{
			{
				ID: "st-1", Name: "eth-30m", Enabled: true,
				Groups: []config.StrategyGroupConfig{
					{ID: "g1", Name: "G1", Enabled: true, Dispatch: "all", AccountIDs: []string{"a1", "a2"}},
				},
			},
		},
	}
	mgr, err := config.LoadManager(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	_ = mgr.Update(func(c *config.Config) { *c = cfg })

	logger := logs.NewLogger(100)
	orderClient := order.NewClient(logger)
	proc := NewProcessor(mgr, logger, orderClient)

	sig := Signal{Action: "buy", Symbol: "ETHUSDT", Strategy: "st-1", Period: "30m"}
	_ = proc.Handle("test", sig, false)

	count := 0
	for _, e := range logger.Entries() {
		if e.Source == "signal" && contains(e.Message, "source=test") {
			count++
		}
	}
	if count != 2 {
		t.Errorf("expected 2 executed accounts in all group, got %d", count)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && searchStr(s, substr)
}

func searchStr(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
