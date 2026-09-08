package balance

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/kykira/ws-order-go/internal/config"
	"github.com/kykira/ws-order-go/internal/logs"
)

const balanceURL = "https://www.binance.com/bapi/asset/v2/private/asset-service/wallet/balance?quoteAsset=USDT&needBalanceDetail=true&needEuFuture=true&includeOption=true"

// 每日 0 点（北京时间午夜）快照。
var cnLocation = func() *time.Location {
	if loc, err := time.LoadLocation("Asia/Shanghai"); err == nil {
		return loc
	}
	return time.FixedZone("CST", 8*3600)
}()

// Info 是一个币安账号的余额快照。Total 是所有钱包账户的总和，
// Futures 是 U 本位合约账户余额（币安事件合约下单使用的是 UM 钱包）。
type Info struct {
	Total     string    `json:"total"`
	Futures   string    `json:"futures"`
	UpdatedAt time.Time `json:"updatedAt"`
	Error     string    `json:"error,omitempty"`
}

// Summary 是所有币安账号的合计资产 + 最近几日收益。
type Summary struct {
	Total        string        `json:"total"`
	AccountCount int           `json:"accountCount"`
	RecentDays   []DailyProfit `json:"recentDays"`
}

// DailyProfit 是某一天的收益。Profit = 当日总资产 - 收益基准。
type DailyProfit struct {
	Date     string `json:"date"`
	Profit   string `json:"profit"`
	Baseline string `json:"baseline"` // 收益基准：前一日 0 点快照，无记录时为 0.00
}

// Snapshot 是某一天 0 点记录的资产快照。
type Snapshot struct {
	Date       string `json:"date"`
	Total      string `json:"total"`
	RecordedAt string `json:"recordedAt"`
}

type snapshotFile struct {
	Snapshots []Snapshot `json:"snapshots"`
}

type Service struct {
	cfgMgr     *config.Manager
	logger     *logs.Logger
	httpClient *http.Client

	mu           sync.RWMutex
	balances     map[string]Info
	snapshotMu   sync.Mutex
	snapshotPath string
}

func NewService(cfgMgr *config.Manager, logger *logs.Logger) *Service {
	return &Service{
		cfgMgr:       cfgMgr,
		logger:       logger,
		httpClient:   &http.Client{Timeout: 20 * time.Second},
		balances:     map[string]Info{},
		snapshotPath: filepath.Join("data", "daily_balance.json"),
	}
}

// Start 立即拉取一次余额，然后每分钟拉取一次。
func (s *Service) Start() {
	go func() {
		s.FetchAll(context.Background())
		s.recordDailySnapshotIfNeeded(time.Now())

		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			s.FetchAll(context.Background())
			s.recordDailySnapshotIfNeeded(time.Now())
		}
	}()
}

func (s *Service) GetBalances() map[string]Info {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make(map[string]Info, len(s.balances))
	for k, v := range s.balances {
		out[k] = v
	}
	return out
}

// Summary 计算所有币安账号的合计资产与最近几日收益。
func (s *Service) Summary() Summary {
	s.mu.RLock()
	var total float64
	accountCount := 0
	for _, info := range s.balances {
		if info.Error != "" {
			continue
		}
		t, _ := strconv.ParseFloat(info.Total, 64)
		total += t
		accountCount++
	}
	s.mu.RUnlock()

	return Summary{
		Total:        strconv.FormatFloat(total, 'f', 2, 64),
		AccountCount: accountCount,
		RecentDays:   s.recentDailyProfits(total),
	}
}

// recentDailyProfits 根据 0 点快照差值计算最近几天收益。
// 每日收益 = 当日快照 - 前一日快照；没有记录时，收益基准为 0。
func (s *Service) recentDailyProfits(currentTotal float64) []DailyProfit {
	s.snapshotMu.Lock()
	defer s.snapshotMu.Unlock()

	snapshots := s.loadSnapshotsLocked()
	sort.Slice(snapshots, func(i, j int) bool { return snapshots[i].Date < snapshots[j].Date })

	const maxDays = 5
	today := time.Now().In(cnLocation).Format("2006-01-02")

	if len(snapshots) == 0 {
		return []DailyProfit{{
			Date:     today,
			Profit:   strconv.FormatFloat(currentTotal, 'f', 2, 64),
			Baseline: "0.00",
		}}
	}

	out := make([]DailyProfit, 0, maxDays)

	// 第一个快照日：收益基准为 0
	first, _ := strconv.ParseFloat(snapshots[0].Total, 64)
	out = append(out, DailyProfit{
		Date:     snapshots[0].Date,
		Profit:   strconv.FormatFloat(first, 'f', 2, 64),
		Baseline: "0.00",
	})

	// 后续快照日：当日快照 - 前一日快照
	for i := 1; i < len(snapshots); i++ {
		prev, _ := strconv.ParseFloat(snapshots[i-1].Total, 64)
		curr, _ := strconv.ParseFloat(snapshots[i].Total, 64)
		out = append(out, DailyProfit{
			Date:     snapshots[i].Date,
			Profit:   strconv.FormatFloat(curr-prev, 'f', 2, 64),
			Baseline: snapshots[i-1].Total,
		})
	}

	if len(out) > maxDays {
		out = out[len(out)-maxDays:]
	}
	return out
}

