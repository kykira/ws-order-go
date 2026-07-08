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
			{ID: "a1", Name: "Account-A", Enabled: true, APIUrl: "http://a", Method: "GET"},
			{ID: "a2", Name: "Account-B", Enabled: true, APIUrl: "http://b", Method: "GET"},
			{ID: "a3", Name: "Account-C", Enabled: true, APIUrl: "http://c", Method: "GET"},
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
			{ID: "a1", Name: "Account-A", Enabled: true, APIUrl: "http://a", Method: "GET"},
			{ID: "a2", Name: "Account-B", Enabled: true, APIUrl: "http://b", Method: "GET"},
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
	// dispatch=all should produce 2 account log lines (one per task)
	if signalCount < 2 {
		t.Errorf("expected >=2 signal log lines for all dispatch, got %d", signalCount)
	}
}

func TestDispatchDefaultToRandom(t *testing.T) {
	dir := t.TempDir()
	cfgPath := dir + "/config.json"

	// LoadManager's DefaultConfig sets Dispatch="random"
	mgr, err := config.LoadManager(cfgPath)
	if err != nil {
		t.Fatal(err)
	}

	got := mgr.Get().Dispatch
	if got != "random" {
		t.Errorf("dispatch should default to 'random', got %q", got)
	} else {
		t.Logf("default dispatch = %q ✓", got)
	}
}
