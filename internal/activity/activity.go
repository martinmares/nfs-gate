package activity

import (
	"sync"
	"time"
)

type Event struct {
	Time      time.Time
	Operation string
	Path      string
	Error     string
}

type Store struct {
	mu     sync.Mutex
	events []Event
	next   int
	full   bool
	counts map[string]uint64
	errors uint64
}

func New(capacity int) *Store {
	if capacity < 1 {
		capacity = 1
	}
	return &Store{events: make([]Event, capacity), counts: make(map[string]uint64)}
}

func (s *Store) Add(operation, path string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := Event{Time: time.Now(), Operation: operation, Path: path}
	if err != nil {
		e.Error = err.Error()
		s.errors++
	}
	s.events[s.next] = e
	s.next = (s.next + 1) % len(s.events)
	if s.next == 0 {
		s.full = true
	}
	s.counts[operation]++
}

func (s *Store) Snapshot() ([]Event, map[string]uint64, uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := s.next
	if s.full {
		n = len(s.events)
	}
	events := make([]Event, 0, n)
	for i := 0; i < n; i++ {
		index := (s.next - 1 - i + len(s.events)) % len(s.events)
		events = append(events, s.events[index])
	}
	counts := make(map[string]uint64, len(s.counts))
	for k, v := range s.counts {
		counts[k] = v
	}
	return events, counts, s.errors
}
