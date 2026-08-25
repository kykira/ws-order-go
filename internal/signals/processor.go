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
}

type Processor struct {
	cfg    *config.Manager
	logger *logs.Logger
	order  *order.Client

	mu            sync.Mutex
	seenSkip      map[string]int
	skipStartTime map[string]time.Time
	orderSlots    map[string][]time.Time // slotGroupKey → 开仓时间列表，每个独立30min TTL
	stopCh        chan struct{}
}

func NewProcessor(cfg *config.Manager, logger *logs.Logger, orderClient *order.Client) *Processor {
	p := &Processor{
		cfg:           cfg,
		logger:        logger,
		order:         orderClient,
		seenSkip:      make(map[string]int),
		skipStartTime: make(map[string]time.Time),
		orderSlots:    make(map[string][]time.Time),
		stopCh:        make(chan struct{}),
	}
	go p.slotExpiryLoop()
	return p
}

// Stop shuts down the background expiry loop.
func (p *Processor) Stop() {
	close(p.stopCh)
}

// slotExpiryLoop ticks every minute to release expired slots.
func (p *Processor) slotExpiryLoop() {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-p.stopCh:
			return
		case <-ticker.C:
			p.expireSlots()
		}
	}
}

const maxOrdersPerWindow = 5
const orderSlotTTL = 29*time.Minute + 30*time.Second

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

	// Collect matching accounts
	var matched []config.TaskConfig
	var matchedRange string

	for _, task := range cfg.Tasks {
		if !task.Enabled {
			continue
		}

		currentTime := time.Now()
		allowedNow, rangeDesc, err := config.IsTimeAllowed(task.TimeRanges, currentTime)
		if err != nil {
			p.logger.Error("signal", fmt.Sprintf("account=[%s] invalid time ranges: %v", task.Name, err))
			continue
		}
		if !allowedNow {
			p.logger.Info("signal", fmt.Sprintf("account=[%s] ignored by time ranges current=%s ranges=%s", task.Name, currentTime.Format("15:04"), config.FormatTimeRanges(task.TimeRanges)))
			continue
		}
		matchedRange = rangeDesc

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
				continue
			}
		}

		if applySkip && task.SkipSignals > 0 {
			if !p.checkSkip(task) {
				continue
			}
		}

		// Proba filter
		if sig.Proba > 0 && task.MinProba > 0 {
			switch action {
			case "buy":
				if sig.Proba < task.MinProba {
					p.logger.Info("signal", fmt.Sprintf("account=[%s] proba=%.4f < %.2f, skip buy", task.Name, sig.Proba, task.MinProba))
					continue
				}
			case "sell":
				if sig.Proba > (1 - task.MinProba) {
					p.logger.Info("signal", fmt.Sprintf("account=[%s] proba=%.4f > %.2f, skip sell", task.Name, sig.Proba, 1-task.MinProba))
					continue
				}
			}
		}

		matched = append(matched, task)
	}

	if len(matched) == 0 {
		return nil
	}

	switch cfg.Dispatch {
	case "random":
		p.dispatchRandom(source, sig, matched, action, amount, unit, matchedRange)
		return nil

	case "round-robin":
		p.dispatchRoundRobin(source, sig, matched, action, amount, unit, matchedRange)
		return nil

	default: // "all" or empty
		p.mu.Lock()
		for _, task := range matched {
			if p.tryClaimSlot(task.SlotGroupKey(), task.Name) {
				p.mu.Unlock()
				p.executeTask(source, sig, task, action, amount, unit, matchedRange)
				p.mu.Lock()
			}
		}
		p.mu.Unlock()
	}

	return nil
}

// expireSlots removes slots older than orderSlotTTL for all accounts.
func (p *Processor) expireSlots() {
	p.mu.Lock()
	defer p.mu.Unlock()

	now := time.Now()
	for slotKey, slots := range p.orderSlots {
		cutoff := now.Add(-orderSlotTTL)
		alive := make([]time.Time, 0, len(slots))
		expired := 0
		for _, t := range slots {
			if t.Before(cutoff) {
				expired++
			} else {
				alive = append(alive, t)
			}
		}
		if expired > 0 {
			p.orderSlots[slotKey] = alive
			p.logger.Info("signal", fmt.Sprintf("slotGroup=[%s] expire -%d slots (29m30s TTL) %d→%d", slotKey, expired, len(slots), len(alive)))
		}
	}
}

// tryClaimSlot tries to claim one slot for a slot group. Returns true if
// claimed, false if full. Must be called with p.mu held.
func (p *Processor) tryClaimSlot(slotKey, taskName string) bool {
	slots := p.orderSlots[slotKey]
	if len(slots) >= maxOrdersPerWindow {
		p.logger.Info("signal", fmt.Sprintf("account=[%s] slotGroup=[%s] slots full %d/%d, skipping", taskName, slotKey, len(slots), maxOrdersPerWindow))
		return false
	}

	now := time.Now()
	p.orderSlots[slotKey] = append(slots, now)
	p.logger.Info("signal", fmt.Sprintf("account=[%s] slotGroup=[%s] slot %d/%d", taskName, slotKey, len(p.orderSlots[slotKey]), maxOrdersPerWindow))
	return true
}

