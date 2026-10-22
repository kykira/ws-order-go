package orders

import (
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
