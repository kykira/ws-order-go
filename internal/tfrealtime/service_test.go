package tfrealtime

import (
	"net/url"
	"strings"
	"testing"
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
