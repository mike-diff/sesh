package main

import (
	"sort"
	"sync"
	"time"
)

// Ticket is the unit of work. Closed tickets keep their row forever; the
// service has no delete, only Close, so history is append-only by design.
type Ticket struct {
	ID       int64      `json:"id"`
	Title    string     `json:"title"`
	Body     string     `json:"body"`
	Priority int        `json:"priority"`
	Created  time.Time  `json:"created"`
	Closed   *time.Time `json:"closed,omitempty"`
}

// Store is an in-memory ticket table guarded by one RWMutex. Reads take the
// read lock and copy out; writes take the write lock. Nothing escapes by
// pointer, so callers can never mutate a row without the lock.
type Store struct {
	mu     sync.RWMutex
	rows   map[int64]Ticket
	nextID int64
}

func NewStore() *Store {
	return &Store{rows: make(map[int64]Ticket), nextID: 1}
}

func (s *Store) Create(title, body string, priority int) Ticket {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := Ticket{ID: s.nextID, Title: title, Body: body, Priority: priority, Created: time.Now()}
	s.rows[t.ID] = t
	s.nextID++
	return t
}

func (s *Store) Get(id int64) (Ticket, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	t, ok := s.rows[id]
	return t, ok
}

// List returns tickets after the cursor id, oldest first, plus the next
// cursor (0 when the listing is exhausted). Cursor pagination keeps the
// results stable under concurrent inserts, unlike offset pagination.
func (s *Store) List(cursor int64, limit int) ([]Ticket, int64) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ids := make([]int64, 0, len(s.rows))
	for id := range s.rows {
		if id > cursor {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	if len(ids) > limit {
		ids = ids[:limit]
	}
	out := make([]Ticket, len(ids))
	for i, id := range ids {
		out[i] = s.rows[id]
	}
	var next int64
	if len(ids) == limit && limit > 0 {
		next = ids[len(ids)-1]
	}
	return out, next
}

func (s *Store) Close(id int64, at time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.rows[id]
	if !ok || t.Closed != nil {
		return false
	}
	t.Closed = &at
	s.rows[id] = t
	return true
}
