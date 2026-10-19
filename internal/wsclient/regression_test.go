package wsclient

// Regression tests for the outbound upstream WebSocket client. They cover the
// confirmed concurrency bugs:
//
//	(A) reconnect hot-spin after an upgrade-then-close upstream,
//	(B) zombie connection when Stop() lands while a dial is in flight,
//	(C) duplicate live connections for a single upstream after a config change,
//	(D) one leaked ping goroutine per reconnect,
//	(E) the unsynchronised c.ctx read (covered by -race on the above).
//
// Processor invocations are counted through the logger: with no tasks
// configured, signals.Processor.Handle logs exactly one "signal"-sourced entry
// and returns, so the log ring is an exact invocation counter and no order can
// ever be placed from these tests.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/kykira/ws-order-go/internal/config"
	"github.com/kykira/ws-order-go/internal/logs"
	"github.com/kykira/ws-order-go/internal/signals"
)

// testSignal is a signal that never reaches order placement: the test config has
// no tasks, so Handle only records "no tasks configured, ignore".
var testSignal = []byte(`{"action":"buy","symbol":"BTCUSDT","orderID":1}`)

// TestMain moves the working directory into a scratch dir: logs.Logger appends
// "signal"/"order" lines to order.log relative to the working directory, and
// these tests must not litter the package.
func TestMain(m *testing.M) {
	orig, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(os.Stderr, "wsclient tests: getwd:", err)
		os.Exit(1)
	}
	dir, err := os.MkdirTemp("", "wsclient-regression-")
	if err != nil {
		dir, err = os.MkdirTemp(orig, ".wsclient-regression-")
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "wsclient tests: temp dir:", err)
		os.Exit(1)
	}
	if err := os.Chdir(dir); err != nil {
		fmt.Fprintln(os.Stderr, "wsclient tests: chdir:", err)
		os.Exit(1)
	}
	code := m.Run()
	_ = os.Chdir(orig)
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// ── test upstream ──

var testUpgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin:     func(*http.Request) bool { return true },
}

type testUpstreamOpts struct {
	// beforeUpgrade runs in the HTTP handler before the handshake is answered;
	// it keeps a client dial in flight for as long as it blocks.
	beforeUpgrade func()
	// onConn replaces the default read-until-closed handler.
	onConn func(u *testUpstream, conn *websocket.Conn)
}

type testUpstream struct {
	beforeUpgrade func()
	onConn        func(*websocket.Conn)

	srv      *httptest.Server
	stop     chan struct{}
	stopOnce sync.Once

	mu      sync.Mutex
	total   int
	live    int
	maxLive int
	conns   []*websocket.Conn
}

func newTestUpstream(t *testing.T, opts testUpstreamOpts) *testUpstream {
	t.Helper()
	u := &testUpstream{
		stop:          make(chan struct{}),
		beforeUpgrade: opts.beforeUpgrade,
	}
	if opts.onConn != nil {
		u.onConn = func(conn *websocket.Conn) { opts.onConn(u, conn) }
	}
	u.srv = httptest.NewServer(http.HandlerFunc(u.handle))
	t.Cleanup(u.Close)
	return u
}

func (u *testUpstream) handle(w http.ResponseWriter, r *http.Request) {
	if hook := u.beforeUpgrade; hook != nil {
		hook()
	}
	conn, err := testUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}

	u.mu.Lock()
	u.total++
	u.live++
	if u.live > u.maxLive {
		u.maxLive = u.live
	}
	u.conns = append(u.conns, conn)
	u.mu.Unlock()

	defer func() {
		u.mu.Lock()
		u.live--
		u.mu.Unlock()
		_ = conn.Close()
	}()

	if u.onConn != nil {
		u.onConn(conn)
		return
	}
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
	}
}

func (u *testUpstream) WSURL() string {
	return "ws" + strings.TrimPrefix(u.srv.URL, "http")
}

