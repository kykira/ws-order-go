package balance

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kykira/ws-order-go/internal/config"
	"github.com/kykira/ws-order-go/internal/logs"
)

// --- helpers ---------------------------------------------------------------

type balanceRow struct {
	AccountType string
	Balance     string
	WalletName  string
}

func binanceBody(rows []balanceRow) string {
	type rowJSON struct {
		AccountType string `json:"accountType"`
		Balance     string `json:"balance"`
		WalletName  string `json:"walletName"`
	}
	data := make([]rowJSON, 0, len(rows))
	for _, r := range rows {
		data = append(data, rowJSON{AccountType: r.AccountType, Balance: r.Balance, WalletName: r.WalletName})
	}
	// data 一定是数组（空时是 []），便于测试 "empty balance payload" 分支。
	bs, err := json.Marshal(map[string]any{"code": "000000", "data": data})
	if err != nil {
		panic(err)
	}
	return string(bs)
}

// newTestService 构造一个指向临时目录快照文件的服务实例。
func newTestService(t *testing.T, tasks ...config.TaskConfig) *Service {
	t.Helper()
	dir := t.TempDir()
	mgr, err := config.LoadManager(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatalf("config.LoadManager: %v", err)
	}
	if err := mgr.Update(func(c *config.Config) { c.Tasks = tasks }); err != nil {
		t.Fatalf("config update: %v", err)
	}
	svc := NewService(mgr, logs.NewLogger(100))
	svc.snapshotPath = filepath.Join(dir, "daily_balance.json")
	return svc
}

func setTasks(t *testing.T, svc *Service, tasks ...config.TaskConfig) {
	t.Helper()
	if err := svc.cfgMgr.Update(func(c *config.Config) { c.Tasks = tasks }); err != nil {
		t.Fatalf("config update: %v", err)
	}
}

func binanceTask(id, csrf string) config.TaskConfig {
	return config.TaskConfig{
		ID:      id,
		Name:    id,
		Type:    "binance",
		Enabled: true,
		Auth:    map[string]string{"csrftoken": csrf, "p20t": "p20t-" + id},
	}
}

func useBalanceURL(t *testing.T, url string) {
	t.Helper()
	old := balanceURL
	balanceURL = url
	t.Cleanup(func() { balanceURL = old })
}

// --- (1) stale account sweep ----------------------------------------------

func TestFetchAllSweepsRemovedTask(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(binanceBody([]balanceRow{
			{AccountType: "FUTURE", Balance: "1000.00", WalletName: "USDⓈ-M Futures"},
			{AccountType: "SPOT", Balance: "234.56", WalletName: "Spot"},
		})))
	}))
	defer srv.Close()
	useBalanceURL(t, srv.URL)

	svc := newTestService(t, binanceTask("acc-1", "csrf-1"))
	svc.FetchAll(context.Background())

	got, ok := svc.GetBalances()["acc-1"]
	if !ok || got.Total != "1234.56" {
		t.Fatalf("first fetch: got %+v (ok=%v), want total 1234.56", got, ok)
	}

	// 账号被完全移出配置：必须从 balances 中清掉，而不是永远留着旧余额。
	setTasks(t, svc)
	svc.FetchAll(context.Background())

	if balances := svc.GetBalances(); len(balances) != 0 {
		t.Fatalf("removed task still present in balances: %+v", balances)
	}
	if summary := svc.Summary(); summary.AccountCount != 0 || summary.Total != "0.00" {
		t.Fatalf("removed task still inflates summary: %+v", summary)
	}
}

// --- (2) transient failure keeps last good values --------------------------

func TestTransientFailurePreservesPreviousBalance(t *testing.T) {
	var failing atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if failing.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("upstream exploded"))
			return
		}
		_, _ = w.Write([]byte(binanceBody([]balanceRow{
			{AccountType: "FUTURE", Balance: "1000.00"},
			{AccountType: "SPOT", Balance: "234.56"},
		})))
	}))
	defer srv.Close()
	useBalanceURL(t, srv.URL)

	svc := newTestService(t, binanceTask("acc-1", "csrf-1"))
	svc.FetchAll(context.Background())
	before := svc.GetBalances()["acc-1"]

	failing.Store(true)
	svc.FetchAll(context.Background())

	after := svc.GetBalances()["acc-1"]
	if after.Error == "" {
		t.Fatalf("expected error to be recorded, got %+v", after)
	}
	if after.Platform != "binance" {
		t.Fatalf("platform not preserved: %+v", after)
	}
	if after.Total != before.Total || after.Futures != before.Futures {
		t.Fatalf("failed fetch zeroed good data: before=%+v after=%+v", before, after)
	}
	if !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Fatalf("UpdatedAt changed on failed fetch: before=%v after=%v", before.UpdatedAt, after.UpdatedAt)
	}

	summary := svc.Summary()
	if summary.Total != "1234.56" || summary.AccountCount != 1 {
		t.Fatalf("summary dropped errored-but-known account: %+v", summary)
	}
	if fb := svc.GetFuturesBalances(); fb["acc-1"] != 1000 {
		t.Fatalf("errored-but-known account lost its dispatch weight: %+v", fb)
	}
}

