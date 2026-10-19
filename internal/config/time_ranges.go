package config

import (
	"fmt"
	"strings"
	"time"
)

const MaxTimeRangesPerTask = 4

type TimeRange struct {
	Start string `json:"start"`
	End   string `json:"end"`
}

func PrepareTasks(tasks []TaskConfig) error {
	// Reserve every explicitly configured ID first so a generated ID can never
	// collide with one the operator set by hand.
	used := make(map[string]bool, len(tasks))
	for i := range tasks {
		if id := strings.TrimSpace(tasks[i].ID); id != "" {
			used[id] = true
		}
	}

	for i := range tasks {
		if strings.TrimSpace(tasks[i].ID) == "" {
			// Preserve the historical task-<index> naming, but skip IDs that
			// are already taken instead of silently duplicating them.
			for n := i + 1; ; n++ {
				candidate := fmt.Sprintf("task-%d", n)
				if !used[candidate] {
					tasks[i].ID = candidate
					used[candidate] = true
					break
				}
			}
		}
		normalizeTask(&tasks[i], i)
		if err := validateTask(tasks[i]); err != nil {
			name := tasks[i].Name
			if name == "" {
				name = fmt.Sprintf("task-%d", i+1)
			}
			return fmt.Errorf("task[%d] %q invalid: %w", i+1, name, err)
		}
	}

	// Duplicate IDs are not just cosmetic: task.ID keys the skip counters and
	// the balance map, so two accounts sharing an ID would share that state.
	seen := make(map[string]int, len(tasks))
	for i := range tasks {
		if prev, dup := seen[tasks[i].ID]; dup {
			return fmt.Errorf("task[%d] %q invalid: duplicate task id %q (also used by task[%d])",
				i+1, tasks[i].Name, tasks[i].ID, prev+1)
		}
		seen[tasks[i].ID] = i
	}
	return nil
}

// validDispatch lists the dispatch modes understood by signals.Processor.
var validDispatch = map[string]bool{
	"random":      true,
	"weighted":    true,
	"round-robin": true,
	"all":         true,
}

// NormalizeDispatch returns a dispatch mode the processor implements. Anything
// empty or unrecognized falls back to "round-robin": signals.Processor treats
// an unknown mode as "all" (an order on EVERY matched account), so a typo must
// not silently fan out to all accounts.
func NormalizeDispatch(mode string) string {
	v := strings.ToLower(strings.TrimSpace(mode))
	if validDispatch[v] {
		return v
	}
	return "round-robin"
}

// PrepareStrategies validates strategy/group/account bindings. It runs on load
// and on every update so the API can never persist a binding that would size
// an order unsafely.
func PrepareStrategies(strategies []StrategyConfig) error {
	seenStrategy := make(map[string]bool, len(strategies))
	seenGroup := make(map[string]bool)

	for si := range strategies {
		st := &strategies[si]
		st.ID = strings.TrimSpace(st.ID)
		st.Name = strings.TrimSpace(st.Name)
		if st.Name == "" {
			st.Name = st.ID
		}
		key := st.ID
		if key == "" {
			key = st.Name
		}
		if key == "" {
			return fmt.Errorf("strategy[%d] must have an id or name", si+1)
		}
		if seenStrategy[key] {
			return fmt.Errorf("duplicate strategy id %q", key)
		}
		seenStrategy[key] = true

		for gi := range st.Groups {
			g := &st.Groups[gi]
			g.Dispatch = NormalizeDispatch(g.Dispatch)
			g.ID = strings.TrimSpace(g.ID)
			if g.ID == "" {
				g.ID = fmt.Sprintf("%s-g%d", key, gi+1)
			}
			if seenGroup[g.ID] {
				return fmt.Errorf("duplicate group id %q", g.ID)
			}
			seenGroup[g.ID] = true

			for ai := range g.Accounts {
				a := &g.Accounts[ai]
				a.AccountID = strings.TrimSpace(a.AccountID)
				a.Amount = strings.TrimSpace(a.Amount)
				if a.AccountID == "" {
					return fmt.Errorf("group[%s] account[%d] is missing accountId", g.ID, ai+1)
				}
				// percent is a share of the account's available balance. Cap it
				// at 100% so a typo cannot size an order far beyond the account.
				if a.Percent < 0 || a.Percent > 100 {
					return fmt.Errorf("group[%s] account[%s] percent must be between 0 and 100 (got %g)",
						g.ID, a.AccountID, a.Percent)
				}
				if a.MinAmount < 0 || a.MaxAmount < 0 {
					return fmt.Errorf("group[%s] account[%s] minAmount/maxAmount cannot be negative",
						g.ID, a.AccountID)
				}
				if a.MinAmount > 0 && a.MaxAmount > 0 && a.MinAmount > a.MaxAmount {
					return fmt.Errorf("group[%s] account[%s] minAmount %g is greater than maxAmount %g",
						g.ID, a.AccountID, a.MinAmount, a.MaxAmount)
				}
			}
		}
	}
	return nil
}

