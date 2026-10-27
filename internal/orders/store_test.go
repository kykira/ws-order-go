package orders

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestStoreLifecycle(t *testing.T) {
	s := NewStore(3)

	id1 := s.Begin(Record{Account: "a", Amount: "1"})
	id2 := s.Begin(Record{Account: "b"})
	if id1 == 0 || id2 != id1+1 {
		t.Fatalf("ids = %d, %d; want consecutive non-zero ids", id1, id2)
	}

	s.Update(id1, func(r *Record) {
		r.Status = StatusSuccess
		r.Code = "0"
		r.Message = "ok"
		r.Attempts = 1
		r.HTTPStatus = 200
	})

	list := s.List(10)
	if len(list) != 2 {
		t.Fatalf("List = %d records, want 2", len(list))
	}
	// Newest first.
	if list[0].ID != id2 || list[0].Account != "b" {
		t.Fatalf("newest record = %+v, want id=%d account=b", list[0], id2)
	}
	got := list[1]
	if got.Status != StatusSuccess || got.Code != "0" || got.Message != "ok" ||
		got.Attempts != 1 || got.HTTPStatus != 200 {
		t.Fatalf("updated record = %+v", got)
	}
	if got.DurationMS < 0 {
		t.Fatalf("DurationMS = %d, want >= 0", got.DurationMS)
	}

	// Ring cap: a is dropped once a fourth record is added.
	s.Begin(Record{Account: "c"})
	s.Begin(Record{Account: "d"})
	list = s.List(10)
	if len(list) != 3 {
		t.Fatalf("List after cap = %d records, want 3", len(list))
	}
	if list[0].Account != "d" || list[2].Account != "b" {
		t.Fatalf("ring contents = %v, want [d c b]", []string{list[0].Account, list[1].Account, list[2].Account})
	}
}

func TestStoreNilAndUnknown(t *testing.T) {
	var s *Store
	if id := s.Begin(Record{}); id != 0 {
		t.Fatalf("nil store Begin = %d, want 0", id)
	}
	s.Update(42, func(*Record) { t.Fatal("nil store must not call fn") })
	if got := s.List(10); got == nil || len(got) != 0 {
		t.Fatalf("nil store List = %v, want empty slice", got)
	}

	real := NewStore(2)
	real.Update(999, func(*Record) { t.Fatal("unknown id must not call fn") })
}

func TestStoreConcurrent(t *testing.T) {
	s := NewStore(100)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				id := s.Begin(Record{Account: "a"})
				s.Update(id, func(r *Record) {
					r.Status = StatusSuccess
					r.Attempts = 1
				})
				_ = s.List(10)
			}
		}()
	}
	wg.Wait()

	if got := len(s.List(1000)); got != 100 {
		t.Fatalf("records = %d, want the ring cap 100", got)
	}
}

// ── persistence ──

func TestPersistentStoreRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "orders.json")

	s := NewPersistentStore(10, path)
	id := s.Begin(Record{Account: "a", Amount: "1", Strategy: "st"})
	s.Update(id, func(r *Record) {
		r.Status = StatusSuccess
		r.Code = "0"
		r.Message = "ok"
		r.Attempts = 1
	})
	s.Begin(Record{Account: "b"})
	s.Close()

	s2 := NewPersistentStore(10, path)
	defer s2.Close()

	list := s2.List(10)
	if len(list) != 2 {
		t.Fatalf("reloaded %d records, want 2", len(list))
	}
	if list[0].Account != "b" || list[0].Status != StatusPending {
		t.Fatalf("newest = %+v, want pending b", list[0])
	}
	if list[1].Account != "a" || list[1].Status != StatusSuccess || list[1].Code != "0" {
		t.Fatalf("older = %+v, want successful a", list[1])
	}

	// The id sequence must continue after a restart.
	if next := s2.Begin(Record{Account: "c"}); next <= id {
		t.Fatalf("id after reload = %d, want > %d", next, id)
	}
}

func TestPersistentStoreTrimsToMax(t *testing.T) {
	path := filepath.Join(t.TempDir(), "orders.json")

	s := NewPersistentStore(2, path)
	for i := 0; i < 5; i++ {
		s.Begin(Record{Account: fmt.Sprintf("a%d", i)})
	}
	s.Close()

	s2 := NewPersistentStore(2, path)
	defer s2.Close()

	list := s2.List(10)
	if len(list) != 2 {
		t.Fatalf("reloaded %d records, want the cap 2", len(list))
	}
	if list[0].Account != "a4" || list[1].Account != "a3" {
		t.Fatalf("reloaded %q/%q, want a4/a3", list[0].Account, list[1].Account)
	}
}

func TestPersistentStoreCorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "orders.json")
	if err := os.WriteFile(path, []byte("{not json"), 0644); err != nil {
		t.Fatal(err)
	}

	s := NewPersistentStore(5, path)
	if got := len(s.List(10)); got != 0 {
		t.Fatalf("corrupt file loaded %d records, want 0", got)
	}
	s.Begin(Record{Account: "x"})
	s.Close()

	s2 := NewPersistentStore(5, path)
	defer s2.Close()
	if got := len(s2.List(10)); got != 1 {
		t.Fatalf("after recovery got %d records, want 1", got)
	}
}

func TestPersistentStoreCloseIsSafe(t *testing.T) {
	var nilStore *Store
	nilStore.Close()

	mem := NewStore(5)
	mem.Close() // in-memory store: no-op

	s := NewPersistentStore(5, filepath.Join(t.TempDir(), "orders.json"))
	s.Close()
	s.Close() // idempotent
}

// The production cap is 200: neither memory nor the JSON file may grow past it.
func TestPersistentStoreKeepsAtMost200(t *testing.T) {
	path := filepath.Join(t.TempDir(), "orders.json")

	s := NewPersistentStore(200, path)
	for i := 0; i < 250; i++ {
		s.Begin(Record{Account: fmt.Sprintf("a%d", i)})
	}
	s.Close()

	s2 := NewPersistentStore(200, path)
	defer s2.Close()

	list := s2.List(1000)
	if len(list) != 200 {
		t.Fatalf("kept %d records, want 200", len(list))
	}
	if list[0].Account != "a249" || list[199].Account != "a50" {
		t.Fatalf("kept %q..%q, want a249..a50", list[0].Account, list[199].Account)
	}
}
