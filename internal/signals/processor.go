package signals

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kykira/ws-order-go/internal/config"
	"github.com/kykira/ws-order-go/internal/logs"
	"github.com/kykira/ws-order-go/internal/order"
	"github.com/kykira/ws-order-go/internal/tfrealtime"
)

type Signal struct {
	Type       string `json:"type"`
	OrderID    int64  `json:"orderID"`
	Action     string `json:"action"`
	Symbol     string `json:"symbol"`
	TickerType string `json:"tickerType,omitempty"`
	Timestamp  string `json:"timestamp"`

	Amount    string  `json:"amount,omitempty"`
	Unit      string  `json:"unit,omitempty"`
	Direction string  `json:"direction,omitempty"`
	Proba     float64 `json:"proba,omitempty"` // 0=未传，>0=模型置信度

	Strategy string `json:"strategy,omitempty"` // 策略 ID 或名称
	Period   string `json:"period,omitempty"`   // 时间周期，如 30m
}

// BalanceProvider supplies each account's futures balance for weighted random
// dispatch. A nil provider means all accounts keep equal weight.
type BalanceProvider interface {
	GetFuturesBalances() map[string]float64
}

// OddsProvider supplies the latest TurboFlow evt_cfg snapshot for signal-time
// odds waiting.
type OddsProvider interface {
	GetPair(symbol string) (tfrealtime.Pair, bool)
	Updates() <-chan struct{}
}

type Processor struct {
	cfg             *config.Manager
	logger          *logs.Logger
	order           *order.Client
	balanceProvider BalanceProvider
	oddsProvider    OddsProvider

	mu            sync.Mutex
	seenSkip      map[string]int
	skipStartTime map[string]time.Time
	// rrCursor 是顺序轮询的游标（key = group:<id> 或 legacy）。
	rrCursor map[string]int

	// orders counts in-flight order goroutines so shutdown can wait for them.
	// An atomic counter is used instead of sync.WaitGroup because a connection
	// that is still winding down may call Handle (and therefore Add) while
	// Stop is already waiting, which is illegal for WaitGroup reuse.
	orders atomic.Int64
}

func NewProcessor(cfg *config.Manager, logger *logs.Logger, orderClient *order.Client, balanceProviders ...BalanceProvider) *Processor {
	var bp BalanceProvider
	if len(balanceProviders) > 0 {
		bp = balanceProviders[0]
	}
	return &Processor{
		cfg:             cfg,
		logger:          logger,
		order:           orderClient,
		balanceProvider: bp,
		seenSkip:        make(map[string]int),
		skipStartTime:   make(map[string]time.Time),
		rrCursor:        make(map[string]int),
	}
}

// SetOddsProvider injects the TurboFlow realtime odds snapshot source.
func (p *Processor) SetOddsProvider(provider OddsProvider) {
	p.oddsProvider = provider
}

// weightsForTasks returns a random weight for every task. Binance accounts
// with a lower futures balance get a higher weight (inverse balance weight);
// non-Binance accounts and unknown balances keep weight 1.
func (p *Processor) weightsForTasks(tasks []config.TaskConfig) []float64 {
	weights := make([]float64, len(tasks))
	for i := range weights {
		weights[i] = 1
	}

	if p.balanceProvider == nil {
		return weights
	}
	balances := p.balanceProvider.GetFuturesBalances()

	// Compute the average balance only across matched accounts with valid
	// balances, then use it as the target for inverse-balance weighting.
	target, valid := 0.0, 0
	for _, task := range tasks {
		if bal, ok := balances[task.ID]; ok && bal > 0 {
			target += bal
			valid++
		}
	}
	if valid == 0 || target <= 0 {
		return weights
	}
	target /= float64(valid)

	// weight = 1 + 2 * clamp((target-balance)/target, 0, 1)
	// so the strongest/weakest weight ratio is at most 3:1.
	for i, task := range tasks {
		bal, ok := balances[task.ID]
		if !ok || bal <= 0 {
			continue
		}
		ratio := (target - bal) / target
		if ratio < 0 {
			ratio = 0
		}
		if ratio > 1 {
			ratio = 1
		}
		weights[i] = 1 + 2*ratio
	}
	return weights
}

