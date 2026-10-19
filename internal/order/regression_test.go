package order

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kykira/ws-order-go/internal/config"
	"github.com/kykira/ws-order-go/internal/logs"
)

// The logger appends order.log under a path relative to the working directory,
// so run these tests from a scratch dir to keep the package directory clean.
func TestMain(m *testing.M) {
	orig, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(os.Stderr, "order tests: getwd:", err)
		os.Exit(1)
	}
	dir, err := os.MkdirTemp("", "order-regression-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "order tests: temp dir:", err)
		os.Exit(1)
	}
	if err := os.Chdir(dir); err != nil {
		fmt.Fprintln(os.Stderr, "order tests: chdir:", err)
		os.Exit(1)
	}
	code := m.Run()
	_ = os.Chdir(orig)
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

func newTestClient() *Client {
	return NewClient(logs.NewLogger(50))
}

// withURLOverride points a built-in account type at a local test server for the
// duration of the test.
func withURLOverride(t *testing.T, accountType, target string) {
	t.Helper()
	prev, had := urlOverrideForTests[accountType]
	urlOverrideForTests[accountType] = target
	t.Cleanup(func() {
		if had {
			urlOverrideForTests[accountType] = prev
			return
		}
		delete(urlOverrideForTests, accountType)
	})
}

func turboflowTask() config.TaskConfig {
	return config.TaskConfig{
		ID:        "tf-regression",
		Name:      "tf-regression",
		Type:      "turboflow",
		ValueBuy:  "1",
		ValueSell: "2",
		Auth: map[string]string{
			"account_id":    "12345",
			"authorization": "Bearer test-token",
			"uid":           "12345",
			"biz-pf":        "6",
		},
	}
}

func hibtTask() config.TaskConfig {
	return config.TaskConfig{
		ID:   "hibt-regression",
		Name: "hibt-regression",
		Type: "hibt",
		Auth: map[string]string{
			"authorization": "Bearer test-token",
			"x-auth-token":  "test-token",
			"v":             "abc+/=def",
		},
	}
}

func binanceTask() config.TaskConfig {
	return config.TaskConfig{
		ID:   "binance-regression",
		Name: "binance-regression",
		Type: "binance",
		Auth: map[string]string{
			"csrftoken": "test-csrf",
			"p20t":      "test-p20t",
		},
	}
}

// newResponder starts a test server that answers every request with the given
// status and body while counting the requests it received.
func newResponder(t *testing.T, status int, body string) (*httptest.Server, *int64) {
	t.Helper()
	var hits int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&hits, 1)
		io.Copy(io.Discard, r.Body)
		w.WriteHeader(status)
		io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// (a) A 2xx with a body that is not a JSON business response must never be
// reported as a successful order, except for raw webhook tasks.
func TestPlaceOrderRejectsUnparseable2xxBody(t *testing.T) {
	bodies := []string{
		"<html>502 Bad Gateway</html>",
		"",
		"OK",
		`{"ok":"yes"}`, // JSON, but a field type mismatch
	}

	for _, body := range bodies {
		t.Run(fmt.Sprintf("turboflow/%q", body), func(t *testing.T) {
			srv, hits := newResponder(t, http.StatusOK, body)
			withURLOverride(t, "turboflow", srv.URL)

			err := newTestClient().PlaceOrder(context.Background(), turboflowTask(), PlaceOrderRequest{
				Amount: "2", Action: "buy", Symbol: "BTCUSDT", Period: "5m",
			})
			if err == nil {
				t.Fatalf("PlaceOrder(200 %q) = nil, want error", body)
			}
			if !strings.Contains(err.Error(), "unparseable") {
				t.Fatalf("PlaceOrder(200 %q) error = %v, want unparseable body error", body, err)
			}
			if got := atomic.LoadInt64(hits); got != 1 {
				t.Fatalf("PlaceOrder(200 %q) sent %d requests, want 1 (no retry on bad body)", body, got)
			}
		})
	}

	t.Run("hibt/HTML", func(t *testing.T) {
		srv, _ := newResponder(t, http.StatusOK, "<html>502</html>")
		withURLOverride(t, "hibt", srv.URL)

		err := newTestClient().PlaceOrder(context.Background(), hibtTask(), PlaceOrderRequest{
			Amount: "2", Action: "buy", Symbol: "BTCUSDT", Period: "5m",
		})
		if err == nil {
			t.Fatal("PlaceOrder(hibt, 200 HTML) = nil, want error")
		}
	})

	for _, body := range bodies {
		t.Run(fmt.Sprintf("raw/%q", body), func(t *testing.T) {
			srv, hits := newResponder(t, http.StatusOK, body)
			task := config.TaskConfig{
				ID:     "raw-regression",
				Name:   "raw-regression",
				Type:   "raw",
				APIUrl: srv.URL,
				Method: "POST",
				Body:   `{"side":"{{action}}","symbol":"{{symbol}}","tf":"{{period}}"}`,
			}

			err := newTestClient().PlaceOrder(context.Background(), task, PlaceOrderRequest{
				Amount: "2", Action: "buy", Symbol: "BTCUSDT", Period: "5m",
			})
			if err != nil {
				t.Fatalf("PlaceOrder(raw, 200 %q) = %v, want nil", body, err)
			}
			if got := atomic.LoadInt64(hits); got != 1 {
				t.Fatalf("raw task sent %d requests, want 1", got)
			}
		})
	}
}

