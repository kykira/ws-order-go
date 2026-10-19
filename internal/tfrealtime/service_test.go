package tfrealtime

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/kykira/ws-order-go/internal/config"
	"github.com/kykira/ws-order-go/internal/logs"
)

func TestSignedURL(t *testing.T) {
	s := NewService(nil, nil)
	raw, err := s.signedURL("test-token")
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if q.Get("API-KEY") != accessKeyHex {
		t.Fatalf("API-KEY = %q", q.Get("API-KEY"))
	}
	if q.Get("Authorization") != "test-token" {
		t.Fatalf("Authorization = %q", q.Get("Authorization"))
	}
	if q.Get("TIMESTAMP") == "" || q.Get("SIGN") == "" {
		t.Fatalf("missing timestamp/sign: %s", raw)
	}
	if len(q.Get("SIGN")) != 128 {
		t.Fatalf("SIGN length = %d, want 128", len(q.Get("SIGN")))
	}
	if !strings.HasPrefix(raw, realtimeBase) {
		t.Fatalf("unexpected base: %s", raw)
	}
}

func TestUpdatesNotify(t *testing.T) {
	s := NewService(nil, nil)
	ch := s.Updates()
	raw := []byte(`{"group":"evt_cfg_6","data":{"pc":{"pid":"6","pn":"BTC/USDT","e":true,"ocs":[{"du":1800,"arr":"0.79","brr":"0.77","minAmt":"2","maxAmt":"500","qe":true,"s":"valid"}]}}}`)
	s.handleMessage(raw)

	select {
	case <-ch:
	default:
		t.Fatal("Updates channel was not notified")
	}
	if s.GetSnapshot().Revision != 1 {
		t.Fatalf("revision = %d, want 1", s.GetSnapshot().Revision)
	}
}

func TestHandleEvtCfgMessage(t *testing.T) {
	s := NewService(nil, nil)
	raw := []byte(`{"group":"evt_cfg_6","data":{"pc":{"pid":"6","pn":"BTC/USDT","e":true,"ocs":[{"du":1800,"arr":"0.79","brr":"0.77","minAmt":"2","maxAmt":"500","qe":true,"s":"valid"}]}}}`)
	s.handleMessage(raw)

	p, ok := s.GetSnapshot().Pairs["BTCUSDT"]
	if !ok {
		t.Fatal("BTCUSDT pair not cached")
	}
	if p.PairID != "6" || len(p.Options) != 1 {
		t.Fatalf("unexpected pair: %+v", p)
	}
	o := p.Options[0]
	if o.Duration != 1800 || o.UpRate != "0.77" || o.DownRate != "0.79" {
		t.Fatalf("unexpected option: %+v", o)
	}
}

const goodEvtCfgBTC = `{"group":"evt_cfg_6","data":{"pc":{"pid":"6","pn":"BTC/USDT","e":true,"ocs":[{"du":1800,"arr":"0.79","brr":"0.77","minAmt":"2","maxAmt":"500","qe":true,"s":"valid"}]}}}`