func weightedIndex(weights []float64) int {
	if len(weights) == 0 {
		return -1
	}
	total := 0.0
	for _, w := range weights {
		if w > 0 {
			total += w
		}
	}
	if total <= 0 {
		return rand.Intn(len(weights))
	}
	r := rand.Float64() * total
	for i, w := range weights {
		if w <= 0 {
			continue
		}
		r -= w
		if r <= 0 {
			return i
		}
	}
	return len(weights) - 1
}

func (p *Processor) pickWeightedTask(matched []config.TaskConfig) config.TaskConfig {
	return matched[weightedIndex(p.weightsForTasks(matched))]
}

func (p *Processor) pickWeightedGroupAccount(matched []groupAccountMatch) groupAccountMatch {
	tasks := make([]config.TaskConfig, len(matched))
	for i, m := range matched {
		tasks[i] = m.task
	}
	return matched[weightedIndex(p.weightsForTasks(tasks))]
}

// Stop waits (bounded) for in-flight order goroutines so a graceful shutdown
// does not cut an order request in half. There is no background slot loop
// anymore; it is safe to call more than once.
func (p *Processor) Stop() {
	p.WaitOrders(5 * time.Second)
}

// WaitOrders blocks until in-flight order goroutines finish or the timeout
// expires, whichever comes first.
func (p *Processor) WaitOrders(timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for p.orders.Load() > 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
}

// Handle 处理一条信号。applySkip 表示是否应用 skipSignals 逻辑。
func (p *Processor) Handle(source string, sig Signal, applySkip bool) error {
	cfg := p.cfg.Get()

	action := strings.ToLower(strings.TrimSpace(sig.Action))
	if action == "" {
		p.logger.Error("signal", "empty action, ignore")
		return nil
	}

	amount := strings.TrimSpace(sig.Amount)
	unit := strings.TrimSpace(sig.Unit)

	if source == "upstream" && action == "test" {
		return nil
	}

	if len(cfg.Tasks) == 0 {
		p.logger.Error("signal", "no tasks configured, ignore")
		return nil
	}

	if sig.Strategy != "" && len(cfg.Strategies) > 0 {
		p.handleStrategySignal(source, sig, cfg, action, amount, unit, applySkip)
		return nil
	}

	// Collect matching accounts (legacy flat-task mode)
	var matched []config.TaskConfig
	var matchedRange string
	for _, task := range cfg.Tasks {
		if task.Type == "raw" {
			continue // raw 任务(如 webhook)只在 strategy group 显式绑定, 不进 legacy flat-task
		}
		allowed, rangeDesc := p.taskMatches(sig, action, task, applySkip)
		if allowed {
			matched = append(matched, task)
			matchedRange = rangeDesc
		}
	}

	if len(matched) == 0 {
		return nil
	}

	// Legacy mode: upstream signals may not carry strategy/amount. Fall back to
	// the amount configured in the strategy-group binding for this account.
	if strings.TrimSpace(amount) == "" && sig.Strategy == "" {
		for _, task := range matched {
			if amt := p.amountForTask(cfg, task.ID); amt != "" {
				amount = amt
				break
			}
		}
	}

	switch cfg.Dispatch {
	case "random":
		p.dispatchRandom(source, sig, matched, action, amount, unit, matchedRange)
		return nil

	case "weighted":
		p.dispatchWeighted(source, sig, matched, action, amount, unit, matchedRange)
		return nil

	case "round-robin":
		p.dispatchRoundRobin(source, sig, matched, action, amount, unit, matchedRange)
		return nil

	default: // "all" or empty
		for _, task := range matched {
			amt := amount
			// Each account may carry its own configured amount, so resolve it
			// per account instead of reusing the first match for everyone.
			if strings.TrimSpace(sig.Amount) == "" {
				if v := p.amountForTask(cfg, task.ID); v != "" {
					amt = v
				}
			}
			p.executeTask(source, sig, task, action, amt, unit, sig.Period, matchedRange)
		}
	}

	return nil
}

