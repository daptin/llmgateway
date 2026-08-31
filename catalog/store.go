package catalog

import (
	"errors"
	"sync/atomic"
)

var ErrStaleRevision = errors.New("catalog revision is not newer than the active revision")

type Store struct {
	active atomic.Pointer[Snapshot]
}

func (s *Store) Current() (*Snapshot, bool) {
	current := s.active.Load()
	return current, current != nil
}

func (s *Store) Swap(next *Snapshot) error {
	if next == nil {
		return errors.New("catalog snapshot is nil")
	}
	for {
		current := s.active.Load()
		if current != nil && next.Revision() <= current.Revision() {
			return ErrStaleRevision
		}
		if s.active.CompareAndSwap(current, next) {
			return nil
		}
	}
}