func testConfigManager(t *testing.T, tasksJSON string) *config.Manager {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"tasks":`+tasksJSON+`}`), 0o644); err != nil {
		t.Fatal(err)
	}
	mgr, err := config.LoadManager(path)
	if err != nil {
		t.Fatalf("LoadManager: %v", err)
	}
	return mgr
}

// A status-only/schema-variant frame must not replace a cached good pair, bump
// the revision, or clear an existing error.
func TestHandleMessageIgnoresUnusablePayload(t *testing.T) {
	s := NewService(nil, nil)
	s.handleMessage([]byte(goodEvtCfgBTC))

	before, ok := s.GetPair("BTCUSDT")
	if !ok || len(before.Options) != 1 {
		t.Fatalf("good frame not cached: %+v ok=%v", before, ok)
	}
	revBefore := s.GetSnapshot().Revision

	s.setError("boom") // e.g. a subscribe rejection; must survive unusable frames

	for _, raw := range []string{
		`{"group":"evt_cfg_6","data":{"pc":{}}}`,
		`{"group":"evt_cfg_6","data":{}}`,
		`{"group":"evt_cfg_6","data":"not-an-object"}`,
		`{"group":"evt_cfg_6","data":{"pc":{"pn":"   ","ocs":[]}}}`,
		`{"group":"evt_cfg_9","data":{"pc":{"pn":"X","ocs":[{"du":60}]}}}`,
		`not json at all`,
	} {
		s.handleMessage([]byte(raw))
	}

	after, ok := s.GetPair("BTCUSDT")
	if !ok {
		t.Fatal("cached pair was wiped")
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("cached pair changed:\n before=%+v\n after =%+v", before, after)
	}
	snap := s.GetSnapshot()
	if snap.Revision != revBefore {
		t.Fatalf("revision bumped by unusable frame: %d -> %d", revBefore, snap.Revision)
	}
	if snap.LastError != "boom" {
		t.Fatalf("LastError = %q, want it preserved", snap.LastError)
	}
}

// A usable frame must update the cache, bump the revision, and wake Updates waiters.
func TestHandleMessageUsableFrameUpdatesAndNotifies(t *testing.T) {
	s := NewService(nil, nil)
	s.handleMessage([]byte(goodEvtCfgBTC))

	ch := s.Updates()
	s.handleMessage([]byte(`{"group":"evt_cfg_6","data":{"pc":{"pid":"6","pn":"BTC/USDT","e":true,"ocs":[{"du":1800,"arr":"0.81","brr":"0.83","minAmt":"2","maxAmt":"500","qe":true,"s":"valid"}]}}}`))

	select {
	case <-ch:
	default:
		t.Fatal("Updates channel was not notified for a usable frame")
	}
	snap := s.GetSnapshot()
	if snap.Revision != 2 {
		t.Fatalf("revision = %d, want 2", snap.Revision)
	}
	if !snap.Connected || snap.LastError != "" {
		t.Fatalf("snapshot connected=%v lastError=%q", snap.Connected, snap.LastError)
	}
	p, ok := s.GetPair(" btcusdt ")
	if !ok {
		t.Fatal("pair not cached")
	}
	if len(p.Options) != 1 || p.Options[0].UpRate != "0.83" || p.Options[0].DownRate != "0.81" {
		t.Fatalf("options not updated: %+v", p.Options)
	}
}

// GetPair/GetSnapshot must not hand out the live Options backing array.
func TestGetPairCopiesOptions(t *testing.T) {
	s := NewService(nil, nil)
	s.handleMessage([]byte(goodEvtCfgBTC))

	p, ok := s.GetPair("BTCUSDT")
	if !ok || len(p.Options) != 1 {
		t.Fatalf("pair not cached: %+v ok=%v", p, ok)
	}
	p.Options[0].UpRate = "mutated"

	again, _ := s.GetPair("BTCUSDT")
	if again.Options[0].UpRate != "0.77" {
		t.Fatalf("cache mutated through GetPair slice: %q", again.Options[0].UpRate)
	}

	sp := s.GetSnapshot().Pairs["BTCUSDT"]
	sp.Options[0].DownRate = "mutated"
	if got := s.GetSnapshot().Pairs["BTCUSDT"].Options[0].DownRate; got != "0.79" {
		t.Fatalf("cache mutated through GetSnapshot slice: %q", got)
	}
}

// Start must be idempotent, Stop must close the connection, exit the loop and be
// safe to call repeatedly (and after a restart).
func TestStartIdempotentAndStop(t *testing.T) {
	var conns atomic.Int64
	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		conns.Add(1)
		defer c.Close()
		for {
			if _, _, err := c.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer srv.Close()

	mgr := testConfigManager(t, `[
		{"id":"tf-1","name":"tf-one","type":"turboflow","enabled":true,"auth":{"authorization":"token-one"}},
		{"id":"tf-2","name":"tf-two","type":"turboflow","enabled":true,"auth":{"authorization":"token-two"}}
	]`)
	s := NewService(mgr, logs.NewLogger(50))
	s.reconnectDelay = 20 * time.Millisecond
	s.wsBase = "ws://" + strings.TrimPrefix(srv.URL, "http://")

	s.Start()
	s.Start() // must be a no-op

	deadline := time.Now().Add(3 * time.Second)
	for !s.GetSnapshot().Connected && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !s.GetSnapshot().Connected {
		t.Fatal("service never connected")
	}
	time.Sleep(150 * time.Millisecond) // give a duplicate Start time to dial
	if n := conns.Load(); n != 1 {
		t.Fatalf("server saw %d connections after two Start calls, want 1", n)
	}

	s.Stop()
	s.Stop() // idempotent
	if snap := s.GetSnapshot(); snap.Connected {
		t.Fatal("Connected still true after Stop")
	}
	time.Sleep(200 * time.Millisecond) // a live loop would redial after 20ms
	if n := conns.Load(); n != 1 {
		t.Fatalf("reconnect loop survived Stop: %d connections", n)
	}

	// Restart after Stop must work.
	s.Start()
	deadline = time.Now().Add(3 * time.Second)
	for conns.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := conns.Load(); n != 2 {
		t.Fatalf("restart did not connect: %d connections", n)
	}
	s.Stop()
}

// Stop must be safe when the service was never started.
func TestStopWithoutStart(t *testing.T) {
	s := NewService(nil, nil)
	s.Stop()
	s.Stop()
	if snap := s.GetSnapshot(); snap.Connected || snap.LastError != "" {
		t.Fatalf("unexpected snapshot after Stop: %+v", snap)
	}
}

// The reconnect loop must rotate through enabled TurboFlow accounts instead of
// pinning itself to the first (possibly expired) token.
func TestNextEnabledAuthRotates(t *testing.T) {
	mgr := testConfigManager(t, `[
		{"id":"tf-a","name":"acct-a","type":"turboflow","enabled":true,"auth":{"authorization":"token-a"}},
		{"id":"tf-disabled","name":"acct-disabled","type":"turboflow","enabled":false,"auth":{"authorization":"token-disabled"}},
		{"id":"tf-b","name":"acct-b","type":"turboflow","enabled":true,"auth":{"authorization":"token-b"}},
		{"id":"other","name":"acct-other","type":"binance","enabled":true,"auth":{"authorization":"token-other"}},
		{"id":"tf-empty","name":"acct-empty","type":"turboflow","enabled":true,"auth":{"authorization":"   "}}
	]`)
	s := NewService(mgr, nil)

	var tokens, labels []string
	for i := 0; i < 4; i++ {
		token, label := s.nextEnabledAuth()
		tokens = append(tokens, token)
		labels = append(labels, label)
	}
	if want := []string{"token-a", "token-b", "token-a", "token-b"}; !reflect.DeepEqual(tokens, want) {
		t.Fatalf("tokens = %v, want %v", tokens, want)
	}
	if want := []string{"acct-a", "acct-b", "acct-a", "acct-b"}; !reflect.DeepEqual(labels, want) {
		t.Fatalf("labels = %v, want %v", labels, want)
	}
	if got := s.firstEnabledAuth(); got != "token-a" {
		t.Fatalf("firstEnabledAuth = %q, want token-a", got)
	}
	if got := NewService(nil, nil).firstEnabledAuth(); got != "" {
		t.Fatalf("firstEnabledAuth with nil config = %q, want empty", got)
	}
}