// dispatchRandom picks one matched account with equal probability.
func (p *Processor) dispatchRandom(source string, sig Signal, matched []config.TaskConfig, action, amount, unit, matchedRange string) {
	if len(matched) == 0 {
		return
	}

	task := matched[rand.Intn(len(matched))]
	p.executeTaskWithFallback(source, sig, task, matched, action, amount, unit, sig.Period, matchedRange, nil)
	p.logger.Info("signal", fmt.Sprintf("dispatch=random picked account=[%s]", task.Name))
}

// dispatchWeighted picks one matched account randomly, weighted by Binance
// account balance (lower balance => higher weight, capped at 3:1).
func (p *Processor) dispatchWeighted(source string, sig Signal, matched []config.TaskConfig, action, amount, unit, matchedRange string) {
	if len(matched) == 0 {
		return
	}

	task := p.pickWeightedTask(matched)
	p.executeTaskWithFallback(source, sig, task, matched, action, amount, unit, sig.Period, matchedRange, nil)
	p.logger.Info("signal", fmt.Sprintf("dispatch=weighted picked account=[%s]", task.Name))
}

// dispatchRoundRobin rotates through the matched accounts in config order
// (legacy flat mode has no per-group priorities).
func (p *Processor) dispatchRoundRobin(source string, sig Signal, matched []config.TaskConfig, action, amount, unit, matchedRange string) {
	if len(matched) == 0 {
		return
	}

	idx := p.nextRoundRobinIndex("legacy", len(matched))
	task := matched[idx]
	p.logger.Info("signal", fmt.Sprintf("dispatch=round-robin account=[%s] index=%d/%d", task.Name, idx+1, len(matched)))
	p.executeTaskWithFallback(source, sig, task, matched, action, amount, unit, sig.Period, matchedRange, nil)
}

// nextRoundRobinIndex returns the next cursor position for key and advances it.
// The cursor is per group (and one for legacy dispatch), so independent groups
// rotate independently and concurrently.
func (p *Processor) nextRoundRobinIndex(key string, n int) int {
	if n <= 1 {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.rrCursor == nil {
		p.rrCursor = make(map[string]int)
	}
	idx := p.rrCursor[key] % n
	if idx < 0 {
		idx += n
	}
	p.rrCursor[key] = (idx + 1) % n
	return idx
}

// sortByPriority orders matches by descending priority, keeping the configured
// order for equal priorities so fallback also tries the preferred account first.
func sortByPriority(matched []groupAccountMatch) {
	sort.SliceStable(matched, func(i, j int) bool {
		return matched[i].priority > matched[j].priority
	})
}

// pickRoundRobinGroupAccount rotates through the accounts sharing the highest
// priority. Lower-priority accounts are never picked here: they are only used as
// fallback when the preferred ones fail (order limit, expired login, ...).
func (p *Processor) pickRoundRobinGroupAccount(groupID string, matched []groupAccountMatch) groupAccountMatch {
	top := matched[0].priority
	for _, m := range matched[1:] {
		if m.priority > top {
			top = m.priority
		}
	}

	tier := make([]groupAccountMatch, 0, len(matched))
	for _, m := range matched {
		if m.priority == top {
			tier = append(tier, m)
		}
	}

	idx := p.nextRoundRobinIndex("group:"+groupID, len(tier))
	return tier[idx]
}

func (p *Processor) checkSkip(task config.TaskConfig) bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	now := time.Now()
	if start, ok := p.skipStartTime[task.ID]; ok && now.Sub(start) > 30*time.Minute {
		p.seenSkip[task.ID] = 0
		delete(p.skipStartTime, task.ID)
		p.logger.Info("signal", fmt.Sprintf("account=[%s] skip counter reset after 30m", task.Name))
	}

	if p.seenSkip[task.ID] < task.SkipSignals {
		if p.seenSkip[task.ID] == 0 {
			p.skipStartTime[task.ID] = now
		}
		p.seenSkip[task.ID]++
		p.logger.Info("signal", fmt.Sprintf("skip %d/%d account=[%s]", p.seenSkip[task.ID], task.SkipSignals, task.Name))
		return false
	}
	return true
}