// Total is the number of completed handshakes (== client dials that reached us).
func (u *testUpstream) Total() int { return u.read(func() int { return u.total }) }

// Live is the number of upstream connections not yet observed as closed.
func (u *testUpstream) Live() int { return u.read(func() int { return u.live }) }

// MaxLive is the highest concurrency of live connections ever observed.
func (u *testUpstream) MaxLive() int { return u.read(func() int { return u.maxLive }) }

func (u *testUpstream) read(f func() int) int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return f()
}

// Broadcast writes payload to every connection the upstream ever accepted.
func (u *testUpstream) Broadcast(payload []byte) {
	u.mu.Lock()
	conns := append([]*websocket.Conn(nil), u.conns...)
	u.mu.Unlock()
	for _, conn := range conns {
		_ = conn.WriteMessage(websocket.TextMessage, payload)
	}
}

func (u *testUpstream) CloseLiveConns() {
	u.mu.Lock()
	conns := append([]*websocket.Conn(nil), u.conns...)
	u.mu.Unlock()
	for _, conn := range conns {
		_ = conn.Close()
	}
}

func (u *testUpstream) Close() {
	u.stopOnce.Do(func() {
		close(u.stop)
		u.CloseLiveConns()
		u.srv.CloseClientConnections()
		u.srv.Close()
	})
}

// ── helpers ──

// newTestConfigManager returns a config manager with an empty task list, so a
// processor built on it can never place an order.
func newTestConfigManager(t *testing.T) *config.Manager {
	t.Helper()
	mgr, err := config.LoadManager(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatalf("load config manager: %v", err)
	}
	if err := mgr.Update(func(c *config.Config) {
		c.Tasks = []config.TaskConfig{}
		c.Upstreams = nil
	}); err != nil {
		t.Fatalf("empty config: %v", err)
	}
	return mgr
}

// newInertProcessor builds a processor that handles signals without ever
// placing an order (no tasks => Handle logs once and returns).
func newInertProcessor(t *testing.T, cfgMgr *config.Manager, logger *logs.Logger) *signals.Processor {
	t.Helper()
	if n := len(cfgMgr.Get().Tasks); n != 0 {
		t.Fatalf("test config must have no tasks, got %d (orders could be placed)", n)
	}
	return signals.NewProcessor(cfgMgr, logger, nil)
}

// countSignalLogs counts processor invocations since mark. Handle logs exactly
// one "signal"-sourced entry per call when no tasks are configured.
func countSignalLogs(logger *logs.Logger, mark time.Time) int {
	n := 0
	for _, e := range logger.Entries() {
		if e.Source == "signal" && e.Time.After(mark) {
			n++
		}
	}
	return n
}

// loopExited reports whether the loop goroutine has returned (its deferred
// cleanup clears loopDone).
func loopExited(c *Conn) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.loopDone == nil && !c.running
}

