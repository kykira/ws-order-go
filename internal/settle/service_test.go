package settle

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/kykira/ws-order-go/internal/config"
	"github.com/kykira/ws-order-go/internal/logs"
	"github.com/kykira/ws-order-go/internal/orders"
)

func newTestConfigManager(t *testing.T, tasks ...config.TaskConfig) *config.Manager {
	t.Helper()
	mgr, err := config.LoadManager(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := mgr.Update(func(c *config.Config) { c.Tasks = tasks }); err != nil {
		t.Fatal(err)
	}
	return mgr
}

func TestPollClassifiesWinLossPending(t *testing.T) {
	now := time.Now()
	at := func(d time.Duration) time.Time { return now.Add(d) }

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if got := r.Header.Get("csrftoken"); got != "csrf" {
			t.Errorf("csrftoken = %q", got)
		}
		if c, err := r.Cookie("p20t"); err != nil || c.Value != "p20t" {
			t.Errorf("p20t cookie = %v, %v", c, err)
		}
		var body struct {
			StartTime int64 `json:"startTime"`
			EndTime   int64 `json:"endTime"`
			Page      int64 `json:"page"`
			Rows      int64 `json:"rows"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode body: %v", err)
		}
		if body.Rows != rowsPerPage || body.Page != 1 {
			t.Errorf("body = %+v", body)
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": "000000", "success": true, "total": 4,
			"data": []map[string]any{
				{"tranId": 1, "balancetype": "EVENT_CONTRACTS_ORDER", "balanceDelta": -125.0, "balanceInfo": "a", "time": at(-10 * time.Minute).UnixMilli()},
				{"tranId": 2, "balancetype": "EVENT_CONTRACTS_PAYOUT", "balanceDelta": 231.25, "balanceInfo": "a", "time": at(-5 * time.Minute).UnixMilli()},
				{"tranId": 3, "balancetype": "EVENT_CONTRACTS_ORDER", "balanceDelta": -100.0, "balanceInfo": "b", "time": at(-3 * time.Hour).UnixMilli()},
				{"tranId": 4, "balancetype": "EVENT_CONTRACTS_ORDER", "balanceDelta": -50.0, "balanceInfo": "c", "time": at(-1 * time.Minute).UnixMilli()},
			},
		})
	}))
	defer srv.Close()

	old := txHistoryURL
	txHistoryURL = srv.URL
	defer func() { txHistoryURL = old }()

	mgr := newTestConfigManager(t, config.TaskConfig{
		ID: "bn1", Name: "bn1", Type: "binance", Enabled: true,
		Auth: map[string]string{"csrftoken": "csrf", "p20t": "p20t"},
	})

	store := orders.NewStore(50)
	add := func(amount string, when time.Time) int64 {
		id := store.Begin(orders.Record{
			Account: "bn1", TaskID: "bn1", Amount: amount, Period: "30m",
			Action: "buy", Symbol: "BTCUSDT",
		})
		store.Update(id, func(r *orders.Record) { r.Time = when })
		return id
	}
	winID := add("125", at(-10*time.Minute))
	lossID := add("100", at(-3*time.Hour))
	pendingID := add("50", at(-1*time.Minute))

	svc := NewService(mgr, logs.NewLogger(20), store)
	svc.pollOnce(context.Background())

	snap := svc.Snapshot()
	if snap.WindowStart.IsZero() {
		t.Fatal("window start is zero")
	}
	if len(snap.Accounts) != 1 {
		t.Fatalf("accounts = %d, want 1", len(snap.Accounts))
	}
	st := snap.Accounts[0]
	if st.Orders != 3 || st.Wins != 1 || st.Losses != 1 || st.Pending != 1 {
		t.Fatalf("counts = %+v", st)
	}
	if math.Abs(st.Staked-275) > 0.001 || math.Abs(st.Payout-231.25) > 0.001 || math.Abs(st.Net+43.75) > 0.001 {
		t.Fatalf("amounts = %+v", st)
	}
	if st.Error != "" {
		t.Fatalf("unexpected error: %s", st.Error)
	}

	byID := make(map[int64]orders.Record)
	for _, r := range store.List(10) {
		byID[r.ID] = r
	}
	if got := byID[winID]; got.Result != "win" || math.Abs(got.Payout-231.25) > 0.001 || got.SettledAt == nil {
		t.Fatalf("win record = %+v", got)
	}
	if got := byID[lossID]; got.Result != "loss" || got.SettledAt == nil {
		t.Fatalf("loss record = %+v", got)
	}
	if got := byID[pendingID]; got.Result != "pending" || got.SettledAt != nil {
		t.Fatalf("pending record = %+v", got)
	}
}

func TestMissingCredentialsReportsError(t *testing.T) {
	mgr := newTestConfigManager(t, config.TaskConfig{
		ID: "bn1", Name: "bn1", Type: "binance", Enabled: true,
	})
	svc := NewService(mgr, logs.NewLogger(20), orders.NewStore(10))
	svc.pollOnce(context.Background())

	snap := svc.Snapshot()
	if len(snap.Accounts) != 1 || snap.Accounts[0].Error == "" {
		t.Fatalf("snapshot = %+v", snap)
	}
}

func TestWindowStart(t *testing.T) {
	cst := time.FixedZone("CST", 8*60*60)
	mk := func(y int, mo time.Month, d, h, mi int) time.Time {
		return time.Date(y, mo, d, h, mi, 0, 0, cst)
	}

	cases := []struct{ now, want time.Time }{
		{mk(2026, 10, 31, 13, 0), mk(2026, 10, 31, 0, 0)},
		{mk(2026, 10, 31, 9, 0), mk(2026, 10, 31, 0, 0)},
		{mk(2026, 10, 31, 0, 0), mk(2026, 10, 31, 0, 0)},
		{mk(2026, 10, 31, 23, 59), mk(2026, 10, 31, 0, 0)},
		{mk(2026, 11, 1, 0, 30), mk(2026, 11, 1, 0, 0)},
	}
	for _, c := range cases {
		if got := windowStart(c.now); !got.Equal(c.want) {
			t.Errorf("windowStart(%s) = %s, want %s", c.now, got, c.want)
		}
	}

	// 16:30 UTC is already 00:30 the next day in CST.
	utc := time.Date(2026, 10, 30, 16, 30, 0, 0, time.UTC)
	if got, want := windowStart(utc), mk(2026, 10, 31, 0, 0); !got.Equal(want) {
		t.Errorf("windowStart(%s) = %s, want %s", utc, got, want)
	}
}

func TestPeriodDuration(t *testing.T) {
	cases := map[string]time.Duration{
		"30s":   30 * time.Second,
		"5m":    5 * time.Minute,
		"30m":   30 * time.Minute,
		"4h":    4 * time.Hour,
		"":      maxSettle,
		"bogus": maxSettle,
	}
	for p, want := range cases {
		if got := periodDuration(p); got != want {
			t.Errorf("periodDuration(%q) = %v, want %v", p, got, want)
		}
	}
}