// dispatchRandom picks one matched account, respecting slot limits.
func (p *Processor) dispatchRandom(source string, sig Signal, matched []config.TaskConfig, action, amount, unit, matchedRange string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	perm := rand.Perm(len(matched))
	for _, i := range perm {
		task := matched[i]
		if p.tryClaimSlot(task.SlotGroupKey(), task.Name) {
			p.mu.Unlock()
			p.executeTaskWithFallback(source, sig, task, matched, action, amount, unit, matchedRange)
			p.mu.Lock()
			p.logger.Info("signal", fmt.Sprintf("dispatch=random picked account=[%s]", task.Name))
			return
		}
	}
	p.logger.Info("signal", fmt.Sprintf("dispatch=random all %d accounts full, signal dropped", len(matched)))
}

// dispatchRoundRobin walks matched accounts in config order, picks the first
// one below the 5-slot limit.
func (p *Processor) dispatchRoundRobin(source string, sig Signal, matched []config.TaskConfig, action, amount, unit, matchedRange string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	for _, task := range matched {
		if p.tryClaimSlot(task.SlotGroupKey(), task.Name) {
			p.logger.Info("signal", fmt.Sprintf("dispatch=round-robin account=[%s]", task.Name))
			p.mu.Unlock()
			p.executeTaskWithFallback(source, sig, task, matched, action, amount, unit, matchedRange)
			p.mu.Lock()
			return
		}
	}

	p.logger.Info("signal", fmt.Sprintf("dispatch=round-robin all %d accounts full, signal dropped", len(matched)))
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

func (p *Processor) executeTask(source string, sig Signal, task config.TaskConfig, action, amount, unit, matchedRange string) {
	p.logger.Info("signal", fmt.Sprintf("source=%s orderID=%v account=[%s] action=%s symbol=%s amount=%s unit=%s timeRange=%s", source, sig.OrderID, task.Name, action, sig.Symbol, amount, unit, matchedRange))

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
	})
}

// isFallbackError reports whether an order error should trigger switching to
// another executable account.
func isFallbackError(err error) bool {
	return errors.Is(err, order.ErrOrderLimitReached) || errors.Is(err, order.ErrAccountUnavailable)
}

// executeTaskWithFallback is used by single-account dispatch modes (random and
// round-robin). If the selected account is unavailable (order limit reached,
// login expired, banned, etc.), it automatically tries the other matched
// executable accounts.
func (p *Processor) executeTaskWithFallback(source string, sig Signal, primary config.TaskConfig, matched []config.TaskConfig, action, amount, unit, matchedRange string) {
	p.logger.Info("signal", fmt.Sprintf("source=%s orderID=%v account=[%s] action=%s symbol=%s amount=%s unit=%s timeRange=%s", source, sig.OrderID, primary.Name, action, sig.Symbol, amount, unit, matchedRange))

	req := order.PlaceOrderRequest{
		Amount:     amount,
		Unit:       unit,
		Action:     action,
		Symbol:     sig.Symbol,
		TickerType: sig.TickerType,
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
			p.tryFallbackOrder(source, sig, matched, t.ID, t.SlotGroupKey(), r)
			return
		}

		p.logger.Error("signal", fmt.Sprintf("account=[%s] order error: %v", t.Name, err))
	}(primary, req)
}

// tryFallbackOrder attempts the remaining matched accounts in config order.
// It only tries one account per slot group, because accounts in the same group
// share the same token/account slot pool and would hit the same limit.
// Fallback is allowed to bypass the normal slot limit: once the primary
// account fails, we prefer to try another executable account even if its slot
// group is currently full.
func (p *Processor) tryFallbackOrder(source string, sig Signal, matched []config.TaskConfig, excludeID, excludeGroupKey string, req order.PlaceOrderRequest) {
	attemptedGroups := map[string]bool{excludeGroupKey: true}
	for _, task := range matched {
		if task.ID == excludeID {
			continue
		}
		groupKey := task.SlotGroupKey()
		if attemptedGroups[groupKey] {
			p.logger.Info("signal", fmt.Sprintf("account=[%s] skip fallback already attempted slotGroup=[%s]", task.Name, groupKey))
			continue
		}
		attemptedGroups[groupKey] = true

		p.logger.Info("signal", fmt.Sprintf("source=%s orderID=%v account=[%s] fallback after order limit (bypass slot limit) symbol=%s amount=%s unit=%s", source, sig.OrderID, task.Name, sig.Symbol, req.Amount, req.Unit))

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err := p.order.PlaceOrder(ctx, task, req)
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