func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return true
		}
		if time.Now().After(deadline) {
			t.Logf("timeout after %s waiting for %s", timeout, what)
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// ── (A) reconnect hot-spin ──

func TestReconnectHotSpinIsBackedOff(t *testing.T) {
	// Upstream completes the handshake and closes immediately (auth rejection,
	// restart, LB drain): the classic tight-dial-loop trigger.
	u := newTestUpstream(t, testUpstreamOpts{
		onConn: func(_ *testUpstream, conn *websocket.Conn) { _ = conn.Close() },
	})
	logger := logs.NewLogger(1000)
	cfgMgr := newTestConfigManager(t)

	c := &Conn{ID: "spin", Name: "spin", URL: u.WSURL()}
	c.Start(logger, newInertProcessor(t, cfgMgr, logger))
	defer c.Stop()

	time.Sleep(time.Second)
	switch got := u.Total(); {
	case got < 1:
		t.Fatal("no dial reached the upstream")
	case got >= 10:
		t.Fatalf("hot-spin: %d dials in ~1s, want < 10", got)
	default:
		t.Logf("dials in ~1s: %d", got)
	}

	// Backoff must not turn into "never reconnect again".
	if !waitFor(t, 6*time.Second, "dial after the backoff", func() bool { return u.Total() >= 2 }) {
		t.Fatalf("client never reconnected after the backoff (%d dials)", u.Total())
	}
}

// ── (B) zombie connection after Stop ──

func TestStopDuringDialLeavesNoZombie(t *testing.T) {
	dialStarted := make(chan struct{})
	var startOnce sync.Once
	var attempts int32

	u := newTestUpstream(t, testUpstreamOpts{
		beforeUpgrade: func() {
			startOnce.Do(func() { close(dialStarted) })
			// Hold the handshake so Stop lands while the dial is in flight.
			time.Sleep(400 * time.Millisecond)
		},
		onConn: func(u *testUpstream, conn *websocket.Conn) {
			// Buffered signal written before the client's dial returns, then a
			// chatty stream: a zombie connection would keep handling these.
			send := func() bool {
				atomic.AddInt32(&attempts, 1)
				return conn.WriteMessage(websocket.TextMessage, testSignal) == nil
			}
			send()
			ticker := time.NewTicker(20 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-u.stop:
					return
				case <-ticker.C:
					if !send() {
						return
					}
				}
			}
		},
	})

	logger := logs.NewLogger(2000)
	cfgMgr := newTestConfigManager(t)
	proc := newInertProcessor(t, cfgMgr, logger)

	c := &Conn{ID: "zombie", Name: "zombie", URL: u.WSURL()}
	c.Start(logger, proc)
	defer c.Stop()

	if !waitFor(t, 3*time.Second, "the upstream to receive the dial", func() bool {
		select {
		case <-dialStarted:
			return true
		default:
			return false
		}
	}) {
		t.Fatal("upstream never saw a dial")
	}

	mark := time.Now()
	c.Stop()

	// Let the delayed handshake complete: the buggy code goes live here and then
	// stays live forever on a chatty upstream.
	deadline := time.Now().Add(1200 * time.Millisecond)
	for time.Now().Before(deadline) {
		if c.IsConnected() {
			t.Fatalf("connection became live %v after Stop (zombie)", time.Since(mark))
		}
		time.Sleep(20 * time.Millisecond)
	}

	if n := countSignalLogs(logger, mark); n != 0 {
		t.Fatalf("processor handled %d signal(s) after Stop (zombie connection)", n)
	}
	if got := atomic.LoadInt32(&attempts); got == 0 {
		t.Fatal("upstream never attempted to send a signal; zombie path not exercised")
	}
	if got := u.Live(); got != 0 {
		t.Fatalf("%d upstream connection(s) still live after Stop, want 0", got)
	}
	if !waitFor(t, 2*time.Second, "the loop goroutine to exit", func() bool { return loopExited(c) }) {
		t.Fatal("loop goroutine still running after Stop")
	}
}

// ── (C) duplicate live connections ──

