package signals

import (
	"strings"
	"testing"

	"github.com/kykira/ws-order-go/internal/config"
	"github.com/kykira/ws-order-go/internal/logs"
	"github.com/kykira/ws-order-go/internal/order"
)

func newSizingProcessor(balances map[string]float64) *Processor {
	p := &Processor{logger: logs.NewLogger(100)}
	if balances != nil {
		p.balanceProvider = fakeBalanceProvider(balances)
	}
	return p
}

// The requested behaviour: when percent × balance lands below the configured
// minimum position, the minimum is used for the order.
func TestDynamicAmountUsesMinimumPosition(t *testing.T) {
	p := newSizingProcessor(map[string]float64{"acct-a": 100})

	got, ok := p.dynamicAmount(config.StrategyGroupAccountConfig{
		AccountID: "acct-a", Percent: 1, MinAmount: 5,
	})
	if !ok {
		t.Fatal("expected a usable amount")
	}
	if got != "5.0" {
		t.Fatalf("1%% of 100 with minAmount=5: want \"5.0\", got %q", got)
	}
}

func TestDynamicAmountUsesPercentageAboveMinimum(t *testing.T) {
	p := newSizingProcessor(map[string]float64{"acct-a": 1000})

	got, ok := p.dynamicAmount(config.StrategyGroupAccountConfig{
		AccountID: "acct-a", Percent: 2, MinAmount: 5,
	})
	if !ok {
		t.Fatal("expected a usable amount")
	}
	if got != "20.0" {
		t.Fatalf("2%% of 1000 with minAmount=5: want \"20.0\", got %q", got)
	}
}

func TestDynamicAmountHonoursMaxAmount(t *testing.T) {
	p := newSizingProcessor(map[string]float64{"acct-a": 1000})

	got, ok := p.dynamicAmount(config.StrategyGroupAccountConfig{
		AccountID: "acct-a", Percent: 10, MaxAmount: 20,
	})
	if !ok || got != "20.0" {
		t.Fatalf("10%% of 1000 capped at 20: want \"20.0\", got %q (ok=%v)", got, ok)
	}
}

// A balance that is simply unknown (first fetch not finished, fetch failed, or
// an account type the balance service does not cover) must never produce a
// zero-sized order.
func TestDynamicAmountUnknownBalanceNeverZero(t *testing.T) {
	cases := []struct {
		name    string
		p       *Processor
		binding config.StrategyGroupAccountConfig
		want    string
		wantOK  bool
	}{
		{
			name:    "missing key falls back to minAmount",
			p:       newSizingProcessor(map[string]float64{}),
			binding: config.StrategyGroupAccountConfig{AccountID: "acct-a", Percent: 10, MinAmount: 7},
			want:    "7.0",
			wantOK:  true,
		},
		{
			name:    "nil provider falls back to minAmount",
			p:       newSizingProcessor(nil),
			binding: config.StrategyGroupAccountConfig{AccountID: "acct-a", Percent: 10, MinAmount: 7},
			want:    "7.0",
			wantOK:  true,
		},
		{
			name:    "missing key falls back to static amount",
			p:       newSizingProcessor(map[string]float64{}),
			binding: config.StrategyGroupAccountConfig{AccountID: "acct-a", Percent: 10, Amount: "13"},
			want:    "13",
			wantOK:  true,
		},
		{
			name:    "zero balance falls back to static amount",
			p:       newSizingProcessor(map[string]float64{"acct-a": 0}),
			binding: config.StrategyGroupAccountConfig{AccountID: "acct-a", Percent: 10, Amount: "13"},
			want:    "13",
			wantOK:  true,
		},
		{
			name:    "nothing usable must be skipped, not zero",
			p:       newSizingProcessor(map[string]float64{}),
			binding: config.StrategyGroupAccountConfig{AccountID: "acct-a", Percent: 10},
			want:    "",
			wantOK:  false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := c.p.dynamicAmount(c.binding)
			if got == "0.0" {
				t.Fatalf("dynamicAmount returned a zero-sized order (%q)", got)
			}
			if ok != c.wantOK || got != c.want {
				t.Fatalf("want (%q, %v), got (%q, %v)", c.want, c.wantOK, got, ok)
			}
		})
	}
}

