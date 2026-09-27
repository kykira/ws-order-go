package signals

import (
	"testing"

	"github.com/kykira/ws-order-go/internal/config"
	"github.com/kykira/ws-order-go/internal/logs"
	"github.com/kykira/ws-order-go/internal/order"
	"github.com/kykira/ws-order-go/internal/tfrealtime"
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

type fakeBalanceProvider map[string]float64

func (f fakeBalanceProvider) GetFuturesBalances() map[string]float64 { return f }

func TestBinanceBalanceWeights(t *testing.T) {
	proc := &Processor{balanceProvider: fakeBalanceProvider{
		"a1": 100,
		"a2": 400,
	}}
	tasks := []config.TaskConfig{
		{ID: "a1", Type: "binance"},
		{ID: "a2", Type: "binance"},
		{ID: "tf", Type: "turboflow"},
	}

	weights := proc.weightsForTasks(tasks)
	if weights[0] <= weights[1] {
		t.Fatalf("lower balance should have higher weight: a1=%v a2=%v", weights[0], weights[1])
	}
	if weights[0]/weights[1] > 3 {
		t.Fatalf("weight ratio exceeds 3:1: a1=%v a2=%v", weights[0], weights[1])
	}
	if weights[2] != 1 {
		t.Fatalf("non-binance weight = %v, want 1", weights[2])
	}

	// Extreme gap: 1U vs 10000U must still cap at 3:1.
	proc.balanceProvider = fakeBalanceProvider{"a1": 1, "a2": 10000}
	weights = proc.weightsForTasks(tasks[:2])
	if weights[0]/weights[1] > 3 || weights[0] <= weights[1] || weights[1] != 1 {
		t.Fatalf("expected capped weights near 3/1 for extreme balance gap, got %v/%v", weights[0], weights[1])
	}
}

type fakeOddsProvider struct {
	pairs map[string]tfrealtime.Pair
}

func (f fakeOddsProvider) GetPair(symbol string) (tfrealtime.Pair, bool) {
	p, ok := f.pairs[symbol]
	return p, ok
}

func (f fakeOddsProvider) Updates() <-chan struct{} { return nil }

func TestTurboFlowDirectionOdds(t *testing.T) {
	proc := &Processor{oddsProvider: fakeOddsProvider{pairs: map[string]tfrealtime.Pair{
		"BTCUSDT": {
			PairID: "6",
			Symbol: "BTCUSDT",
			Options: []tfrealtime.Option{
				{Duration: 1800, UpRate: "0.82", DownRate: "0.79"},
			},
		},
	}}}

	buy := order.PlaceOrderRequest{Action: "buy", Symbol: "BTCUSDT", Period: "30m"}
	if got := proc.currentDirectionOdds(buy); got != 0.82 {
		t.Fatalf("buy odds = %v, want 0.82", got)
	}
	sell := order.PlaceOrderRequest{Action: "sell", Symbol: "BTCUSDT", Period: "30m"}
	if got := proc.currentDirectionOdds(sell); got != 0.79 {
		t.Fatalf("sell odds = %v, want 0.79", got)
	}

	if got := proc.minOdds(config.TaskConfig{Type: "turboflow", MinOdds: "0.80"}); got != 0.80 {
		t.Fatalf("minOdds = %v, want 0.80", got)
	}
	if got := proc.minOdds(config.TaskConfig{Type: "binance", MinOdds: "0.80"}); got != 0 {
		t.Fatalf("binance minOdds = %v, want 0", got)
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
