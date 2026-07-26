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
	for i := range tasks {
		normalizeTask(&tasks[i], i)
		if err := validateTask(tasks[i]); err != nil {
			name := tasks[i].Name
			if name == "" {
				name = fmt.Sprintf("task-%d", i+1)
			}
			return fmt.Errorf("task[%d] %q invalid: %w", i+1, name, err)
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
	if task.Method == "" {
		task.Method = "POST"
	}
	task.TimeRanges = normalizeTimeRanges(task.TimeRanges)
}

func normalizeTimeRanges(ranges []TimeRange) []TimeRange {
	if len(ranges) == 0 {
		return nil
	}

	out := make([]TimeRange, 0, len(ranges))
	for _, tr := range ranges {
		start := strings.TrimSpace(tr.Start)
		end := strings.TrimSpace(tr.End)
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
