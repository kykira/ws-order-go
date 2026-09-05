package signals

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"time"

	"github.com/kykira/ws-order-go/internal/config"
	"github.com/kykira/ws-order-go/internal/logs"
	"github.com/kykira/ws-order-go/internal/order"
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

type Processor struct {
	cfg    *config.Manager
	logger *logs.Logger
	order  *order.Client

	mu            sync.Mutex
	seenSkip      map[string]int
	skipStartTime map[string]time.Time
}

func NewProcessor(cfg *config.Manager, logger *logs.Logger, orderClient *order.Client) *Processor {
	return &Processor{
		cfg:           cfg,
		logger:        logger,
		order:         orderClient,
		seenSkip:      make(map[string]int),
		skipStartTime: make(map[string]time.Time),
	}
}

// Stop is kept for API compatibility. There is no background slot loop anymore.
func (p *Processor) Stop() {}

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

	case "round-robin":
		p.dispatchRoundRobin(source, sig, matched, action, amount, unit, matchedRange)
		return nil

	default: // "all" or empty
		for _, task := range matched {
			p.executeTask(source, sig, task, action, amount, unit, sig.Period, matchedRange)
		}
	}

	return nil
}

// dispatchRandom picks one matched account randomly.
func (p *Processor) dispatchRandom(source string, sig Signal, matched []config.TaskConfig, action, amount, unit, matchedRange string) {
	if len(matched) == 0 {
		return
	}

	task := matched[rand.Intn(len(matched))]
	p.executeTaskWithFallback(source, sig, task, matched, action, amount, unit, sig.Period, matchedRange, nil)
	p.logger.Info("signal", fmt.Sprintf("dispatch=random picked account=[%s]", task.Name))
}

// dispatchRoundRobin picks the first matched account in config order.
func (p *Processor) dispatchRoundRobin(source string, sig Signal, matched []config.TaskConfig, action, amount, unit, matchedRange string) {
	if len(matched) == 0 {
		return
	}

	task := matched[0]
	p.logger.Info("signal", fmt.Sprintf("dispatch=round-robin account=[%s]", task.Name))
	p.executeTaskWithFallback(source, sig, task, matched, action, amount, unit, sig.Period, matchedRange, nil)
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
					return strings.TrimSpace(a.Amount)
				}
			}
		}
	}
	return ""
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
	task   config.TaskConfig
	amount string
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
		if allowed {
			matched = append(matched, groupAccountMatch{task: task, amount: strings.TrimSpace(binding.Amount)})
		}
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
		m := matched[0]
		p.logger.Info("signal", fmt.Sprintf("dispatch=group round-robin strategy=[%s] group=[%s] account=[%s]", sig.Strategy, group.Name, m.task.Name))
		p.executeTaskWithFallback(source, sig, m.task, matchedTasks, action, amountFor(m.task), unit, period, "", amountFor)

	default: // random
		m := matched[rand.Intn(len(matched))]
		p.logger.Info("signal", fmt.Sprintf("dispatch=group random strategy=[%s] group=[%s] account=[%s]", sig.Strategy, group.Name, m.task.Name))
		p.executeTaskWithFallback(source, sig, m.task, matchedTasks, action, amountFor(m.task), unit, period, "", amountFor)
	}
}

func (p *Processor) executeTask(source string, sig Signal, task config.TaskConfig, action, amount, unit, period, matchedRange string) {
	p.logger.Info("signal", fmt.Sprintf("source=%s orderID=%v account=[%s] action=%s symbol=%s amount=%s unit=%s period=%s timeRange=%s", source, sig.OrderID, task.Name, action, sig.Symbol, amount, unit, period, matchedRange))

	go func(t config.TaskConfig, req order.PlaceOrderRequest) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		if err := p.order.PlaceOrder(ctx, t, req); err != nil {
			p.logger.Error("signal", fmt.Sprintf("account=[%s] order error: %v", t.Name, err))
		}
	}(task, order.PlaceOrderRequest{
		Amount:     amount,
		Unit:       unit,
		Action:     action,
		Symbol:     sig.Symbol,
		TickerType: sig.TickerType,
		Period:     period,
	})
}

// isFallbackError reports whether an order error should trigger switching to
// another executable account.
func isFallbackError(err error) bool {
	return errors.Is(err, order.ErrRetryExhausted) || errors.Is(err, order.ErrAccountUnavailable)
}

// executeTaskWithFallback is used by single-account dispatch modes (random and
// round-robin). If the selected account is unavailable (order limit reached,
// login expired, banned, etc.), it automatically tries the other matched
// executable accounts.
func (p *Processor) executeTaskWithFallback(source string, sig Signal, primary config.TaskConfig, matched []config.TaskConfig, action, amount, unit, period, matchedRange string, amountFor func(config.TaskConfig) string) {
	p.logger.Info("signal", fmt.Sprintf("source=%s orderID=%v account=[%s] action=%s symbol=%s amount=%s unit=%s period=%s timeRange=%s", source, sig.OrderID, primary.Name, action, sig.Symbol, amount, unit, period, matchedRange))

	req := order.PlaceOrderRequest{
		Amount:     amount,
		Unit:       unit,
		Action:     action,
		Symbol:     sig.Symbol,
		TickerType: sig.TickerType,
		Period:     period,
	}

	go func(t config.TaskConfig, r order.PlaceOrderRequest) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		err := p.order.PlaceOrder(ctx, t, r)
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
func (p *Processor) tryFallbackOrder(source string, sig Signal, matched []config.TaskConfig, excludeID string, req order.PlaceOrderRequest, amountFor func(config.TaskConfig) string) {
	for _, task := range matched {
		if task.ID == excludeID {
			continue
		}

		amt := req.Amount
		if amountFor != nil {
			amt = amountFor(task)
		}
		r := req
		r.Amount = amt

		p.logger.Info("signal", fmt.Sprintf("source=%s orderID=%v account=[%s] fallback after order limit symbol=%s amount=%s unit=%s", source, sig.OrderID, task.Name, sig.Symbol, amt, req.Unit))

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err := p.order.PlaceOrder(ctx, task, r)
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

	p.logger.Info("signal", "no available fallback account after order limit")
}
