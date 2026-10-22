// Package orders keeps a small in-memory ring of recent order attempts so the
// web panel can render them as a table instead of parsing raw log lines.
package orders

import (
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

// Store is a bounded, concurrency-safe ring of order records.
type Store struct {
	mu   sync.Mutex
	max  int
	seq  int64
	recs []*Record
}

func NewStore(max int) *Store {
	if max <= 0 {
		max = 500
	}
	return &Store{max: max}
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
