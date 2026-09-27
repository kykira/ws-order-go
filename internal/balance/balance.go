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
	"strings"
	"sync"
	"time"

	"github.com/kykira/ws-order-go/internal/config"
	"github.com/kykira/ws-order-go/internal/logs"
)

const balanceURL = "https://www.binance.com/bapi/asset/v2/private/asset-service/wallet/balance?quoteAsset=USDT&needBalanceDetail=true&needEuFuture=true&includeOption=true"

const turboflowAssetsURL = "https://apis.turboflow.xyz/account/assets/v2?fill_coin_sub_info=yes"

// 每日 0 点（北京时间午夜）快照。
var cnLocation = func() *time.Location {
	if loc, err := time.LoadLocation("Asia/Shanghai"); err == nil {
		return loc
	}
	return time.FixedZone("CST", 8*3600)
}()

// Info 是一个交易账号的余额快照。对于币安，Total 是所有钱包账户的总和，
// Futures 是 U 本位合约账户余额；对于 TurboFlow，Total/Futures 均为 USDT
// 可用余额。余额快照用于页面展示和 weighted 随机权重。
type Info struct {
	Platform  string    `json:"platform,omitempty"`
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

// GetFuturesBalances 返回可用于随机权重的账号余额。
// 优先使用 Futures；若 Futures 为 0/解析失败，则退回 Total。
func (s *Service) GetFuturesBalances() map[string]float64 {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make(map[string]float64, len(s.balances))
	for id, info := range s.balances {
		if info.Error != "" {
			continue
		}
		v, err := strconv.ParseFloat(strings.TrimSpace(info.Futures), 64)
		if err != nil || v <= 0 {
			v, _ = strconv.ParseFloat(strings.TrimSpace(info.Total), 64)
		}
		if v < 0 {
			v = 0
		}
		out[id] = v
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
		if info.Platform != "" && info.Platform != "binance" {
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

// recentDailyProfits 计算最近几天收益，按“起始日”归属：
// 某日收益 = 次日 0 点快照 - 当日 0 点快照；
// 当天 = 实时总资产 - 当天 0 点快照（昨天 24 点记录下来的余额）。
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

	// 每个快照日 = 次日 0 点快照 - 当日 0 点快照
	for i := 0; i < len(snapshots)-1; i++ {
		curr, _ := strconv.ParseFloat(snapshots[i].Total, 64)
		next, _ := strconv.ParseFloat(snapshots[i+1].Total, 64)
		out = append(out, DailyProfit{
			Date:     snapshots[i].Date,
			Profit:   strconv.FormatFloat(next-curr, 'f', 2, 64),
			Baseline: snapshots[i].Total,
		})
	}

	// 当天：实时总资产 - 最近一次 0 点快照
	last := snapshots[len(snapshots)-1]
	lastTotal, _ := strconv.ParseFloat(last.Total, 64)
	out = append(out, DailyProfit{
		Date:     today,
		Profit:   strconv.FormatFloat(currentTotal-lastTotal, 'f', 2, 64),
		Baseline: last.Total,
	})

	if len(out) > maxDays {
		out = out[len(out)-maxDays:]
	}
	return out
}

// recordDailySnapshotIfNeeded 只在北京时间 0 点记录当天总资产快照。
// 服务当天晚启动时不补记，当天收益会使用最近一次快照（通常是昨天 24 点余额）作为基准。
func (s *Service) recordDailySnapshotIfNeeded(now time.Time) {
	now = now.In(cnLocation)
	if now.Hour() != 0 {
		return
	}
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

// FetchAll 拉取配置中所有币安/TurboFlow 类型账号的余额。
func (s *Service) FetchAll(ctx context.Context) {
	cfg := s.cfgMgr.Get()
	for _, task := range cfg.Tasks {
		if !task.Enabled {
			s.delete(task.ID)
			continue
		}
		switch task.Type {
		case "binance":
			csrf := task.Auth["csrftoken"]
			p20t := task.Auth["p20t"]
			if csrf == "" || p20t == "" {
				s.set(task.ID, Info{Platform: "binance", Error: "missing csrftoken or p20t"})
				continue
			}
			info, err := s.fetchBalance(ctx, csrf, p20t)
			if err != nil {
				s.logger.Error("balance", fmt.Sprintf("task=[%s] fetch balance error: %v", task.Name, err))
				s.set(task.ID, Info{Platform: "binance", Error: err.Error()})
				continue
			}
			info.Platform = "binance"
			s.set(task.ID, info)

		case "turboflow":
			authorization := task.Auth["authorization"]
			if strings.TrimSpace(authorization) == "" {
				s.set(task.ID, Info{Platform: "turboflow", Error: "missing authorization"})
				continue
			}
			info, err := s.fetchTurboFlowBalance(ctx, task)
			if err != nil {
				s.logger.Error("balance", fmt.Sprintf("task=[%s] fetch TurboFlow balance error: %v", task.Name, err))
				s.set(task.ID, Info{Platform: "turboflow", Error: err.Error()})
				continue
			}
			s.set(task.ID, info)
		}
	}
}

func (s *Service) set(taskID string, info Info) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.balances[taskID] = info
}

func (s *Service) delete(taskID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.balances, taskID)
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

func (s *Service) fetchTurboFlowBalance(ctx context.Context, task config.TaskConfig) (Info, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, turboflowAssetsURL, nil)
	if err != nil {
		return Info{}, err
	}
	req.Header.Set("accept", "application/json, text/plain, */*")
	req.Header.Set("authorization", task.Auth["authorization"])
	req.Header.Set("biz-pf", task.Auth["biz-pf"])
	req.Header.Set("lang", "zh-cn")
	req.Header.Set("origin", "https://www.turboflow.xyz")
	req.Header.Set("referer", "https://www.turboflow.xyz/")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/153.0.0.0 Safari/537.36")

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
		Errno string `json:"errno"`
		Msg   string `json:"msg"`
		Data  struct {
			List []struct {
				CoinCode         string `json:"coin_code"`
				CoinName         string `json:"coin_name"`
				AvailableBalance string `json:"available_balance"`
			} `json:"list"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return Info{}, fmt.Errorf("parse response failed: %w", err)
	}
	if resp.StatusCode != http.StatusOK || payload.Errno != "200" {
		return Info{}, fmt.Errorf("unexpected status=%d errno=%s msg=%s", resp.StatusCode, payload.Errno, payload.Msg)
	}

	for _, asset := range payload.Data.List {
		if asset.CoinCode != "1" && !strings.EqualFold(asset.CoinName, "USDT") {
			continue
		}
		v, err := strconv.ParseFloat(strings.TrimSpace(asset.AvailableBalance), 64)
		if err != nil {
			return Info{}, fmt.Errorf("parse USDT available_balance failed: %w", err)
		}
		balance := strconv.FormatFloat(v, 'f', 2, 64)
		return Info{
			Platform:  "turboflow",
			Total:     balance,
			Futures:   balance,
			UpdatedAt: time.Now(),
		}, nil
	}

	return Info{}, fmt.Errorf("USDT asset not found in TurboFlow response")
}
