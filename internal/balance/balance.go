package balance

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
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

// 接口地址是变量而非常量，便于测试指向 httptest 服务。
var balanceURL = "https://www.binance.com/bapi/asset/v2/private/asset-service/wallet/balance?quoteAsset=USDT&needBalanceDetail=true&needEuFuture=true&includeOption=true"

var turboflowAssetsURL = "https://apis.turboflow.xyz/account/assets/v2?fill_coin_sub_info=yes"

// HiBT（taichuwuji 白标域名）期货账户余额。v 为毫秒时间戳防缓存参数。
var hibtBalanceURL = "https://api-ws.taichuwuji.com/rest/c/future/u/user/balance"

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

	startOnce sync.Once
	stopOnce  sync.Once
	stopCh    chan struct{}
}

func NewService(cfgMgr *config.Manager, logger *logs.Logger) *Service {
	return &Service{
		cfgMgr:       cfgMgr,
		logger:       logger,
		httpClient:   &http.Client{Timeout: 20 * time.Second},
		balances:     map[string]Info{},
		snapshotPath: filepath.Join("data", "daily_balance.json"),
		stopCh:       make(chan struct{}),
	}
}

// Start 立即拉取一次余额，然后每 30 秒拉取一次。重复调用是安全的（只会启动一个轮询协程）。
func (s *Service) Start() {
	s.startOnce.Do(func() {
		go func() {
			s.FetchAll(context.Background())
			s.recordDailySnapshotIfNeeded(time.Now())

			ticker := time.NewTicker(30 * time.Second) // ★ 30 秒刷新账户可用额(2026-10-14)
			defer ticker.Stop()
			for {
				select {
				case <-s.stopCh:
					return
				case <-ticker.C:
					s.FetchAll(context.Background())
					s.recordDailySnapshotIfNeeded(time.Now())
				}
			}
		}()
	})
}

