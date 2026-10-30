// Package settle polls the Binance event-contract transaction history (balance
// ledger) and derives win/loss results for the orders placed by the bridge.
//
// The ledger has no order id and the place-order API returns none, so results
// are paired by Binance's balanceInfo (shared by an order debit and its payout)
// and matched to local order records by account + amount + nearest time.
package settle

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kykira/ws-order-go/internal/config"
	"github.com/kykira/ws-order-go/internal/logs"
	"github.com/kykira/ws-order-go/internal/orders"
)

var txHistoryURL = "https://www.binance.com/bapi/futures/v1/private/future/user-data/transaction-history"

const (
	windowHours  = 24
	rowsPerPage  = 100
	maxPages     = 5
	pollInterval = 2 * time.Minute
	matchWindow  = 30 * time.Second
	maxSettle    = 4 * time.Hour
	settleBuffer = 5 * time.Minute
)

type ledgerEntry struct {
	TranID      int64   `json:"tranId"`
	BalanceType string  `json:"balancetype"`
	Delta       float64 `json:"balanceDelta"`
	BalanceInfo string  `json:"balanceInfo"`
	Time        int64   `json:"time"`
}

type ledgerResp struct {
	Code    string        `json:"code"`
	Message string        `json:"message"`
	Data    []ledgerEntry `json:"data"`
	Total   int           `json:"total"`
	Success bool          `json:"success"`
}

// AccountStat is the settlement summary for one Binance account.
type AccountStat struct {
	AccountID   string  `json:"accountId"`
	AccountName string  `json:"accountName"`
	Orders      int     `json:"orders"`
	Wins        int     `json:"wins"`
	Losses      int     `json:"losses"`
	Pending     int     `json:"pending"`
	Staked      float64 `json:"staked"`
	Payout      float64 `json:"payout"`
	Net         float64 `json:"net"`
	Error       string  `json:"error,omitempty"`
}

// Snapshot is the API payload for the panel.
type Snapshot struct {
	UpdatedAt   time.Time     `json:"updatedAt"`
	WindowHours int           `json:"windowHours"`
	Accounts    []AccountStat `json:"accounts"`
}

// Service periodically reconciles Binance settlements.
type Service struct {
	cfg    *config.Manager
	logger *logs.Logger
	orders *orders.Store
	client *http.Client

	mu        sync.RWMutex
	stats     map[string]AccountStat
	updatedAt time.Time

	startOnce sync.Once
	stopOnce  sync.Once
	started   atomic.Bool
	stopCh    chan struct{}
	doneCh    chan struct{}
}

func NewService(cfg *config.Manager, logger *logs.Logger, orderStore *orders.Store) *Service {
	return &Service{
		cfg:    cfg,
		logger: logger,
		orders: orderStore,
		client: &http.Client{Timeout: 20 * time.Second},
		stats:  make(map[string]AccountStat),
		stopCh: make(chan struct{}),
		doneCh: make(chan struct{}),
	}
}

// Start begins polling. A second call is a no-op.
func (s *Service) Start() {
	s.startOnce.Do(func() {
		s.started.Store(true)
		go s.loop()
	})
}

// Stop ends polling and waits for the loop to exit. Safe before Start and
// when called twice.
func (s *Service) Stop() {
	if s == nil || !s.started.Load() {
		return
	}
	s.stopOnce.Do(func() {
		close(s.stopCh)
		<-s.doneCh
	})
}

func (s *Service) loop() {
	defer close(s.doneCh)
	s.pollOnce(context.Background())

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.stopCh:
			return
		case <-ticker.C:
			s.pollOnce(context.Background())
		}
	}
}

func (s *Service) pollOnce(ctx context.Context) {
	cfg := s.cfg.Get()
	now := time.Now()
	next := make(map[string]AccountStat)

	for _, task := range cfg.Tasks {
		if task.Type != "binance" || !task.Enabled {
			continue
		}
		if strings.TrimSpace(task.Auth["csrftoken"]) == "" || strings.TrimSpace(task.Auth["p20t"]) == "" {
			next[task.ID] = AccountStat{AccountID: task.ID, AccountName: task.Name, Error: "missing csrftoken or p20t"}
			continue
		}

		stat, err := s.pollAccount(ctx, task, now)
		if err != nil {
			s.logger.Error("settle", fmt.Sprintf("task=[%s] transaction history: %v", task.Name, err))
			s.mu.RLock()
			prev, ok := s.stats[task.ID]
			s.mu.RUnlock()
			if ok {
				prev.Error = err.Error()
				next[task.ID] = prev
			} else {
				next[task.ID] = AccountStat{AccountID: task.ID, AccountName: task.Name, Error: err.Error()}
			}
			continue
		}
		next[task.ID] = stat
	}

	s.mu.Lock()
	s.stats = next
	s.updatedAt = now
	s.mu.Unlock()
}

