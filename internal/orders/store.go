// Package orders keeps a small ring of recent order attempts so the web panel
// can render them as a table instead of parsing raw log lines.
//
// Records are in-memory by default. NewPersistentStore additionally snapshots
// them to a JSON file (loaded on start, flushed after changes and on Close) so
// a restart does not lose the table.
package orders

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Status is the lifecycle state of one order attempt.
type Status string

const (
	StatusPending  Status = "pending"
	StatusRetrying Status = "retrying"
	StatusSuccess  Status = "success"
	StatusFailed   Status = "failed"
)

// Record describes one PlaceOrder call (retries are folded into Attempts; a
// fallback order on another account is a separate record).
type Record struct {
	ID         int64     `json:"id"`
	Time       time.Time `json:"time"`
	UpdatedAt  time.Time `json:"updatedAt"`
	DurationMS int64     `json:"durationMs"`
	Account    string    `json:"account"`
	TaskID     string    `json:"taskId"`
	Strategy   string    `json:"strategy"`
	Action     string    `json:"action"`
	Symbol     string    `json:"symbol"`
	Period     string    `json:"period"`
	Amount     string    `json:"amount"`
	Platform   string    `json:"platform"`
	Status     Status    `json:"status"`
	HTTPStatus int       `json:"httpStatus,omitempty"`
	Code       string    `json:"code,omitempty"`
	Message    string    `json:"message,omitempty"`
	Error      string    `json:"error,omitempty"`
	Attempts   int       `json:"attempts"`
	Source     string    `json:"source,omitempty"` // "" normal, "fallback", "test"
}

// diskFile is the on-disk envelope written to path.
type diskFile struct {
	SavedAt time.Time `json:"savedAt"`
	Records []Record  `json:"records"`
}

// Store is a bounded, concurrency-safe ring of order records.
type Store struct {
	mu   sync.Mutex
	max  int
	seq  int64
	recs []*Record

	// Persistence (path == "" means in-memory only).
	path          string
	dirty         bool
	flushInterval time.Duration
	stopCh        chan struct{}
	doneCh        chan struct{}
	closeOnce     sync.Once
}

// NewStore returns an in-memory store capped at max records (200 by default).
func NewStore(max int) *Store {
	if max <= 0 {
		max = 200
	}
	return &Store{max: max}
}

// NewPersistentStore loads previously saved records from path and flushes
// changes in the background. Loading problems are non-fatal: the store starts
// with an empty history and keeps working.
func NewPersistentStore(max int, path string) *Store {
	if path == "" {
		return NewStore(max)
	}
	s := NewStore(max)
	s.path = path
	s.flushInterval = time.Second
	s.load()
	s.stopCh = make(chan struct{})
	s.doneCh = make(chan struct{})
	go s.flushLoop()
	return s
}

// Close stops the background flusher and writes a final snapshot. It is safe to
// call more than once and safe on an in-memory store.
func (s *Store) Close() {
	if s == nil || s.stopCh == nil {
		return
	}
	s.closeOnce.Do(func() {
		close(s.stopCh)
		<-s.doneCh
		s.flush()
	})
}

// Begin creates a pending record and returns its id (0 for a nil store).
func (s *Store) Begin(fields Record) int64 {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	s.seq++
	now := time.Now()
	fields.ID = s.seq
	fields.Time = now
	fields.UpdatedAt = now
	fields.Status = StatusPending
	fields.DurationMS = 0

	s.recs = append(s.recs, &fields)
	if len(s.recs) > s.max {
		s.recs = s.recs[len(s.recs)-s.max:]
	}
	s.dirty = true
	return fields.ID
}

// Update applies fn to the record with the given id. Unknown ids are ignored.
func (s *Store) Update(id int64, fn func(*Record)) {
	if s == nil || id == 0 || fn == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	for i := len(s.recs) - 1; i >= 0; i-- {
		if s.recs[i].ID == id {
			fn(s.recs[i])
			s.recs[i].UpdatedAt = time.Now()
			s.recs[i].DurationMS = s.recs[i].UpdatedAt.Sub(s.recs[i].Time).Milliseconds()
			s.dirty = true
			return
		}
	}
}

// List returns up to limit records, newest first (limit <= 0 means all).
func (s *Store) List(limit int) []Record {
	if s == nil {
		return []Record{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	n := len(s.recs)
	if limit > 0 && limit < n {
		n = limit
	}
	out := make([]Record, 0, n)
	for i := len(s.recs) - 1; i >= 0 && len(out) < n; i-- {
		out = append(out, *s.recs[i])
	}
	return out
}

// ── persistence ──

func (s *Store) load() {
	bs, err := os.ReadFile(s.path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("orders: load %s: %v", s.path, err)
		}
		return
	}

	var f diskFile
	if err := json.Unmarshal(bs, &f); err != nil {
		log.Printf("orders: parse %s: %v (starting with an empty history)", s.path, err)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	recs := f.Records
	if len(recs) > s.max {
		recs = recs[len(recs)-s.max:]
	}
	s.recs = make([]*Record, 0, len(recs))
	for i := range recs {
		r := recs[i]
		if r.ID > s.seq {
			s.seq = r.ID
		}
		s.recs = append(s.recs, &r)
	}
}

func (s *Store) flushLoop() {
	defer close(s.doneCh)
	ticker := time.NewTicker(s.flushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.stopCh:
			return
		case <-ticker.C:
			s.flush()
		}
	}
}

// snapshot marshals a consistent copy and clears the dirty flag. The caller
// writes the bytes outside the lock.
func (s *Store) snapshot() ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.dirty {
		return nil, false
	}

	recs := make([]Record, 0, len(s.recs))
	for _, r := range s.recs {
		recs = append(recs, *r)
	}
	bs, err := json.Marshal(diskFile{SavedAt: time.Now(), Records: recs})
	if err != nil {
		return nil, false
	}
	s.dirty = false
	return bs, true
}

func (s *Store) flush() {
	bs, ok := s.snapshot()
	if !ok {
		return
	}
	if err := writeAtomic(s.path, bs); err != nil {
		s.mu.Lock()
		s.dirty = true
		s.mu.Unlock()
		log.Printf("orders: save %s: %v", s.path, err)
	}
}

// writeAtomic writes through a temp file + rename, falling back to an in-place
// write when the target is a single-file bind mount (rename returns EBUSY).
func writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}

	tmp, err := os.CreateTemp(dir, ".orders-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }() // no-op once the rename succeeded

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(name, 0644); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return os.WriteFile(path, data, 0644)
	}
	return nil
}
