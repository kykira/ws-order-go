package order

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/kykira/ws-order-go/internal/logs"
	"github.com/kykira/ws-order-go/internal/orders"
)

func newRecordingClient() (*Client, *orders.Store) {
	store := orders.NewStore(50)
	c := NewClient(logs.NewLogger(50))
	c.SetRecorder(store)
	return c, store
}

func TestPlaceOrderRecordsSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = w.Write([]byte(`{"errno":"200","msg":"success"}`))
	}))
	defer srv.Close()

	c, store := newRecordingClient()
	withURLOverride(t, "turboflow", srv.URL)

	err := c.PlaceOrder(context.Background(), turboflowTask(), PlaceOrderRequest{
		Amount: "7", Action: "buy", Symbol: "BTCUSDT", Period: "5m", Strategy: "st-1",
	})
	if err != nil {
		t.Fatalf("PlaceOrder: %v", err)
	}

	list := store.List(10)
	if len(list) != 1 {
		t.Fatalf("records = %d, want 1", len(list))
	}
	rec := list[0]
	if rec.Status != orders.StatusSuccess {
		t.Fatalf("status = %q, want success", rec.Status)
	}
	if rec.Account != "tf-regression" || rec.Platform != "turboflow" {
		t.Fatalf("account/platform = %q/%q", rec.Account, rec.Platform)
	}
	if rec.Strategy != "st-1" || rec.Action != "buy" || rec.Symbol != "BTCUSDT" || rec.Period != "5m" {
		t.Fatalf("metadata = %+v", rec)
	}
	if rec.Amount != "7" {
		t.Fatalf("amount = %q, want 7", rec.Amount)
	}
	if rec.HTTPStatus != 200 || rec.Attempts != 1 {
		t.Fatalf("httpStatus/attempts = %d/%d, want 200/1", rec.HTTPStatus, rec.Attempts)
	}
	if rec.Source != "" || rec.Error != "" {
		t.Fatalf("source/error = %q/%q, want empty", rec.Source, rec.Error)
	}
}

func TestPlaceOrderRecordsFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = w.Write([]byte(`{"code":500,"message":"持仓上限","success":false}`))
	}))
	defer srv.Close()

	c, store := newRecordingClient()
	withURLOverride(t, "turboflow", srv.URL)

	err := c.PlaceOrder(context.Background(), turboflowTask(), PlaceOrderRequest{
		Amount: "7", Action: "sell", Symbol: "ETHUSDT", Period: "30m", Strategy: "st-2",
	})
	if err == nil {
		t.Fatal("PlaceOrder = nil, want a business error")
	}

	rec := store.List(1)[0]
	if rec.Status != orders.StatusFailed {
		t.Fatalf("status = %q, want failed", rec.Status)
	}
	if rec.Code != "500" || rec.Message != "持仓上限" {
		t.Fatalf("code/message = %q/%q", rec.Code, rec.Message)
	}
	if rec.Error == "" {
		t.Fatal("error was not recorded")
	}
	if rec.Attempts != 1 {
		t.Fatalf("attempts = %d, want 1", rec.Attempts)
	}
}

// A retryable rejection followed by success must produce one record with two
// attempts and the caller-supplied source (fallback).
func TestPlaceOrderRecordsRetryAndSource(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if atomic.AddInt32(&calls, 1) == 1 {
			_, _ = w.Write([]byte(`{"errno":"1021110","msg":"too frequent"}`))
			return
		}
		_, _ = w.Write([]byte(`{"errno":"200","msg":"success"}`))
	}))
	defer srv.Close()

	c, store := newRecordingClient()
	withURLOverride(t, "turboflow", srv.URL)

	err := c.PlaceOrder(context.Background(), turboflowTask(), PlaceOrderRequest{
		Amount: "7", Action: "buy", Symbol: "BTCUSDT", Period: "5m", Strategy: "st-1",
		Source: "fallback",
	})
	if err != nil {
		t.Fatalf("PlaceOrder: %v", err)
	}

	rec := store.List(1)[0]
	if rec.Status != orders.StatusSuccess {
		t.Fatalf("status = %q, want success", rec.Status)
	}
	if rec.Attempts != 2 {
		t.Fatalf("attempts = %d, want 2", rec.Attempts)
	}
	if rec.Source != "fallback" {
		t.Fatalf("source = %q, want fallback", rec.Source)
	}
}