func (p *Processor) taskMatches(sig Signal, action string, task config.TaskConfig, applySkip bool) (bool, string) {
	if !task.Enabled {
		return false, ""
	}

	currentTime := time.Now()
	allowedNow, rangeDesc, err := config.IsTimeAllowed(task.TimeRanges, currentTime)
	if err != nil {
		p.logger.Error("signal", fmt.Sprintf("account=[%s] invalid time ranges: %v", task.Name, err))
		return false, ""
	}
	if !allowedNow {
		p.logger.Info("signal", fmt.Sprintf("account=[%s] ignored by time ranges current=%s ranges=%s", task.Name, currentTime.Format("15:04"), config.FormatTimeRanges(task.TimeRanges)))
		return false, rangeDesc
	}

	if strings.TrimSpace(task.AllowedSymbols) != "" {
		allowed := false
		for _, s := range strings.Split(task.AllowedSymbols, ",") {
			if strings.TrimSpace(s) != "" && strings.EqualFold(strings.TrimSpace(s), sig.Symbol) {
				allowed = true
				break
			}
		}
		if !allowed {
			p.logger.Info("signal", fmt.Sprintf("account=[%s] skipped (symbol %s not in allowed list)", task.Name, sig.Symbol))
			return false, rangeDesc
		}
	}

	if applySkip && task.SkipSignals > 0 {
		if !p.checkSkip(task) {
			return false, rangeDesc
		}
	}

	return true, rangeDesc
}

func (p *Processor) amountForTask(cfg config.Config, accountID string) string {
	for _, st := range cfg.Strategies {
		for _, g := range st.Groups {
			for _, a := range g.Accounts {
				if a.AccountID == accountID {
					if amt, ok := p.dynamicAmount(a); ok {
						return amt
					}
					return ""
				}
			}
		}
	}
	return ""
}

// formatAmount renders an amount with the single decimal place the dynamic
// sizing feature promises.
func formatAmount(v float64) string {
	return strconv.FormatFloat(math.Round(v*10)/10, 'f', 1, 64)
}

