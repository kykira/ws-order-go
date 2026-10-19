package balance

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/kykira/ws-order-go/internal/config"
)

func useHIBTBalanceURL(t *testing.T, url string) {
	t.Helper()
	old := hibtBalanceURL
	hibtBalanceURL = url
	t.Cleanup(func() { hibtBalanceURL = old })
}

func hibtTask() config.TaskConfig {
	return config.TaskConfig{
		ID:      "hb",
		Name:    "hb",
		Type:    "hibt",
		Enabled: true,
		Auth: map[string]string{
			"authorization": "auth-value",
			"x-auth-token":  "token-value",
			"v":             "order-signature-must-not-be-reused",
		},
	}
}

func TestFetchHIBTBalanceSuccess(t *testing.T) {
	var gotAuth, gotToken, gotV, gotLang string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("authorization")
		gotToken = r.Header.Get("x-auth-token")
		gotV = r.URL.Query().Get("v")
		gotLang = r.URL.Query().Get("langCode")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"msg":"成功","data":{"coin":"usdt","amount":"324.42350882","frozen":"0","margin":"0"}}`))
	}))
	defer srv.Close()
	useHIBTBalanceURL(t, srv.URL)

	svc := newTestService(t, hibtTask())
	info, err := svc.fetchHIBTBalance(context.Background(), hibtTask())
	if err != nil {
		t.Fatalf("fetchHIBTBalance: %v", err)
	}
	if info.Total != "324.42" || info.Futures != "324.42" {
		t.Fatalf("want Total/Futures 324.42, got Total=%q Futures=%q", info.Total, info.Futures)
	}
	if info.UpdatedAt.IsZero() {
		t.Fatal("UpdatedAt was not set")
	}
	if gotAuth != "auth-value" || gotToken != "token-value" {
		t.Fatalf("auth headers not forwarded: authorization=%q x-auth-token=%q", gotAuth, gotToken)
	}
	if gotLang != "zh_CN" {
		t.Fatalf("langCode = %q, want zh_CN", gotLang)
	}
	// v must be a fresh millisecond timestamp, not the order signature.
	if _, err := strconv.ParseInt(gotV, 10, 64); err != nil {
		t.Fatalf("v = %q, want a numeric millisecond timestamp: %v", gotV, err)
	}
	if gotV == "order-signature-must-not-be-reused" {
		t.Fatal("reused the order signature as v")
	}
}

func TestFetchHIBTBalanceRejectsErrorCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":401,"msg":"token expired","data":{}}`))
	}))
	defer srv.Close()
	useHIBTBalanceURL(t, srv.URL)

	svc := newTestService(t, hibtTask())
	if _, err := svc.fetchHIBTBalance(context.Background(), hibtTask()); err == nil {
		t.Fatal("want an error for code != 0")
	}
}

func TestFetchHIBTBalanceRejectsUnparseableAmount(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"msg":"成功","data":{"coin":"usdt","amount":"--"}}`))
	}))
	defer srv.Close()
	useHIBTBalanceURL(t, srv.URL)

	svc := newTestService(t, hibtTask())
	if _, err := svc.fetchHIBTBalance(context.Background(), hibtTask()); err == nil {
		t.Fatal("want an error for an unparseable amount")
	}
}

// FetchAll must feed a HiBT account into the table the dashboard and the
// dynamic position sizing both read.
func TestFetchAllIncludesHIBTAccount(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"msg":"成功","data":{"coin":"usdt","amount":"324.42350882"}}`))
	}))
	defer srv.Close()
	useHIBTBalanceURL(t, srv.URL)

	svc := newTestService(t, hibtTask())
	svc.FetchAll(context.Background())

	infos := svc.GetBalances()
	info, ok := infos["hb"]
	if !ok {
		t.Fatal("HiBT account missing from GetBalances")
	}
	if info.Platform != "hibt" || info.Total != "324.42" {
		t.Fatalf("unexpected Info: %+v", info)
	}
	// This is the value dynamicAmount() reads.
	if got := svc.GetFuturesBalances()["hb"]; got != 324.42 {
		t.Fatalf("GetFuturesBalances[hb] = %v, want 324.42", got)
	}
	if got := svc.Summary().Total; got != "324.42" {
		t.Fatalf("Summary total = %q, want 324.42", got)
	}
}

func TestFetchAllHIBTMissingAuthIsAnError(t *testing.T) {
	svc := newTestService(t, config.TaskConfig{ID: "hb", Name: "hb", Type: "hibt", Enabled: true})
	svc.FetchAll(context.Background())

	info, ok := svc.GetBalances()["hb"]
	if !ok {
		t.Fatal("account should be present with an error flag")
	}
	if info.Error == "" {
		t.Fatal("want an error for missing credentials")
	}
	if _, ok := svc.GetFuturesBalances()["hb"]; ok {
		t.Fatal("an account with no usable value must not feed the dispatcher")
	}
}