func TestSyncConfigChangeKeepsSingleLiveConnection(t *testing.T) {
	dialStarted := make(chan struct{})
	var startOnce sync.Once
	var requests int32

	u := newTestUpstream(t, testUpstreamOpts{
		beforeUpgrade: func() {
			startOnce.Do(func() { close(dialStarted) })
			// Keep the first two dials in flight long enough for Stop to land
			// inside a dial, and separate the dropped connection from its
			// replacement so the concurrency count is unambiguous.
			if atomic.AddInt32(&requests, 1) <= 2 {
				time.Sleep(250 * time.Millisecond)
			}
		},
	})

	logger := logs.NewLogger(2000)
	cfgMgr := newTestConfigManager(t)
	proc := newInertProcessor(t, cfgMgr, logger)

	setUpstream := func(name string) {
		t.Helper()
		if err := cfgMgr.Update(func(c *config.Config) {
			c.Upstreams = []config.UpstreamConfig{{
				ID: "up1", Name: name, WSUrl: u.WSURL(), WSKey: "k", Enabled: true,
			}}
		}); err != nil {
			t.Fatalf("config update: %v", err)
		}
	}

	mgr := NewManager(cfgMgr, logger, proc)
	setUpstream("one")
	mgr.Sync()
	defer mgr.StopAll()

	if !waitFor(t, 3*time.Second, "the first dial", func() bool {
		select {
		case <-dialStarted:
			return true
		default:
			return false
		}
	}) {
		t.Fatal("first dial never reached the upstream")
	}

	// Rename the upstream while it is enabled: Sync stops the old loop and
	// starts a new one, which used to leave two live connections.
	setUpstream("two")
	mgr.Sync()

	if !waitFor(t, 5*time.Second, "both dials to be accepted", func() bool { return u.Total() >= 2 }) {
		t.Fatalf("only %d dial(s) reached the upstream, want 2", u.Total())
	}
	// Give the replaced connection time to be observed as closed upstream.
	time.Sleep(300 * time.Millisecond)

	if got := u.Live(); got != 1 {
		t.Fatalf("%d live upstream connection(s) after the config change, want 1", got)
	}
	if got := u.MaxLive(); got != 1 {
		t.Fatalf("peak of %d concurrent live upstream connection(s), want 1 (double signals)", got)
	}

	// One broadcast must reach the processor exactly once.
	mark := time.Now()
	u.Broadcast(testSignal)
	time.Sleep(400 * time.Millisecond)
	if n := countSignalLogs(logger, mark); n != 1 {
		t.Fatalf("processor handled a single broadcast %d time(s), want 1", n)
	}
}

// TestSyncDisabledUpstreamDoesNotDial covers (C)(i): editing a disabled upstream
// must not start a connection.
func TestSyncDisabledUpstreamDoesNotDial(t *testing.T) {
	u := newTestUpstream(t, testUpstreamOpts{})
	logger := logs.NewLogger(1000)
	cfgMgr := newTestConfigManager(t)
	proc := newInertProcessor(t, cfgMgr, logger)

	update := func(name string, enabled bool) {
		t.Helper()
		if err := cfgMgr.Update(func(c *config.Config) {
			c.Upstreams = []config.UpstreamConfig{{
				ID: "up1", Name: name, WSUrl: u.WSURL(), Enabled: enabled,
			}}
		}); err != nil {
			t.Fatalf("config update: %v", err)
		}
	}

	mgr := NewManager(cfgMgr, logger, proc)
	update("off", false)
	mgr.Sync()
	defer mgr.StopAll()

	if got := u.Total(); got != 0 {
		t.Fatalf("disabled upstream dialled %d time(s), want 0", got)
	}

	// Editing a disabled upstream (name change => "changed" branch) must not dial.
	update("off-renamed", false)
	mgr.Sync()
	time.Sleep(300 * time.Millisecond)
	if got := u.Total(); got != 0 {
		t.Fatalf("renaming a disabled upstream dialled %d time(s), want 0", got)
	}

	// White-box check: the changed branch must not even create a loop for a
	// disabled upstream. Pre-fix it called Start unconditionally (creating a
	// context and a goroutine) and only the enable/disable guard below cancelled
	// it again, which is a spurious dial whenever the goroutine wins the race.
	mgr.mu.Lock()
	conn := mgr.conns["up1"]
	mgr.mu.Unlock()
	if conn == nil {
		t.Fatal("upstream not tracked by the manager")
	}
	conn.mu.Lock()
	started := conn.cancel != nil
	conn.mu.Unlock()
	if started {
		t.Fatal("renaming a disabled upstream created a loop goroutine")
	}

	// Enabling it starts exactly one connection.
	update("on", true)
	mgr.Sync()
	if !waitFor(t, 3*time.Second, "the enabled upstream to connect", func() bool { return u.Total() == 1 }) {
		t.Fatalf("%d connections after enabling, want 1", u.Total())
	}

	// Disabling it again stops it and leaves nothing live.
	update("off-again", false)
	mgr.Sync()
	if !waitFor(t, 2*time.Second, "the upstream to disconnect", func() bool { return u.Live() == 0 }) {
		t.Fatalf("%d connection(s) still live after disabling, want 0", u.Live())
	}
	if mgr.IsConnected() {
		t.Fatal("manager still reports a connected upstream after disabling")
	}
}