// Static (non-percent) bindings keep their previous behaviour.
func TestDynamicAmountStaticModeUnchanged(t *testing.T) {
	p := newSizingProcessor(nil)

	got, ok := p.dynamicAmount(config.StrategyGroupAccountConfig{AccountID: "acct-a", Amount: " 13 "})
	if !ok || got != "13" {
		t.Fatalf("static amount: want (\"13\", true), got (%q, %v)", got, ok)
	}

	got, ok = p.dynamicAmount(config.StrategyGroupAccountConfig{AccountID: "acct-a"})
	if !ok || got != "" {
		t.Fatalf("unset static amount: want (\"\", true) so the signal amount is used, got (%q, %v)", got, ok)
	}
}

// An account whose dynamic amount cannot be resolved must be dropped from the
// group rather than being dispatched with an invalid amount.
func TestGroupDispatchSkipsUnresolvableAccount(t *testing.T) {
	dir := t.TempDir()
	cfgPath := dir + "/config.json"

	cfg := config.Config{
		Server: config.ServerConfig{Port: 0},
		Tasks: []config.TaskConfig{
			{ID: "a1", Name: "A1", Enabled: true},
		},
		Strategies: []config.StrategyConfig{
			{
				ID: "st-1", Name: "eth-30m", Enabled: true,
				Groups: []config.StrategyGroupConfig{
					{
						ID: "g1", Name: "G1", Enabled: true, Dispatch: "all",
						Accounts: []config.StrategyGroupAccountConfig{
							{AccountID: "a1", Percent: 5}, // no balance, no minAmount, no amount
						},
					},
				},
			},
		},
	}
	mgr, err := config.LoadManager(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := mgr.Update(func(c *config.Config) { *c = cfg }); err != nil {
		t.Fatal(err)
	}

	logger := logs.NewLogger(100)
	proc := NewProcessor(mgr, logger, order.NewClient(logger))

	if err := proc.Handle("test", Signal{Action: "buy", Symbol: "ETHUSDT", Strategy: "st-1", Period: "30m"}, false); err != nil {
		t.Fatal(err)
	}

	for _, e := range logger.Entries() {
		if e.Source == "signal" && contains(e.Message, "source=test") {
			t.Fatalf("account with an unresolvable amount should not be dispatched: %s", e.Message)
		}
	}
}

// ── 分组内优先级 + 顺序轮询 ──

func TestNextRoundRobinIndexWraps(t *testing.T) {
	p := &Processor{}
	var got []int
	for i := 0; i < 5; i++ {
		got = append(got, p.nextRoundRobinIndex("k", 3))
	}
	want := []int{0, 1, 2, 0, 1}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("cursor sequence = %v, want %v", got, want)
		}
	}

	// Independent keys have independent cursors.
	if i := p.nextRoundRobinIndex("other", 3); i != 0 {
		t.Fatalf("independent cursor started at %d, want 0", i)
	}
	// Single account: always 0.
	if i := p.nextRoundRobinIndex("k", 1); i != 0 {
		t.Fatalf("n=1 returned %d, want 0", i)
	}
}

func TestSortByPriorityKeepsConfigOrderForTies(t *testing.T) {
	matched := []groupAccountMatch{
		{task: config.TaskConfig{ID: "a"}, priority: 5},
		{task: config.TaskConfig{ID: "b"}, priority: 10},
		{task: config.TaskConfig{ID: "c"}, priority: 10},
		{task: config.TaskConfig{ID: "d"}, priority: 0},
	}
	sortByPriority(matched)

	got := []string{matched[0].task.ID, matched[1].task.ID, matched[2].task.ID, matched[3].task.ID}
	want := []string{"b", "c", "a", "d"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("sorted order = %v, want %v", got, want)
		}
	}
}