// recordDailySnapshotIfNeeded 每天首次成功拉取余额后记录一次当天总资产快照。
// 正常运行时即北京时间 0 点前后；服务当天晚启动时，则以首次成功拉取时的余额作为当天基准。
func (s *Service) recordDailySnapshotIfNeeded(now time.Time) {
	now = now.In(cnLocation)
	date := now.Format("2006-01-02")

	s.snapshotMu.Lock()
	defer s.snapshotMu.Unlock()

	snapshots := s.loadSnapshotsLocked()
	for _, snap := range snapshots {
		if snap.Date == date {
			return
		}
	}

	// 至少有一个成功拉取的账号才记录，避免把 0 资产写成基准
	s.mu.RLock()
	var total float64
	hasAccount := false
	for _, info := range s.balances {
		if info.Error != "" {
			continue
		}
		hasAccount = true
		t, _ := strconv.ParseFloat(info.Total, 64)
		total += t
	}
	s.mu.RUnlock()
	if !hasAccount {
		return
	}

	snapshots = append(snapshots, Snapshot{
		Date:       date,
		Total:      strconv.FormatFloat(total, 'f', 2, 64),
		RecordedAt: now.Format(time.RFC3339),
	})
	s.saveSnapshotsLocked(snapshots)
	s.logger.Info("balance", fmt.Sprintf("recorded daily balance snapshot date=%s total=%s", date, strconv.FormatFloat(total, 'f', 2, 64)))
}

func (s *Service) loadSnapshotsLocked() []Snapshot {
	bs, err := os.ReadFile(s.snapshotPath)
	if err != nil {
		return nil
	}
	var file snapshotFile
	if err := json.Unmarshal(bs, &file); err != nil {
		return nil
	}
	return file.Snapshots
}

func (s *Service) saveSnapshotsLocked(snapshots []Snapshot) {
	if err := os.MkdirAll(filepath.Dir(s.snapshotPath), 0755); err != nil {
		s.logger.Error("balance", fmt.Sprintf("create snapshot dir error: %v", err))
		return
	}
	bs, err := json.MarshalIndent(snapshotFile{Snapshots: snapshots}, "", "  ")
	if err != nil {
		s.logger.Error("balance", fmt.Sprintf("marshal snapshot error: %v", err))
		return
	}
	if err := os.WriteFile(s.snapshotPath, bs, 0644); err != nil {
		s.logger.Error("balance", fmt.Sprintf("write snapshot error: %v", err))
	}
}

// FetchAll 拉取配置中所有币安类型账号的余额。
func (s *Service) FetchAll(ctx context.Context) {
	cfg := s.cfgMgr.Get()
	for _, task := range cfg.Tasks {
		if task.Type != "binance" {
			continue
		}
		csrf := task.Auth["csrftoken"]
		p20t := task.Auth["p20t"]
		if csrf == "" || p20t == "" {
			s.set(task.ID, Info{Error: "missing csrftoken or p20t"})
			continue
		}
		info, err := s.fetchBalance(ctx, csrf, p20t)
		if err != nil {
			s.logger.Error("balance", fmt.Sprintf("task=[%s] fetch balance error: %v", task.Name, err))
			s.set(task.ID, Info{Error: err.Error()})
			continue
		}
		s.set(task.ID, info)
	}
}

func (s *Service) set(taskID string, info Info) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.balances[taskID] = info
}

func (s *Service) fetchBalance(ctx context.Context, csrf, p20t string) (Info, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, balanceURL, nil)
	if err != nil {
		return Info{}, err
	}
	req.Header.Set("clienttype", "web")
	req.Header.Set("csrftoken", csrf)
	req.Header.Set("Cookie", "p20t="+p20t)
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/152.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "*/*")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return Info{}, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return Info{}, err
	}

	var payload struct {
		Code string `json:"code"`
		Data []struct {
			AccountType string `json:"accountType"`
			Balance     string `json:"balance"`
			WalletName  string `json:"walletName"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return Info{}, fmt.Errorf("parse response failed: %w", err)
	}
	if resp.StatusCode != http.StatusOK || payload.Code != "000000" {
		return Info{}, fmt.Errorf("unexpected status=%d code=%s", resp.StatusCode, payload.Code)
	}

	var total, futures float64
	for _, d := range payload.Data {
		v, _ := strconv.ParseFloat(d.Balance, 64)
		total += v
		if d.AccountType == "FUTURE" {
			futures = v
		}
	}

	return Info{
		Total:     strconv.FormatFloat(total, 'f', 2, 64),
		Futures:   strconv.FormatFloat(futures, 'f', 2, 64),
		UpdatedAt: time.Now(),
	}, nil
}