// Stop 终止后台轮询。可重复调用，未调用过 Start 时也可安全调用。
func (s *Service) Stop() {
	s.stopOnce.Do(func() { close(s.stopCh) })
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
// 优先使用 Futures；若 Futures 为 0/不可用，则退回 Total。
// 只要账号还有可用数值就返回（即使本轮 Error 非空，也会沿用上一次成功拉取的
// 余额），避免一次网络抖动就改变派单权重。
func (s *Service) GetFuturesBalances() map[string]float64 {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make(map[string]float64, len(s.balances))
	for id, info := range s.balances {
		v, ok := usableBalance(info.Futures)
		if !ok || v <= 0 {
			v, ok = usableBalance(info.Total)
		}
		if !ok {
			continue // 没有任何可用数值：不参与加权
		}
		if v < 0 {
			v = 0
		}
		out[id] = v
	}
	return out
}

// Summary 计算所有已接入账号的合计资产与最近几日收益。
// 只要账号还有可用 Total 就计入合计（Error 非空但保留了上次成功余额时也计入），
// 完全没有可用数值的账号才忽略。
func (s *Service) Summary() Summary {
	s.mu.RLock()
	var total float64
	accountCount := 0
	for _, info := range s.balances {
		t, ok := usableBalance(info.Total)
		if !ok {
			continue
		}
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

	// 只有本轮所有被跟踪账号都成功拉取（Error 为空）时才记录：部分账号失败时
	// 写下的基准缺少该账号资产，等它恢复后图表会出现巨额虚假收益；跳过后由
	// 后续轮次（同一天内不会重复记录）或第二天补记完整快照。
	s.mu.RLock()
	var total float64
	tracked, failed := 0, 0
	for _, info := range s.balances {
		tracked++
		if info.Error != "" {
			failed++
			continue
		}
		t, _ := strconv.ParseFloat(strings.TrimSpace(info.Total), 64)
		total += t
	}
	s.mu.RUnlock()
	if tracked == 0 {
		return
	}
	if failed > 0 {
		s.logger.Error("balance", fmt.Sprintf("skip daily balance snapshot date=%s: %d/%d account(s) failed this round", date, failed, tracked))
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
		if !os.IsNotExist(err) {
			s.logger.Error("balance", fmt.Sprintf("read snapshot error: %v", err))
		}
		return nil
	}
	var file snapshotFile
	if err := json.Unmarshal(bs, &file); err != nil {
		// 损坏的快照文件先改名备份再继续，交由人工恢复；否则下一次写入会把它
		// 彻底覆盖，丢失仍然可恢复的历史。
		backup := fmt.Sprintf("%s.bak-%d", s.snapshotPath, time.Now().Unix())
		if renameErr := os.Rename(s.snapshotPath, backup); renameErr != nil {
			s.logger.Error("balance", fmt.Sprintf("backup corrupt snapshot error: %v (unmarshal error: %v)", renameErr, err))
		} else {
			s.logger.Error("balance", fmt.Sprintf("corrupt snapshot file unmarshal error: %v, moved to %s", err, backup))
		}
		return nil
	}
	return file.Snapshots
}

// saveSnapshotsLocked 原子写入：先写同目录临时文件再 Rename 覆盖，
// 这样进程崩溃也不会留下被截断的快照文件。
func (s *Service) saveSnapshotsLocked(snapshots []Snapshot) {
	dir := filepath.Dir(s.snapshotPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		s.logger.Error("balance", fmt.Sprintf("create snapshot dir error: %v", err))
		return
	}
	bs, err := json.MarshalIndent(snapshotFile{Snapshots: snapshots}, "", "  ")
	if err != nil {
		s.logger.Error("balance", fmt.Sprintf("marshal snapshot error: %v", err))
		return
	}

	tmp, err := os.CreateTemp(dir, filepath.Base(s.snapshotPath)+".tmp-*")
	if err != nil {
		s.logger.Error("balance", fmt.Sprintf("create temp snapshot error: %v", err))
		return
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(bs); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		s.logger.Error("balance", fmt.Sprintf("write temp snapshot error: %v", err))
		return
	}
	_ = tmp.Chmod(0644) // 与旧的直接写入保持一致的权限
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		s.logger.Error("balance", fmt.Sprintf("sync temp snapshot error: %v", err))
		return
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		s.logger.Error("balance", fmt.Sprintf("close temp snapshot error: %v", err))
		return
	}
	if err := os.Rename(tmpName, s.snapshotPath); err != nil {
		os.Remove(tmpName)
		s.logger.Error("balance", fmt.Sprintf("rename snapshot error: %v", err))
	}
}

// FetchAll 拉取配置中所有币安/TurboFlow 类型账号的余额。
func (s *Service) FetchAll(ctx context.Context) {
	cfg := s.cfgMgr.Get()

	// 当前配置里的账号集合：循环结束后用它清掉已经从配置中删除的账号，
	// 否则这些账号的最后一次余额会永远留在 s.balances 里，页面会出现幽灵
	// 账号并虚增总额和当日收益（直到重启）。
	active := make(map[string]struct{}, len(cfg.Tasks))
	for _, task := range cfg.Tasks {
		active[task.ID] = struct{}{}
	}

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
				s.setError(task.ID, "binance", fmt.Errorf("missing csrftoken or p20t"))
				continue
			}
			info, err := s.fetchBalance(ctx, csrf, p20t)
			if err != nil {
				s.logger.Error("balance", fmt.Sprintf("task=[%s] fetch balance error: %v", task.Name, err))
				s.setError(task.ID, "binance", err)
				continue
			}
			info.Platform = "binance"
			s.set(task.ID, info)

		case "turboflow":
			authorization := task.Auth["authorization"]
			if strings.TrimSpace(authorization) == "" {
				s.setError(task.ID, "turboflow", fmt.Errorf("missing authorization"))
				continue
			}
			info, err := s.fetchTurboFlowBalance(ctx, task)
			if err != nil {
				s.logger.Error("balance", fmt.Sprintf("task=[%s] fetch TurboFlow balance error: %v", task.Name, err))
				s.setError(task.ID, "turboflow", err)
				continue
			}
			s.set(task.ID, info)

		case "hibt":
			if strings.TrimSpace(task.Auth["authorization"]) == "" && strings.TrimSpace(task.Auth["x-auth-token"]) == "" {
				s.setError(task.ID, "hibt", fmt.Errorf("missing authorization or x-auth-token"))
				continue
			}
			info, err := s.fetchHIBTBalance(ctx, task)
			if err != nil {
				s.logger.Error("balance", fmt.Sprintf("task=[%s] fetch HiBT balance error: %v", task.Name, err))
				s.setError(task.ID, "hibt", err)
				continue
			}
			info.Platform = "hibt"
			s.set(task.ID, info)
		}
	}

	s.mu.Lock()
	for id := range s.balances {
		if _, ok := active[id]; !ok {
			delete(s.balances, id)
		}
	}
	s.mu.Unlock()
}

func (s *Service) set(taskID string, info Info) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.balances[taskID] = info
}

