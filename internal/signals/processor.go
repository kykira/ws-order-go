package signals

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"wsordergo/internal/config"
	"wsordergo/internal/logs"
	"wsordergo/internal/order"
)

type Signal struct {
	Type      string      `json:"type"`
	OrderID   interface{} `json:"orderID"`
	Action    string      `json:"action"`
	Timestamp string      `json:"timestamp"`

	// Keep these for backward compatibility or future extension if needed,
	// though they may not be present in the new signal format.
	Amount    string `json:"amount,omitempty"`
	Unit      string `json:"unit,omitempty"`
	Direction string `json:"direction,omitempty"`
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

// Handle 处理一条信号。applySkip 表示是否应用 skipSignals 逻辑（仅建议对上游 WS 启用）。
func (p *Processor) Handle(source string, sig Signal, applySkip bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	cfg := p.cfg.Get()

	action := strings.ToLower(strings.TrimSpace(sig.Action))
	if action == "" {
		p.logger.Error("signal", "empty action, ignore")
		return nil
	}

	amount := strings.TrimSpace(sig.Amount)
	unit := strings.TrimSpace(sig.Unit)

	// Special-case: upstream action=test is treated as a dry-run log only.
	if source == "upstream" && action == "test" {
		p.logger.Info("signal", fmt.Sprintf("source=%s orderID=%v type=%s action=%s (test only, no order)", source, sig.OrderID, sig.Type, action))
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if len(cfg.Tasks) == 0 {
		p.logger.Error("signal", "no tasks configured, ignore")
		return nil
	}

	var firstErr error
	for _, task := range cfg.Tasks {
		if !task.Enabled {
			continue
		}

		if applySkip && task.SkipSignals > 0 {
			now := time.Now()
			// Reset if 30 minutes have passed since the first skipped signal for this task
			if start, ok := p.skipStartTime[task.ID]; ok && now.Sub(start) > 30*time.Minute {
				p.seenSkip[task.ID] = 0
				delete(p.skipStartTime, task.ID)
				p.logger.Info("signal", fmt.Sprintf("task=%s skip counter reset after 30m", task.ID))
			}

			// If we still need to skip
			if p.seenSkip[task.ID] < task.SkipSignals {
				if p.seenSkip[task.ID] == 0 {
					p.skipStartTime[task.ID] = now
				}
				p.seenSkip[task.ID]++
				p.logger.Info("signal", fmt.Sprintf("skip %d/%d from %s for task=%s", p.seenSkip[task.ID], task.SkipSignals, source, task.ID))
				continue
			}
		}

		p.logger.Info("signal", fmt.Sprintf("source=%s orderID=%v task=%s action=%s amount=%s unit=%s", source, sig.OrderID, task.ID, action, amount, unit))

		if err := p.order.PlaceOrder(ctx, task, order.PlaceOrderRequest{
			Amount: amount,
			Unit:   unit,
			Action: action,
		}); err != nil {
			p.logger.Error("signal", fmt.Sprintf("task=%s order error: %v", task.ID, err))
			if firstErr == nil {
				firstErr = err
			}
			// Continue other tasks even if one fails.
			continue
		}
	}

	return firstErr
}