// dynamicAmount resolves the order amount for one group binding.
//
//	percent unset → the static amount (unchanged legacy behaviour)
//	percent set   → balance × percent%, floored at minAmount and capped at
//	                maxAmount. When the percentage lands below minAmount the
//	                configured minimum position is used instead.
//
// ok is false when dynamic sizing is configured but no safe amount can be
// determined. The caller must then SKIP the account: emitting a zero or empty
// amount would submit an order with an invalid size.
func (p *Processor) dynamicAmount(a config.StrategyGroupAccountConfig) (string, bool) {
	if a.Percent <= 0 {
		return strings.TrimSpace(a.Amount), true
	}

	bal := 0.0
	known := false
	if p.balanceProvider != nil {
		bal, known = p.balanceProvider.GetFuturesBalances()[a.AccountID]
	}

	// An unknown balance happens before the first fetch completes, after a
	// failed fetch, and for account types the balance service does not cover at
	// all. Never size an order from it; fall back to the floor, then to the
	// static amount, and only then give up.
	if !known || bal <= 0 {
		if a.MinAmount > 0 {
			p.logger.Info("signal", fmt.Sprintf("amount: account=[%s] balance unavailable, using minAmount=%.1f", a.AccountID, a.MinAmount))
			return formatAmount(a.MinAmount), true
		}
		if amt := strings.TrimSpace(a.Amount); amt != "" {
			p.logger.Info("signal", fmt.Sprintf("amount: account=[%s] balance unavailable, falling back to amount=%s", a.AccountID, amt))
			return amt, true
		}
		p.logger.Error("signal", fmt.Sprintf("amount: account=[%s] balance unavailable and no minAmount/amount configured, skipping order", a.AccountID))
		return "", false
	}

	amt := bal * a.Percent / 100.0
	if a.MinAmount > 0 && amt < a.MinAmount {
		// The configured minimum position wins when the percentage is small.
		amt = a.MinAmount
	}
	if a.MaxAmount > 0 && amt > a.MaxAmount {
		amt = a.MaxAmount
	}
	if amt <= 0 {
		p.logger.Error("signal", fmt.Sprintf("amount: account=[%s] computed amount is not positive (balance=%.2f percent=%.2f%%), skipping order", a.AccountID, bal, a.Percent))
		return "", false
	}
	p.logger.Info("signal", fmt.Sprintf("amount: account=[%s] balance=%.2f percent=%.2f%% => %.1f",
		a.AccountID, bal, a.Percent, amt))
	return formatAmount(amt), true
}

func (p *Processor) accountByID(tasks []config.TaskConfig, id string) (config.TaskConfig, bool) {
	for _, t := range tasks {
		if t.ID == id {
			return t, true
		}
	}
	return config.TaskConfig{}, false
}

type groupAccountMatch struct {
	task     config.TaskConfig
	amount   string
	priority int
}

func (p *Processor) groupMatchedAccounts(sig Signal, action string, tasks []config.TaskConfig, group config.StrategyGroupConfig, applySkip bool) []groupAccountMatch {
	var matched []groupAccountMatch

	bindings := group.Accounts
	if len(bindings) == 0 && len(group.AccountIDs) > 0 {
		for _, id := range group.AccountIDs {
			bindings = append(bindings, config.StrategyGroupAccountConfig{AccountID: id})
		}
	}

	for _, binding := range bindings {
		task, ok := p.accountByID(tasks, binding.AccountID)
		if !ok {
			p.logger.Info("signal", fmt.Sprintf("group=[%s] account id=[%s] not found, skip", group.Name, binding.AccountID))
			continue
		}
		allowed, _ := p.taskMatches(sig, action, task, applySkip)
		if !allowed {
			continue
		}
		amount, ok := p.dynamicAmount(binding)
		if !ok {
			p.logger.Error("signal", fmt.Sprintf("group=[%s] account=[%s] amount unavailable, skip", group.Name, task.Name))
			continue
		}
		matched = append(matched, groupAccountMatch{task: task, amount: amount, priority: binding.Priority}) // ★ 动态金额/优先级
	}
	return matched
}

func (p *Processor) handleStrategySignal(source string, sig Signal, cfg config.Config, action, amount, unit string, applySkip bool) {
	for _, st := range cfg.Strategies {
		if !st.Enabled {
			continue
		}
		if st.ID != sig.Strategy && st.Name != sig.Strategy {
			continue
		}

		for _, group := range st.Groups {
			if !group.Enabled {
				continue
			}
			p.dispatchGroup(source, sig, cfg.Tasks, group, action, amount, unit, sig.Period, applySkip)
		}
	}
}