func TestPickRoundRobinStaysInTopPriorityTier(t *testing.T) {
	p := &Processor{}
	matched := []groupAccountMatch{
		{task: config.TaskConfig{ID: "a", Name: "a"}, priority: 10},
		{task: config.TaskConfig{ID: "b", Name: "b"}, priority: 10},
		{task: config.TaskConfig{ID: "c", Name: "c"}, priority: 5},
	}

	var got []string
	for i := 0; i < 6; i++ {
		got = append(got, p.pickRoundRobinGroupAccount("g1", matched).task.ID)
	}
	want := []string{"a", "b", "a", "b", "a", "b"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("round-robin = %v, want %v (low priority account must not be picked)", got, want)
		}
	}

	// Each group rotates independently.
	if id := p.pickRoundRobinGroupAccount("g2", matched).task.ID; id != "a" {
		t.Fatalf("second group should start at its own cursor, got %q", id)
	}

	// Equal priorities everywhere → plain sequential rotation.
	flat := []groupAccountMatch{
		{task: config.TaskConfig{ID: "x"}, priority: 0},
		{task: config.TaskConfig{ID: "y"}, priority: 0},
		{task: config.TaskConfig{ID: "z"}, priority: 0},
	}
	var seq []string
	for i := 0; i < 4; i++ {
		seq = append(seq, p.pickRoundRobinGroupAccount("flat", flat).task.ID)
	}
	for i, wantID := range []string{"x", "y", "z", "x"} {
		if seq[i] != wantID {
			t.Fatalf("flat rotation = %v, want [x y z x]", seq)
		}
	}
}

// End-to-end: dispatch=round-robin with priorities 10/10/5 alternates the two
// priority-10 accounts and never touches the lower-priority fallback.
func TestGroupRoundRobinPriorityIntegration(t *testing.T) {
	dir := t.TempDir()
	cfgPath := dir + "/config.json"

	cfg := config.Config{
		Server: config.ServerConfig{Port: 0},
		Tasks: []config.TaskConfig{
			{ID: "a1", Name: "A1", Enabled: true},
			{ID: "a2", Name: "A2", Enabled: true},
			{ID: "a3", Name: "A3", Enabled: true},
		},
		Strategies: []config.StrategyConfig{
			{
				ID: "st-1", Name: "eth-30m", Enabled: true,
				Groups: []config.StrategyGroupConfig{
					{
						ID: "g1", Name: "G1", Enabled: true, Dispatch: "round-robin",
						Accounts: []config.StrategyGroupAccountConfig{
							{AccountID: "a1", Amount: "1", Priority: 10},
							{AccountID: "a2", Amount: "1", Priority: 10},
							{AccountID: "a3", Amount: "1", Priority: 5},
						},
					},
				},
			},
		},
	}
	mgr, err := config.LoadManager(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := mgr.Update(func(c *config.Config) { *c = cfg }); err != nil {
		t.Fatal(err)
	}

	logger := logs.NewLogger(500)
	proc := NewProcessor(mgr, logger, order.NewClient(logger))

	for i := 0; i < 4; i++ {
		if err := proc.Handle("test", Signal{Action: "buy", Symbol: "ETHUSDT", Strategy: "st-1", Period: "30m"}, false); err != nil {
			t.Fatal(err)
		}
	}

	var picked []string
	for _, e := range logger.Entries() {
		if e.Source != "signal" || !strings.Contains(e.Message, "dispatch=group round-robin") {
			continue
		}
		if i := strings.Index(e.Message, "account=["); i >= 0 {
			rest := e.Message[i+len("account=["):]
			if j := strings.Index(rest, "]"); j >= 0 {
				picked = append(picked, rest[:j])
			}
		}
	}

	want := []string{"A1", "A2", "A1", "A2"}
	if len(picked) != len(want) {
		t.Fatalf("picked %v, want %v", picked, want)
	}
	for i := range want {
		if picked[i] != want[i] {
			t.Fatalf("picked %v, want %v", picked, want)
		}
	}
}