// (b) {"ok":true} acks: queued or duplicate mean success, neither means the
// upstream silently dropped the order.
func TestPlaceOrderQueuedDuplicateMatrix(t *testing.T) {
	cases := []struct {
		body    string
		wantErr bool
	}{
		{`{"ok":true,"queued":true,"duplicate":false}`, false},
		{`{"ok":true,"queued":false,"duplicate":true}`, false},
		{`{"ok":true,"queued":false,"duplicate":false}`, true},
		{`{"ok":true,"queued":false}`, true},
		{`{"ok":true}`, false},
	}

	for _, tc := range cases {
		t.Run(tc.body, func(t *testing.T) {
			srv, hits := newResponder(t, http.StatusOK, tc.body)
			withURLOverride(t, "turboflow", srv.URL)

			err := newTestClient().PlaceOrder(context.Background(), turboflowTask(), PlaceOrderRequest{
				Amount: "2", Action: "buy", Symbol: "BTCUSDT", Period: "5m",
			})
			if tc.wantErr && err == nil {
				t.Fatalf("PlaceOrder(200 %s) = nil, want error", tc.body)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("PlaceOrder(200 %s) = %v, want nil", tc.body, err)
			}
			if got := atomic.LoadInt64(hits); got != 1 {
				t.Fatalf("PlaceOrder(200 %s) sent %d requests, want 1", tc.body, got)
			}
		})
	}
}

// (c) A substitution that produces malformed JSON must fail before any request
// leaves the process.
func TestPlaceOrderRefusesMalformedBodyBeforeSending(t *testing.T) {
	cases := []struct {
		name string
		task config.TaskConfig
		req  PlaceOrderRequest
	}{
		{
			name: "turboflow empty period",
			task: turboflowTask(),
			req:  PlaceOrderRequest{Amount: "2", Action: "buy", Symbol: "BTCUSDT"},
		},
		{
			name: "binance empty amount",
			task: binanceTask(),
			req:  PlaceOrderRequest{Amount: "", Action: "buy", Symbol: "BTCUSDT", Period: "5m"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, hits := newResponder(t, http.StatusOK, `{"errno":"200","msg":"success"}`)
			withURLOverride(t, tc.task.Type, srv.URL)

			err := newTestClient().PlaceOrder(context.Background(), tc.task, tc.req)
			if err == nil {
				t.Fatal("PlaceOrder(malformed body) = nil, want error")
			}
			// Either the explicit empty-amount guard or the JSON-validity guard
			// may fire first; both refuse the order before it is sent.
			if msg := err.Error(); !strings.Contains(msg, "malformed") && !strings.Contains(msg, "empty amount") {
				t.Fatalf("PlaceOrder(malformed body) error = %v, want malformed body / empty amount error", err)
			}
			if got := atomic.LoadInt64(hits); got != 0 {
				t.Fatalf("malformed order sent %d requests, want 0", got)
			}
		})
	}
}

