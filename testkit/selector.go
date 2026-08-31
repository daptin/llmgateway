package testkit

import "sync"

// Selector returns a configured deterministic sequence. Values are normalized
// into the requested range, so tests can describe route decisions directly.
type Selector struct {
	mu     sync.Mutex
	values []int
	next   int
}

func NewSelector(values ...int) *Selector {
	return &Selector{values: append([]int(nil), values...)}
}

func (s *Selector) Intn(limit int) int {
	if limit <= 0 {
		panic("testkit.Selector.Intn called with non-positive limit")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.values) == 0 {
		return 0
	}
	value := s.values[s.next%len(s.values)]
	s.next++
	value %= limit
	if value < 0 {
		value += limit
	}
	return value
}