// setError 记录一次失败的拉取，但保留上一次成功的 Total/Futures/UpdatedAt，
// 只更新 Platform 和 Error：短暂的网络故障不应该让页面总额骤降，也不应该
// 悄悄改变该账号的派单权重。此前没有任何记录时，清空值即为零值。
func (s *Service) setError(taskID, platform string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	prev := s.balances[taskID]
	prev.Platform = platform
	prev.Error = err.Error()
	s.balances[taskID] = prev
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
	// 200 + code=000000 但 data 为空，与损坏/不完整的会话无法区分，按错误处理，
	// 让页面显示告警而不是一个“资产为 0”的假象。
	if len(payload.Data) == 0 {
		return Info{}, fmt.Errorf("empty balance payload")
	}

	var total, futures float64
	for _, d := range payload.Data {
		v, err := strconv.ParseFloat(strings.TrimSpace(d.Balance), 64)
		if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
			// 非数字余额（如 "1,234.56"）不能当成 0 悄悄计入总额，跳过并告警。
			reason := "not a finite number"
			if err != nil {
				reason = err.Error()
			}
			s.logger.Error("balance", fmt.Sprintf("skip unparseable balance row accountType=%s walletName=%s balance=%q: %s", d.AccountType, d.WalletName, d.Balance, reason))
			continue
		}
		total += v
		if d.AccountType == "FUTURE" {
			// 接口可能返回多个 FUTURE 行（钱包明细），合约账户总额是它们的和；
			// 只保留最后一行会漏计甚至得到 0。
			futures += v
		}
	}

	return Info{
		Total:     strconv.FormatFloat(total, 'f', 2, 64),
		Futures:   strconv.FormatFloat(futures, 'f', 2, 64),
		UpdatedAt: time.Now(),
	}, nil
}

// fetchHIBTBalance 拉取 HiBT（期权/合约）账户的 USDT 余额。
//
// 接口返回 {"code":0,"data":{"coin":"usdt","amount":"324.42",...}}，
// 这里把 data.amount 作为可用 USDT：既用于面板总额，也用于动态仓位
// （金额 = 可用额 × percent%）。
func (s *Service) fetchHIBTBalance(ctx context.Context, task config.TaskConfig) (Info, error) {
	// 下单 URL 里的 v 是签名；余额接口的 v 是毫秒时间戳（防缓存），
	// 每次请求重新生成，不能复用 auth["v"]。
	reqURL := fmt.Sprintf("%s?langCode=zh_CN&v=%d", hibtBalanceURL, time.Now().UnixMilli())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return Info{}, err
	}

	authorization := strings.TrimSpace(task.Auth["authorization"])
	authToken := strings.TrimSpace(task.Auth["x-auth-token"])
	req.Header.Set("accept", "application/json, text/plain, */*")
	req.Header.Set("client-type", "web")
	req.Header.Set("future_source", "1")
	req.Header.Set("hc-language", "zh_CN")
	req.Header.Set("hc-platform", "web")
	req.Header.Set("lang", "zh_CN")
	req.Header.Set("platform", "PC")
	req.Header.Set("user-agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/153.0.0.0 Safari/537.36")
	if authorization != "" {
		req.Header.Set("authorization", authorization)
	}
	if authToken != "" {
		req.Header.Set("x-auth-token", authToken)
	}

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
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			Coin   string `json:"coin"`
			Amount string `json:"amount"`
			Frozen string `json:"frozen"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return Info{}, fmt.Errorf("parse response failed: %w", err)
	}
	if resp.StatusCode != http.StatusOK || payload.Code != 0 {
		return Info{}, fmt.Errorf("unexpected status=%d code=%d msg=%s", resp.StatusCode, payload.Code, payload.Msg)
	}

	amount, ok := usableBalance(payload.Data.Amount)
	if !ok {
		return Info{}, fmt.Errorf("unparseable balance amount %q", payload.Data.Amount)
	}
	if amount < 0 {
		amount = 0
	}
	formatted := strconv.FormatFloat(amount, 'f', 2, 64)

	// 该平台只有一个 USDT 账户，Total 与 Futures 取同一个可用额：
	// GetFuturesBalances（动态仓位/权重）读 Futures，Summary（面板总额）读 Total。
	return Info{
		Total:     formatted,
		Futures:   formatted,
		UpdatedAt: time.Now(),
	}, nil
}

// usableBalance 解析余额字符串，无法解析或非有限值（NaN / ±Inf）时返回 false。
// ParseFloat("NaN") 会成功，而 NaN 参与比较永远为 false，若不显式拦截会污染
// 页面合计和派单权重。
func usableBalance(s string) (float64, bool) {
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, false
	}
	return v, true
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