func (p *Processor) dispatchGroup(source string, sig Signal, tasks []config.TaskConfig, group config.StrategyGroupConfig, action, signalAmount, unit, period string, applySkip bool) {
	matched := p.groupMatchedAccounts(sig, action, tasks, group, applySkip)
	if len(matched) == 0 {
		p.logger.Info("signal", fmt.Sprintf("strategy=[%s] group=[%s] no matched account, skip", sig.Strategy, group.Name))
		return
	}

	// 高优先级在前；同优先级保持配置顺序，兜底也按这个顺序尝试。
	sortByPriority(matched)

	amountFor := func(t config.TaskConfig) string {
		for _, m := range matched {
			if m.task.ID == t.ID {
				if strings.TrimSpace(m.amount) != "" {
					return m.amount
				}
				if strings.TrimSpace(signalAmount) != "" {
					return signalAmount
				}
				return ""
			}
		}
		return signalAmount
	}

	matchedTasks := make([]config.TaskConfig, 0, len(matched))
	for _, m := range matched {
		matchedTasks = append(matchedTasks, m.task)
	}

	switch group.Dispatch {
	case "all":
		for _, m := range matched {
			p.executeTask(source, sig, m.task, action, amountFor(m.task), unit, period, "")
		}

	case "round-robin":
		m := p.pickRoundRobinGroupAccount(group.ID, matched)
		p.logger.Info("signal", fmt.Sprintf("dispatch=group round-robin strategy=[%s] group=[%s] account=[%s] priority=%d", sig.Strategy, group.Name, m.task.Name, m.priority))
		p.executeTaskWithFallback(source, sig, m.task, matchedTasks, action, amountFor(m.task), unit, period, "", amountFor)

	case "weighted":
		m := p.pickWeightedGroupAccount(matched)
		p.logger.Info("signal", fmt.Sprintf("dispatch=group weighted strategy=[%s] group=[%s] account=[%s]", sig.Strategy, group.Name, m.task.Name))
		p.executeTaskWithFallback(source, sig, m.task, matchedTasks, action, amountFor(m.task), unit, period, "", amountFor)

	default: // random
		m := matched[rand.Intn(len(matched))]
		p.logger.Info("signal", fmt.Sprintf("dispatch=group random strategy=[%s] group=[%s] account=[%s]", sig.Strategy, group.Name, m.task.Name))
		p.executeTaskWithFallback(source, sig, m.task, matchedTasks, action, amountFor(m.task), unit, period, "", amountFor)
	}
}

const (
	tfOddsWaitDuration = 50 * time.Second
)

func parseOdds(s string) float64 {
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || v <= 0 {
		return 0
	}
	return v
}

func (p *Processor) minOdds(task config.TaskConfig) float64 {
	if task.Type != "turboflow" {
		return 0
	}
	return parseOdds(task.MinOdds)
}

func (p *Processor) orderTimeout(task config.TaskConfig) time.Duration {
	timeout := 60 * time.Second
	if p.minOdds(task) > 0 {
		timeout = tfOddsWaitDuration + 60*time.Second
	}
	// Binance accounts may wait a random delay before sending the order.
	return timeout + order.MaxRandomDelay(task)
}

func periodSeconds(period string) int {
	switch strings.ToLower(strings.TrimSpace(period)) {
	case "30s":
		return 30
	case "45s":
		return 45
	case "1m":
		return 60
	case "3m":
		return 180
	case "5m":
		return 300
	case "10m":
		return 600
	case "15m":
		return 900
	case "30m":
		return 1800
	case "1h":
		return 3600
	case "2h":
		return 7200
	case "4h":
		return 14400
	default:
		return 0
	}
}

func (p *Processor) currentDirectionOdds(req order.PlaceOrderRequest) float64 {
	if p.oddsProvider == nil {
		return 0
	}
	pair, ok := p.oddsProvider.GetPair(req.Symbol)
	if !ok {
		return 0
	}
	secs := periodSeconds(req.Period)
	if secs == 0 {
		return 0
	}
	for _, opt := range pair.Options {
		if opt.Duration != secs {
			continue
		}
		if strings.EqualFold(req.Action, "buy") {
			return parseOdds(opt.UpRate)
		}
		if strings.EqualFold(req.Action, "sell") {
			return parseOdds(opt.DownRate)
		}
	}
	return 0
}