// --- (3) midnight snapshot completeness -----------------------------------

func TestSnapshotSkippedOnPartialRoundThenRecordedComplete(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("csrftoken") == "bad" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(binanceBody([]balanceRow{{AccountType: "FUTURE", Balance: "1000.00"}})))
	}))
	defer srv.Close()
	useBalanceURL(t, srv.URL)

	svc := newTestService(t, binanceTask("acc-1", "good"), binanceTask("acc-2", "bad"))
	midnight := time.Date(2024, 3, 2, 0, 30, 0, 0, cnLocation)

	svc.FetchAll(context.Background())
	svc.recordDailySnapshotIfNeeded(midnight)
	if _, err := os.Stat(svc.snapshotPath); !os.IsNotExist(err) {
		t.Fatalf("partial round wrote a snapshot (stat err=%v)", err)
	}

	// 同一分钟内账号恢复：本轮完整，应该记录下来。
	setTasks(t, svc, binanceTask("acc-1", "good"), binanceTask("acc-2", "good"))
	svc.FetchAll(context.Background())
	svc.recordDailySnapshotIfNeeded(midnight)

	snaps := svc.loadSnapshotsLocked()
	if len(snaps) != 1 {
		t.Fatalf("complete round should record exactly 1 snapshot, got %+v", snaps)
	}
	if snaps[0].Date != "2024-03-02" || snaps[0].Total != "2000.00" {
		t.Fatalf("unexpected snapshot: %+v", snaps[0])
	}

	// 日期去重仍然有效。
	svc.recordDailySnapshotIfNeeded(midnight.Add(10 * time.Minute))
	if snaps := svc.loadSnapshotsLocked(); len(snaps) != 1 {
		t.Fatalf("date dedupe broken, snapshots=%+v", snaps)
	}

	// 非 0 点不记录。
	svc.recordDailySnapshotIfNeeded(time.Date(2024, 3, 3, 12, 0, 0, 0, cnLocation))
	if snaps := svc.loadSnapshotsLocked(); len(snaps) != 1 {
		t.Fatalf("snapshot recorded outside midnight, snapshots=%+v", snaps)
	}
}

// --- (4) corrupt snapshot backup + atomic save ----------------------------

func TestCorruptSnapshotIsBackedUpAndSavedAtomically(t *testing.T) {
	svc := newTestService(t)
	dir := filepath.Dir(svc.snapshotPath)
	if err := os.WriteFile(svc.snapshotPath, []byte(`{"snapshots":[{"date":"2024-01`), 0644); err != nil {
		t.Fatalf("seed corrupt snapshot: %v", err)
	}

	if snaps := svc.loadSnapshotsLocked(); snaps != nil {
		t.Fatalf("corrupt snapshot should yield empty history, got %+v", snaps)
	}

	backups, err := filepath.Glob(svc.snapshotPath + ".bak-*")
	if err != nil {
		t.Fatalf("glob backups: %v", err)
	}
	if len(backups) != 1 {
		t.Fatalf("want exactly 1 backup file, got %v", backups)
	}
	if _, err := os.Stat(svc.snapshotPath); !os.IsNotExist(err) {
		t.Fatalf("corrupt original should have been moved aside, stat err=%v", err)
	}

	// 下一次保存：原子写入，内容完整，不留临时文件。
	svc.saveSnapshotsLocked([]Snapshot{{Date: "2024-03-02", Total: "2000.00", RecordedAt: "2024-03-02T00:30:00+08:00"}})

	bs, err := os.ReadFile(svc.snapshotPath)
	if err != nil {
		t.Fatalf("read saved snapshot: %v", err)
	}
	var file snapshotFile
	if err := json.Unmarshal(bs, &file); err != nil {
		t.Fatalf("saved snapshot is not valid JSON: %v (%s)", err, bs)
	}
	if len(file.Snapshots) != 1 || file.Snapshots[0].Total != "2000.00" {
		t.Fatalf("unexpected saved snapshot: %+v", file.Snapshots)
	}
	if info, err := os.Stat(svc.snapshotPath); err != nil {
		t.Fatalf("stat saved snapshot: %v", err)
	} else if perm := info.Mode().Perm(); perm != 0644 {
		t.Fatalf("saved snapshot mode = %o, want 644", perm)
	}
	tmpLeftovers, err := filepath.Glob(filepath.Join(dir, filepath.Base(svc.snapshotPath)+".tmp-*"))
	if err != nil {
		t.Fatalf("glob temp files: %v", err)
	}
	if len(tmpLeftovers) != 0 {
		t.Fatalf("atomic save left temp files behind: %v", tmpLeftovers)
	}
}

