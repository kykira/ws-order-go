package signals

import (
	"context"
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

	mu             sync.Mutex
	seenSkip       map[string]int
	skipStartTime  map[string]time.Time
	orderCounts   map[string]int
	orderLastDecay map[string]time.Time
}

func NewProcessor(cfg *config.Manager, logger *logs.Logger, orderClient *order.Client) *Processor {
	return &Processor{
		cfg:            cfg,
		logger:         logger,
		order:          orderClient,
		seenSkip:       make(map[string]int),
		skipStartTime:  make(map[string]time.Time),
		orderCounts:    make(map[string]int),
		orderLastDecay: make(map[string]time.Time),
	}
}

const maxOrdersPerWindow = 5
const orderDecayInterval = 30 * time.Minute

// Handle 处理一条信号。applySkip 表示是否应用 skipSignals 逻辑。
func (p *Processor) Handle(source string, sig Signal, applySkip bool) error {
	cfg := p.cfg.Get()

	// 所有信号到达时，先对所有账号执行 decay（不依赖 proba 匹配）
	p.applyDecayToAll(cfg.Tasks)

	action := strings.ToLower(strings.TrimSpace(sig.Action))
	if action == "" {
		p.logger.Error("signal", "empty action, ignore")
		return nil
	}

	amount := strings.TrimSpace(sig.Amount)
	unit := strings.TrimSpace(sig.Unit)

	if source == "upstream" && action == "test" {
		p.logger.Info("signal", "上游心跳保活 (ping)")
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

		// Proba filter: skip if proba doesn't match the action direction.
		// buy  requires proba >= minProba, sell requires proba <= 1-minProba.
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
			if p.tryClaimSlot(task.ID, task.Name) {
				p.mu.Unlock()
				p.executeTask(source, sig, task, action, amount, unit, matchedRange)
				p.mu.Lock()
			}
		}
		p.mu.Unlock()
	}

	return nil
}

// applyDecayToAll runs decay on all accounts every time a signal arrives.
// This ensures time-based slot recovery independent of proba matching.
func (p *Processor) applyDecayToAll(tasks []config.TaskConfig) {
	p.mu.Lock()
	defer p.mu.Unlock()

	now := time.Now()
	for _, task := range tasks {
		if !task.Enabled {
			continue
		}
		count := p.orderCounts[task.ID]
		if count == 0 {
			continue
		}
		last, ok := p.orderLastDecay[task.ID]
		if !ok {
			continue
		}
		elapsed := now.Sub(last)
		if elapsed < orderDecayInterval {
			continue
		}
		decay := int(elapsed / orderDecayInterval)
		if decay > count {
			decay = count
		}
		count -= decay
		p.orderCounts[task.ID] = count
		p.orderLastDecay[task.ID] = last.Add(time.Duration(decay) * orderDecayInterval)
		if decay > 0 {
			p.logger.Info("signal", fmt.Sprintf("account=[%s] decay -%d (30m×%d) count %d→%d", task.Name, decay, decay, p.orderCounts[task.ID]+decay, count))
		}
	}
}

// tryClaimSlot tries to claim one order slot (max 5 per 30min).
// Decay is handled upstream by applyDecayToAll; this only checks+claims.
// Returns true if a slot was claimed, false if the account is full.
// Must be called with p.mu held.
func (p *Processor) tryClaimSlot(taskID, taskName string) bool {
	count := p.orderCounts[taskID]

	if count >= maxOrdersPerWindow {
		p.logger.Info("signal", fmt.Sprintf("account=[%s] slots full %d/%d, skipping", taskName, count, maxOrdersPerWindow))
		return false
	}

	count++
	p.orderCounts[taskID] = count
	// Ensure orderLastDecay is initialised so applyDecayToAll can work
	if _, ok := p.orderLastDecay[taskID]; !ok {
		p.orderLastDecay[taskID] = time.Now()
	}
	p.logger.Info("signal", fmt.Sprintf("account=[%s] slot %d/%d", taskName, count, maxOrdersPerWindow))
	return true
}

// dispatchRandom picks one matched account, respecting slot limits.
func (p *Processor) dispatchRandom(source string, sig Signal, matched []config.TaskConfig, action, amount, unit, matchedRange string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	// Shuffle to randomise which account gets tried first
	perm := rand.Perm(len(matched))
	for _, i := range perm {
		task := matched[i]
		if p.tryClaimSlot(task.ID, task.Name) {
			p.mu.Unlock()
			p.executeTask(source, sig, task, action, amount, unit, matchedRange)
			p.mu.Lock()
			p.logger.Info("signal", fmt.Sprintf("dispatch=random picked account=[%s]", task.Name))
			return
		}
	}
	p.logger.Info("signal", fmt.Sprintf("dispatch=random all %d accounts full, signal dropped", len(matched)))
}

// dispatchRoundRobin walks matched accounts in config order, picks the first
// one below the 5-slot limit. Each account has 5 order slots; every 30 minutes
// one slot is refilled (decay). +1 on order, -1 per 30min, floor 0.
func (p *Processor) dispatchRoundRobin(source string, sig Signal, matched []config.TaskConfig, action, amount, unit, matchedRange string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	for _, task := range matched {
		if p.tryClaimSlot(task.ID, task.Name) {
			p.logger.Info("signal", fmt.Sprintf("dispatch=round-robin account=[%s]", task.Name))
			p.mu.Unlock()
			p.executeTask(source, sig, task, action, amount, unit, matchedRange)
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