// placeOrderWithOdds waits up to 50s for the configured minimum odds on a
// TurboFlow account, then places the order exactly once: immediately when the
// threshold is reached, or when the 50s window expires.
func (p *Processor) placeOrderWithOdds(ctx context.Context, task config.TaskConfig, req order.PlaceOrderRequest) error {
	threshold := p.minOdds(task)
	if threshold <= 0 || p.oddsProvider == nil {
		return p.order.PlaceOrder(ctx, task, req)
	}

	place := func() error {
		return p.order.PlaceOrder(ctx, task, req)
	}

	if rate := p.currentDirectionOdds(req); rate >= threshold {
		p.logger.Info("order", fmt.Sprintf("account=[%s] odds reached immediately action=%s symbol=%s period=%s rate=%.4f threshold=%.4f", task.Name, req.Action, req.Symbol, req.Period, rate, threshold))
		return place()
	}

	p.logger.Info("order", fmt.Sprintf("account=[%s] wait odds up to %s action=%s symbol=%s period=%s threshold=%.4f", task.Name, tfOddsWaitDuration, req.Action, req.Symbol, req.Period, threshold))
	timer := time.NewTimer(tfOddsWaitDuration)
	defer timer.Stop()

	for {
		// Subscribe BEFORE reading the state. Updates() hands out a channel
		// that is closed and replaced on every push, so checking the odds first
		// would miss an update landing in the gap and then block on the fresh,
		// still-open channel until the timeout below.
		updates := p.oddsProvider.Updates()
		if rate := p.currentDirectionOdds(req); rate >= threshold {
			p.logger.Info("order", fmt.Sprintf("account=[%s] odds reached action=%s symbol=%s period=%s rate=%.4f threshold=%.4f", task.Name, req.Action, req.Symbol, req.Period, rate, threshold))
			return place()
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-updates:
			continue
		case <-timer.C:
			if rate := p.currentDirectionOdds(req); rate >= threshold {
				p.logger.Info("order", fmt.Sprintf("account=[%s] odds reached at timeout action=%s symbol=%s period=%s rate=%.4f threshold=%.4f", task.Name, req.Action, req.Symbol, req.Period, rate, threshold))
			} else {
				p.logger.Info("order", fmt.Sprintf("account=[%s] odds wait timeout after %s, placing fallback order action=%s symbol=%s period=%s threshold=%.4f", task.Name, tfOddsWaitDuration, req.Action, req.Symbol, req.Period, threshold))
			}
			return place()
		}
	}
}

func (p *Processor) executeTask(source string, sig Signal, task config.TaskConfig, action, amount, unit, period, matchedRange string) {
	p.logger.Info("signal", fmt.Sprintf("source=%s orderID=%v strategy=[%s] account=[%s] action=%s symbol=%s amount=%s unit=%s period=%s timeRange=%s", source, sig.OrderID, sig.Strategy, task.Name, action, sig.Symbol, amount, unit, period, matchedRange))

	p.orders.Add(1)
	go func(t config.TaskConfig, req order.PlaceOrderRequest) {
		defer p.orders.Add(-1)
		ctx, cancel := context.WithTimeout(context.Background(), p.orderTimeout(t))
		defer cancel()

		if err := p.placeOrderWithOdds(ctx, t, req); err != nil {
			p.logger.Error("signal", fmt.Sprintf("account=[%s] order error: %v", t.Name, err))
		}
	}(task, order.PlaceOrderRequest{
		Amount:     amount,
		Unit:       unit,
		Action:     action,
		Symbol:     sig.Symbol,
		TickerType: sig.TickerType,
		Period:     period,
		Strategy:   sig.Strategy,
	})
}

// isFallbackError reports whether an order error should trigger switching to
// another executable account.
func isFallbackError(err error) bool {
	return errors.Is(err, order.ErrOrderLimitReached) ||
		errors.Is(err, order.ErrRetryExhausted) ||
		errors.Is(err, order.ErrAccountUnavailable)
}