func (s *Service) pollAccount(ctx context.Context, task config.TaskConfig, now time.Time) (AccountStat, error) {
	entries, err := s.fetchLedger(ctx, task, now.Add(-time.Duration(windowHours)*time.Hour), now)
	if err != nil {
		return AccountStat{}, err
	}

	payouts := make(map[string]float64)
	payoutTimes := make(map[string]int64)
	for _, e := range entries {
		if e.BalanceType != "EVENT_CONTRACTS_PAYOUT" {
			continue
		}
		payouts[e.BalanceInfo] += e.Delta
		if e.Time > payoutTimes[e.BalanceInfo] {
			payoutTimes[e.BalanceInfo] = e.Time
		}
	}

	stat := AccountStat{AccountID: task.ID, AccountName: task.Name}
	seen := make(map[int64]bool)
	for _, e := range entries {
		if e.BalanceType != "EVENT_CONTRACTS_ORDER" || seen[e.TranID] {
			continue
		}
		seen[e.TranID] = true

		amount := -e.Delta
		if amount <= 0 {
			continue
		}
		orderTime := time.UnixMilli(e.Time)
		id, period, matched := s.orders.FindMatch(task.ID, amount, orderTime, matchWindow)

		result := "pending"
		var payout float64
		var settledAt time.Time
		if p := payouts[e.BalanceInfo]; p > 0 {
			result = "win"
			payout = p
			settledAt = time.UnixMilli(payoutTimes[e.BalanceInfo])
		} else {
			deadline := orderTime.Add(maxSettle + settleBuffer)
			if matched {
				deadline = orderTime.Add(periodDuration(period) + settleBuffer)
			}
			if now.After(deadline) {
				result = "loss"
				settledAt = deadline
			}
		}

		if matched {
			s.orders.SetResult(id, result, payout, settledAt)
		}

		stat.Orders++
		stat.Staked += amount
		switch result {
		case "win":
			stat.Wins++
			stat.Payout += payout
		case "loss":
			stat.Losses++
		default:
			stat.Pending++
		}
	}
	stat.Net = stat.Payout - stat.Staked
	return stat, nil
}

func (s *Service) fetchLedger(ctx context.Context, task config.TaskConfig, start, end time.Time) ([]ledgerEntry, error) {
	var out []ledgerEntry
	seen := make(map[int64]bool)

	for page := 1; page <= maxPages; page++ {
		resp, err := s.fetchPage(ctx, task, start, end, page)
		if err != nil {
			return nil, err
		}
		if resp.Code != "000000" {
			return nil, fmt.Errorf("code=%s message=%s", resp.Code, resp.Message)
		}
		for _, e := range resp.Data {
			if !seen[e.TranID] {
				seen[e.TranID] = true
				out = append(out, e)
			}
		}
		if len(resp.Data) < rowsPerPage || (resp.Total > 0 && page*rowsPerPage >= resp.Total) {
			break
		}
	}
	return out, nil
}

func (s *Service) fetchPage(ctx context.Context, task config.TaskConfig, start, end time.Time, page int) (*ledgerResp, error) {
	body, err := json.Marshal(map[string]int64{
		"startTime": start.UnixMilli(),
		"endTime":   end.UnixMilli(),
		"page":      int64(page),
		"rows":      rowsPerPage,
	})
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, txHistoryURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("accept", "application/json, text/plain, */*")
	req.Header.Set("content-type", "application/json")
	req.Header.Set("clienttype", "web")
	req.Header.Set("csrftoken", task.Auth["csrftoken"])
	req.Header.Set("Cookie", "p20t="+task.Auth["p20t"])
	req.Header.Set("lang", "zh-CN")
	req.Header.Set("origin", "https://www.binance.com")
	req.Header.Set("referer", "https://www.binance.com/zh-CN/my/orders/futures/transactionhistory")
	req.Header.Set("user-agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/156.0.0.0 Safari/537.36")

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var lr ledgerResp
	if err := json.NewDecoder(resp.Body).Decode(&lr); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status=%d code=%s", resp.StatusCode, lr.Code)
	}
	return &lr, nil
}

// Snapshot returns the latest per-account stats.
func (s *Service) Snapshot() Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := Snapshot{UpdatedAt: s.updatedAt, WindowHours: windowHours}
	out.Accounts = make([]AccountStat, 0, len(s.stats))
	for _, st := range s.stats {
		out.Accounts = append(out.Accounts, st)
	}
	sort.Slice(out.Accounts, func(i, j int) bool {
		return out.Accounts[i].AccountName < out.Accounts[j].AccountName
	})
	return out
}

// periodDuration maps a configured period to its settlement time. Unknown
// periods fall back to the conservative maxSettle.
func periodDuration(p string) time.Duration {
	switch strings.ToLower(strings.TrimSpace(p)) {
	case "30s":
		return 30 * time.Second
	case "45s":
		return 45 * time.Second
	case "1m":
		return time.Minute
	case "3m":
		return 3 * time.Minute
	case "5m":
		return 5 * time.Minute
	case "10m":
		return 10 * time.Minute
	case "15m":
		return 15 * time.Minute
	case "30m":
		return 30 * time.Minute
	case "1h":
		return time.Hour
	case "2h":
		return 2 * time.Hour
	case "4h":
		return 4 * time.Hour
	default:
		return maxSettle
	}
}