// ── (D) goroutine leak per reconnect ──

// TestNoGoroutineLeakPerReconnect models reconnects the way the loop does it: a
// single long-lived loop context with one short-lived connection after another.
// The ping goroutine is started per connection and must exit with it; watching
// c.ctx (never cancelled on a normal reconnect) leaked one goroutine per
// reconnect for up to a ping period.
func TestNoGoroutineLeakPerReconnect(t *testing.T) {
	u := newTestUpstream(t, testUpstreamOpts{})
	logger := logs.NewLogger(1000)
	cfgMgr := newTestConfigManager(t)
	proc := newInertProcessor(t, cfgMgr, logger)

	c := &Conn{ID: "leak", Name: "leak", URL: u.WSURL()}

	loopCtx, loopCancel := context.WithCancel(context.Background())
	defer loopCancel()

	// Warm up, then take the baseline so runtime/server goroutines have settled.
	for i := 0; i < 2; i++ {
		oneReadLoop(t, loopCtx, c, u, logger, proc)
	}
	time.Sleep(100 * time.Millisecond)
	base := runtime.NumGoroutine()

	for i := 0; i < 6; i++ {
		oneReadLoop(t, loopCtx, c, u, logger, proc)
	}
	if !waitFor(t, 4*time.Second, "ping goroutines to exit", func() bool {
		return runtime.NumGoroutine() <= base+3
	}) {
		t.Fatalf("goroutines leaked: baseline %d, now %d after 6 reconnects", base, runtime.NumGoroutine())
	}

	// Start/Stop cycles (reconnects driven by the loop) must not leak either.
	for i := 0; i < 3; i++ {
		c.Start(logger, proc)
		if !waitFor(t, 3*time.Second, "the connection to come up", c.IsConnected) {
			t.Fatal("connection never came up")
		}
		c.Stop()
		if !waitFor(t, 2*time.Second, "the loop goroutine to exit", func() bool { return loopExited(c) }) {
			t.Fatal("loop goroutine did not exit after Stop")
		}
	}
	if !waitFor(t, 4*time.Second, "goroutines to settle", func() bool {
		return runtime.NumGoroutine() <= base+3
	}) {
		t.Fatalf("goroutines leaked across start/stop cycles: baseline %d, now %d", base, runtime.NumGoroutine())
	}
}

// oneReadLoop runs a single connection through readLoop under parent (the loop
// context, which survives reconnects) and returns once readLoop has exited.
func oneReadLoop(t *testing.T, parent context.Context, c *Conn, u *testUpstream, logger *logs.Logger, proc *signals.Processor) {
	t.Helper()

	dialer := websocket.Dialer{HandshakeTimeout: 5 * time.Second}
	conn, _, err := dialer.DialContext(parent, u.WSURL(), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if u.Live() != 1 {
		t.Fatalf("%d live connections before readLoop, want 1", u.Live())
	}

	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer cancel()
		_ = c.readLoop(ctx, conn, logger, proc)
	}()

	// Drop the upstream side: readLoop must return, taking its ping goroutine
	// with it.
	u.CloseLiveConns()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("readLoop did not return after the upstream closed")
	}
	if !waitFor(t, 2*time.Second, "the upstream connection to be released", func() bool { return u.Live() == 0 }) {
		t.Fatalf("%d upstream connection(s) still live, want 0", u.Live())
	}
}