// executeTaskWithFallback is used by single-account dispatch modes (random and
// round-robin). If the selected account is unavailable (order limit reached,
// login expired, banned, etc.), it automatically tries the other matched
// executable accounts.
func (p *Processor) executeTaskWithFallback(source string, sig Signal, primary config.TaskConfig, matched []config.TaskConfig, action, amount, unit, period, matchedRange string, amountFor func(config.TaskConfig) string) {
	p.logger.Info("signal", fmt.Sprintf("source=%s orderID=%v strategy=[%s] account=[%s] action=%s symbol=%s amount=%s unit=%s period=%s timeRange=%s", source, sig.OrderID, sig.Strategy, primary.Name, action, sig.Symbol, amount, unit, period, matchedRange))

	req := order.PlaceOrderRequest{
		Amount:     amount,
		Unit:       unit,
		Action:     action,
		Symbol:     sig.Symbol,
		TickerType: sig.TickerType,
		Period:     period,
		Strategy:   sig.Strategy,
	}

	p.orders.Add(1)
	go func(t config.TaskConfig, r order.PlaceOrderRequest) {
		defer p.orders.Add(-1)
		ctx, cancel := context.WithTimeout(context.Background(), p.orderTimeout(t))
		defer cancel()

		err := p.placeOrderWithOdds(ctx, t, r)
		if err == nil {
			return
		}

		if isFallbackError(err) {
			p.logger.Error("signal", fmt.Sprintf("account=[%s] order error: %v, trying another executable account", t.Name, err))
			p.tryFallbackOrder(source, sig, matched, t.ID, r, amountFor)
			return
		}

		p.logger.Error("signal", fmt.Sprintf("account=[%s] order error: %v", t.Name, err))
	}(primary, req)
}

// tryFallbackOrder attempts the remaining matched accounts in config order.
const maxFallbackOrderAttempts = 5

// tryFallbackOrder attempts the remaining matched accounts in a loop. It starts
// with the account after the one that just failed, and cycles through the
// matched accounts until one succeeds or maxFallbackOrderAttempts is reached.
func (p *Processor) tryFallbackOrder(source string, sig Signal, matched []config.TaskConfig, excludeID string, req order.PlaceOrderRequest, amountFor func(config.TaskConfig) string) {
	if len(matched) == 0 {
		return
	}

	start := 0
	for i, task := range matched {
		if task.ID == excludeID {
			start = i + 1
			break
		}
	}
	if start >= len(matched) {
		start = 0
	}

	for attempt := 0; attempt < maxFallbackOrderAttempts; attempt++ {
		task := matched[(start+attempt)%len(matched)]

		amt := req.Amount
		if amountFor != nil {
			amt = amountFor(task)
		}
		r := req
		r.Amount = amt
		r.Source = "fallback"

		p.logger.Info("signal", fmt.Sprintf("source=%s orderID=%v strategy=[%s] account=[%s] fallback attempt=%d/%d after order limit symbol=%s amount=%s unit=%s", source, sig.OrderID, sig.Strategy, task.Name, attempt+1, maxFallbackOrderAttempts, sig.Symbol, amt, req.Unit))

		ctx, cancel := context.WithTimeout(context.Background(), p.orderTimeout(task))
		err := p.placeOrderWithOdds(ctx, task, r)
		cancel()
		if err == nil {
			p.logger.Info("signal", fmt.Sprintf("dispatch fallback account=[%s] success", task.Name))
			return
		}

		if isFallbackError(err) {
			p.logger.Error("signal", fmt.Sprintf("account=[%s] fallback order error: %v, continue to next account", task.Name, err))
			continue
		}

		p.logger.Error("signal", fmt.Sprintf("account=[%s] fallback order error: %v", task.Name, err))
		return
	}

	p.logger.Info("signal", fmt.Sprintf("no available fallback account after %d attempts", maxFallbackOrderAttempts))
}
