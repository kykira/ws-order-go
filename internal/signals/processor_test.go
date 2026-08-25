package signals

import (
	"testing"
	"time"

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

func TestDispatchRoundRobin5Limit(t *testing.T) {
	dir := t.TempDir()
	cfgPath := dir + "/config.json"

	cfg := config.Config{
		Server:   config.ServerConfig{Port: 0},
		Dispatch: "round-robin",
		Tasks: []config.TaskConfig{
			{ID: "a1", Name: "Acc-1", Enabled: true, APIUrl: "http://a", Method: "GET"},
			{ID: "a2", Name: "Acc-2", Enabled: true, APIUrl: "http://b", Method: "GET"},
		},
	}
	mgr, err := config.LoadManager(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	_ = mgr.Update(func(c *config.Config) { *c = cfg })

	logger := logs.NewLogger(200)
	orderClient := order.NewClient(logger)
	proc := NewProcessor(mgr, logger, orderClient)

	sig := Signal{Action: "buy", Symbol: "BTCUSDT"}

	// Send 7 signals rapidly — Acc-1 gets 5 slots, Acc-2 gets 2
	for i := 0; i < 7; i++ {
		sig.OrderID = int64(i)
		_ = proc.Handle("test", sig, false)
	}

	count1 := 0
	count2 := 0
	for _, e := range logger.Entries() {
		if e.Source == "signal" && contains(e.Message, "dispatch=round-robin") {
			t.Logf("[%s] %s", e.Level, e.Message)
		}
		if contains(e.Message, "account=[Acc-1]") && contains(e.Message, "source=test") {
			count1++
		}
		if contains(e.Message, "account=[Acc-2]") && contains(e.Message, "source=test") {
			count2++
		}
	}
	t.Logf("Acc-1: %d, Acc-2: %d", count1, count2)

	if count1 != 5 {
		t.Errorf("Acc-1 should have 5 slots, got %d", count1)
	}
	if count2 != 2 {
		t.Errorf("Acc-2 should have 2 slots, got %d", count2)
	}
}

func TestDispatchRoundRobinSlotExpiry(t *testing.T) {
	dir := t.TempDir()
	cfgPath := dir + "/config.json"

	cfg := config.Config{
		Server:   config.ServerConfig{Port: 0},
		Dispatch: "round-robin",
		Tasks: []config.TaskConfig{
			{ID: "a1", Name: "Solo", Enabled: true, APIUrl: "http://a", Method: "GET"},
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

	sig := Signal{Action: "buy", Symbol: "BTCUSDT"}

	// Fill 5 slots
	for i := 0; i < 5; i++ {
		sig.OrderID = int64(i)
		_ = proc.Handle("test", sig, false)
	}

	// Manually age the first slot to 31 minutes ago, then expire it
	proc.mu.Lock()
	slotKey := cfg.Tasks[0].SlotGroupKey()
	slots := proc.orderSlots[slotKey]
	if len(slots) > 0 {
		slots[0] = time.Now().Add(-31 * time.Minute)
		proc.orderSlots[slotKey] = slots
	}
	proc.mu.Unlock()
	proc.expireSlots()

	// Now one slot should have expired, making room
	sig.OrderID = 99
	_ = proc.Handle("test", sig, false)

	count := 0
	for _, e := range logger.Entries() {
		if contains(e.Message, "account=[Solo]") && contains(e.Message, "source=test") {
			count++
			t.Logf("[%s] %s", e.Level, e.Message)
		}
	}
	if count < 6 {
		t.Errorf("expected 6 orders after slot expiry (5 + 1), got %d", count)
	}
}

func TestSlotGroupSharedAcrossTasks(t *testing.T) {
	dir := t.TempDir()
	cfgPath := dir + "/config.json"

	cfg := config.Config{
		Server:   config.ServerConfig{Port: 0},
		Dispatch: "round-robin",
		Tasks: []config.TaskConfig{
			{ID: "a1", Name: "A1", Enabled: true, Group: "same", APIUrl: "http://a", Method: "GET"},
			{ID: "a2", Name: "A2", Enabled: true, Group: "same", APIUrl: "http://b", Method: "GET"},
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

	sig := Signal{Action: "buy", Symbol: "BTCUSDT"}
	for i := 0; i < 6; i++ {
		sig.OrderID = int64(i)
		_ = proc.Handle("test", sig, false)
	}

	count := 0
	for _, e := range logger.Entries() {
		if e.Source == "signal" && contains(e.Message, "source=test") {
			count++
		}
	}
	if count != 5 {
		t.Errorf("expected 5 executed signals with shared group of 5 slots, got %d", count)
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