// (d) A signal-supplied amount cannot break out of its JSON string and rewrite
// other order fields.
func TestPlaceOrderEscapesInjectedAmount(t *testing.T) {
	injected := `1","return_rate":999,"x":"`
	var received string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		received = string(raw)
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `{"errno":"200","msg":"success"}`)
	}))
	defer srv.Close()
	withURLOverride(t, "turboflow", srv.URL)

	err := newTestClient().PlaceOrder(context.Background(), turboflowTask(), PlaceOrderRequest{
		Amount: injected, Action: "buy", Symbol: "BTCUSDT", Period: "5m",
	})
	if err != nil {
		t.Fatalf("PlaceOrder(injected amount) = %v, want nil", err)
	}

	var got map[string]any
	if err := json.Unmarshal([]byte(received), &got); err != nil {
		t.Fatalf("server received invalid JSON %q: %v", received, err)
	}
	if got["return_rate"] != float64(85) {
		t.Fatalf("return_rate = %v, want 85 (injection rewrote it); body=%s", got["return_rate"], received)
	}
	if got["amount"] != injected {
		t.Fatalf("amount = %v, want the escaped original %q", got["amount"], injected)
	}
	if got["pair_id"] != "6" || got["pool_id"] != float64(1) || got["coin_code"] != "1" {
		t.Fatalf("other fields not intact: %v (body=%s)", got, received)
	}
}

// URL placeholders are query values: escaped exactly once, uniformly.
func TestPlaceOrderQueryEscapesURLPlaceholders(t *testing.T) {
	var query url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.Query()
		io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `{"code":0,"message":"下单成功","success":true}`)
	}))
	defer srv.Close()
	withURLOverride(t, "hibt", srv.URL+"/place?v={{v}}&amount={{amount}}")

	err := newTestClient().PlaceOrder(context.Background(), hibtTask(), PlaceOrderRequest{
		Amount: "a&b=c", Action: "buy", Symbol: "BTCUSDT", Period: "5m",
	})
	if err != nil {
		t.Fatalf("PlaceOrder = %v, want nil", err)
	}
	if got := query.Get("v"); got != "abc+/=def" {
		t.Fatalf("query v = %q, want %q (escaped exactly once)", got, "abc+/=def")
	}
	if got := query.Get("amount"); got != "a&b=c" {
		t.Fatalf("query amount = %q, want %q", got, "a&b=c")
	}
}

// (4) An unmappable period must fail loudly instead of silently placing a
// 30-minute contract.
func TestPlaceOrderRejectsUnknownBinancePeriod(t *testing.T) {
	for _, period := range []string{"1m", "3m", "10m", ""} {
		t.Run(fmt.Sprintf("period=%q", period), func(t *testing.T) {
			srv, hits := newResponder(t, http.StatusOK, `{"code":"000000"}`)
			withURLOverride(t, "binance", srv.URL)

			err := newTestClient().PlaceOrder(context.Background(), binanceTask(), PlaceOrderRequest{
				Amount: "2", Action: "buy", Symbol: "BTCUSDT", Period: period,
			})
			if err == nil {
				t.Fatalf("PlaceOrder(binance, period=%q) = nil, want error", period)
			}
			if !strings.Contains(err.Error(), "unsupported period") {
				t.Fatalf("PlaceOrder(binance, period=%q) error = %v, want unsupported period error", period, err)
			}
			if got := atomic.LoadInt64(hits); got != 0 {
				t.Fatalf("unknown period sent %d requests, want 0", got)
			}
		})
	}
}

func TestBinanceTimeIncrementSupportedPeriodsUnchanged(t *testing.T) {
	want := map[string]string{
		"5m":  "FIVE_MINUTE",
		"15m": "FIFTEEN_MINUTE",
		"30m": "THIRTY_MINUTE",
		"1h":  "ONE_HOUR",
		"2h":  "TWO_HOUR",
		"4h":  "FOUR_HOUR",
	}
	for period, expected := range want {
		got, ok := binanceTimeIncrement(period)
		if !ok || got != expected {
			t.Fatalf("binanceTimeIncrement(%q) = (%q,%v), want (%q,true)", period, got, ok, expected)
		}
	}
	for _, period := range []string{"", "1m", "3m", "10m", "30s", "45s", "nope"} {
		if got, ok := binanceTimeIncrement(period); ok {
			t.Fatalf("binanceTimeIncrement(%q) = (%q,true), want unknown", period, got)
		}
	}
}

// (6) HiBT's timeUnit field is minutes; sub-minute periods must not be reported
// as whole minutes.
func TestPeriodToMinutesSubMinute(t *testing.T) {
	cases := map[string]string{
		"30s": "0.5",
		"45s": "0.75",
		"1m":  "1",
		"5m":  "5",
		"30m": "30",
		"1h":  "60",
	}
	for period, want := range cases {
		if got := periodToMinutes(period); got != want {
			t.Fatalf("periodToMinutes(%q) = %q, want %q", period, got, want)
		}
	}
}