func normalizeTask(task *TaskConfig, index int) {
	if task.ID == "" {
		task.ID = fmt.Sprintf("task-%d", index+1)
	}
	if task.Name == "" {
		task.Name = task.ID
	}
	task.TimeRanges = normalizeTimeRanges(task.TimeRanges)
	task.Auth = normalizeAuth(task.Type, task.Auth)
}

func normalizeTimeRanges(ranges []TimeRange) []TimeRange {
	if len(ranges) == 0 {
		return nil
	}

	out := make([]TimeRange, 0, len(ranges))
	for _, tr := range ranges {
		start := canonicalClock(strings.TrimSpace(tr.Start))
		end := canonicalClock(strings.TrimSpace(tr.End))
		if start == "" && end == "" {
			continue
		}
		out = append(out, TimeRange{
			Start: start,
			End:   end,
		})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func validateTask(task TaskConfig) error {
	if len(task.TimeRanges) > MaxTimeRangesPerTask {
		return fmt.Errorf("time ranges exceed limit %d", MaxTimeRangesPerTask)
	}

	for idx, tr := range task.TimeRanges {
		label := fmt.Sprintf("timeRanges[%d]", idx)
		if tr.Start == "" || tr.End == "" {
			return fmt.Errorf("%s start and end are both required", label)
		}

		startMinute, err := parseClockMinute(tr.Start)
		if err != nil {
			return fmt.Errorf("%s start: %w", label, err)
		}
		endMinute, err := parseClockMinute(tr.End)
		if err != nil {
			return fmt.Errorf("%s end: %w", label, err)
		}
		if startMinute == endMinute {
			return fmt.Errorf("%s start and end cannot be the same", label)
		}
	}

	return nil
}

// IsTimeAllowed reports whether now falls in at least one configured time range.
// Empty configuration means allow all trigger signals.
func IsTimeAllowed(ranges []TimeRange, now time.Time) (bool, string, error) {
	ranges = normalizeTimeRanges(ranges)
	if len(ranges) == 0 {
		return true, "all-day", nil
	}

	currentMinute := now.Hour()*60 + now.Minute()
	for _, tr := range ranges {
		startMinute, err := parseClockMinute(tr.Start)
		if err != nil {
			return false, "", err
		}
		endMinute, err := parseClockMinute(tr.End)
		if err != nil {
			return false, "", err
		}
		if startMinute == endMinute {
			return false, "", fmt.Errorf("invalid time range %s-%s", tr.Start, tr.End)
		}

		if minuteInRange(currentMinute, startMinute, endMinute) {
			return true, formatTimeRange(tr), nil
		}
	}

	return false, "", nil
}

func FormatTimeRanges(ranges []TimeRange) string {
	ranges = normalizeTimeRanges(ranges)
	if len(ranges) == 0 {
		return "all-day"
	}

	parts := make([]string, 0, len(ranges))
	for _, tr := range ranges {
		parts = append(parts, formatTimeRange(tr))
	}
	return strings.Join(parts, ", ")
}

func formatTimeRange(tr TimeRange) string {
	return tr.Start + "-" + tr.End
}

func minuteInRange(currentMinute, startMinute, endMinute int) bool {
	if startMinute < endMinute {
		return currentMinute >= startMinute && currentMinute <= endMinute
	}
	return currentMinute >= startMinute || currentMinute <= endMinute
}

// canonicalClock rewrites a parseable clock value into the canonical HH:MM
// form. time.Parse accepts single-digit hours ("9:00"), which the web UI
// rejects, so without this a hand-edited config would load but then be
// impossible to save from the UI.
func canonicalClock(value string) string {
	if value == "" {
		return ""
	}
	parsed, err := time.Parse("15:04", value)
	if err != nil {
		return value
	}
	return parsed.Format("15:04")
}

func parseClockMinute(value string) (int, error) {
	parsed, err := time.Parse("15:04", value)
	if err != nil {
		return 0, fmt.Errorf("must use HH:00 or HH:30 format")
	}
	m := parsed.Minute()
	if m != 0 && m != 30 {
		return 0, fmt.Errorf("must use half-hour boundaries (HH:00 or HH:30)")
	}
	return parsed.Hour()*60 + m, nil
}