// --- (5) fetch parsing robustness -----------------------------------------

func TestFetchBalanceParsingRobustness(t *testing.T) {
	cases := []struct {
		name        string
		body        string
		wantTotal   string
		wantFutures string
		wantErr     string
	}{
		{
			name: "non-numeric row skipped",
			body: binanceBody([]balanceRow{
				{AccountType: "FUTURE", Balance: "100.50"},
				{AccountType: "FUTURE", Balance: "1,234.56"},
				{AccountType: "SPOT", Balance: "10"},
			}),
			wantTotal:   "110.50",
			wantFutures: "100.50",
		},
		{
			name: "multiple futures rows summed",
			body: binanceBody([]balanceRow{
				{AccountType: "FUTURE", Balance: "10"},
				{AccountType: "FUTURE", Balance: "20.5"},
				{AccountType: "SPOT", Balance: "5"},
			}),
			wantTotal:   "35.50",
			wantFutures: "30.50",
		},
		{
			name:      "empty data array is an error",
			body:      binanceBody(nil),
			wantErr:   "empty balance payload",
			wantTotal: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			useBalanceURL(t, srv.URL)

			svc := newTestService(t)
			info, err := svc.fetchBalance(context.Background(), "csrf", "p20t")
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("want error containing %q, got info=%+v err=%v", tc.wantErr, info, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if info.Total != tc.wantTotal || info.Futures != tc.wantFutures {
				t.Fatalf("got total=%s futures=%s, want total=%s futures=%s", info.Total, info.Futures, tc.wantTotal, tc.wantFutures)
			}
			if tc.name == "non-numeric row skipped" {
				// “跳过”而不是“当成 0 静默计入”的唯一可观察差异就是一条告警日志。
				logged := false
				for _, entry := range svc.logger.Entries() {
					if strings.Contains(entry.Message, "1,234.56") {
						logged = true
						break
					}
				}
				if !logged {
					t.Fatalf("unparseable row was not logged: %+v", svc.logger.Entries())
				}
			}
		})
	}
}

func TestNonFiniteAndUnusableValuesDoNotLeak(t *testing.T) {
	svc := newTestService(t)
	svc.set("nan", Info{Platform: "binance", Total: "NaN", Futures: "NaN"})
	svc.set("futures-nan", Info{Platform: "binance", Total: "50.00", Futures: "NaN", Error: "http blip"})
	svc.set("negative", Info{Platform: "binance", Total: "-5", Futures: "-1"})

	fb := svc.GetFuturesBalances()
	for id, v := range fb {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			t.Fatalf("non-finite value leaked for %s: %v", id, v)
		}
	}
	if _, ok := fb["nan"]; ok {
		t.Fatalf("account with no usable value must not carry a weight: %+v", fb)
	}
	if v, ok := fb["futures-nan"]; !ok || v != 50 {
		t.Fatalf("NaN futures should fall back to Total: %+v", fb)
	}
	if v, ok := fb["negative"]; !ok || v != 0 {
		t.Fatalf("negative weight should clamp to 0: %+v", fb)
	}

	summary := svc.Summary()
	// -5 的账号仍计入合计（与旧行为一致），NaN 完全没有可用数值，必须忽略。
	if summary.AccountCount != 2 {
		t.Fatalf("unusable account counted in summary: %+v", summary)
	}
	if summary.Total != "45.00" {
		t.Fatalf("summary total = %s, want 45.00", summary.Total)
	}
}
