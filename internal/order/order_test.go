package order

import (
	"context"
	"testing"
	"time"

	"github.com/kykira/ws-order-go/internal/config"
)

func TestBizResponseIsSuccess(t *testing.T) {
	cases := []struct {
		name string
		resp bizResponse
		want bool
	}{
		{"binance code 000000", bizResponse{Code: "000000"}, true},
		{"binance success flag", bizResponse{Success: true}, true},
		{"turboflow errno 200", bizResponse{Errno: "200", Msg: "success"}, true},
		{"turboflow msg success only", bizResponse{Msg: "SUCCESS"}, true},
		{"empty response", bizResponse{}, false},
		{"binance business error", bizResponse{Code: "93420004"}, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.resp.isSuccess(); got != tc.want {
				t.Fatalf("isSuccess() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestPeriodConversion(t *testing.T) {
	cases := []struct {
		period string
		sec    string
		min    string
	}{
		{"1m", "60", "1"},
		{"3m", "180", "3"},
		{"5m", "300", "5"},
		{"10m", "600", "10"},
		{"15m", "900", "15"},
		{"30m", "1800", "30"},
		{"1h", "3600", "60"},
		{"2h", "7200", "120"},
	}
	for _, tc := range cases {
		t.Run(tc.period, func(t *testing.T) {
			if got := periodToSeconds(tc.period); got != tc.sec {
				t.Fatalf("periodToSeconds(%q) = %q, want %q", tc.period, got, tc.sec)
			}
			if got := periodToMinutes(tc.period); got != tc.min {
				t.Fatalf("periodToMinutes(%q) = %q, want %q", tc.period, got, tc.min)
			}
		})
	}
}

func TestParseRandomDelay(t *testing.T) {
	cases := []struct {
		in       string
		min, max time.Duration
		ok       bool
	}{
		{"", 0, 0, false},
		{"0", 0, 0, false},
		{"5", 0, 5 * time.Second, true},
		{" 5 ", 0, 5 * time.Second, true},
		{"3-8", 3 * time.Second, 8 * time.Second, true},
		{"0-2", 0, 2 * time.Second, true},
		{"3~8", 3 * time.Second, 8 * time.Second, true},
		{"8-3", 0, 0, false},
		{"-5", 0, 0, false},
		{"3-", 0, 0, false},
		{"abc", 0, 0, false},
	}
	for _, tc := range cases {
		min, max, ok := parseRandomDelay(tc.in)
		if ok != tc.ok || min != tc.min || max != tc.max {
			t.Fatalf("parseRandomDelay(%q) = (%v,%v,%v), want (%v,%v,%v)", tc.in, min, max, ok, tc.min, tc.max, tc.ok)
		}
	}
}

func TestMaxRandomDelayOnlyForBinance(t *testing.T) {
	if got := MaxRandomDelay(config.TaskConfig{Type: "binance", RandomDelaySec: "3-8"}); got != 8*time.Second {
		t.Fatalf("MaxRandomDelay(binance 3-8) = %v, want 8s", got)
	}
	if got := MaxRandomDelay(config.TaskConfig{Type: "hibt", RandomDelaySec: "3-8"}); got != 0 {
		t.Fatalf("MaxRandomDelay(hibt) = %v, want 0 (binance only)", got)
	}
	if got := MaxRandomDelay(config.TaskConfig{Type: "binance"}); got != 0 {
		t.Fatalf("MaxRandomDelay(binance, unset) = %v, want 0", got)
	}
}

func TestRandomDelaySkippedWithoutBinanceConfig(t *testing.T) {
	c := &Client{}
	for _, task := range []config.TaskConfig{
		{Type: "hibt", RandomDelaySec: "5"},
		{Type: "binance"},
		{Type: "binance", RandomDelaySec: "0"},
	} {
		if err := c.randomDelay(context.Background(), task); err != nil {
			t.Fatalf("randomDelay(type=%q, config=%q) = %v, want nil", task.Type, task.RandomDelaySec, err)
		}
	}
}
