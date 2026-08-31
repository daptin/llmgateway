package testkit

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/daptin/llmgateway"
)

type CounterStore struct {
	mu      sync.Mutex
	now     func() time.Time
	next    uint64
	values  map[string]counterValue
	leases  map[string]string
	failure error
}

type counterValue struct {
	value     int64
	expiresAt time.Time
}

func NewCounterStore(now func() time.Time) *CounterStore {
	return &CounterStore{now: now, values: make(map[string]counterValue), leases: make(map[string]string)}
}

func (s *CounterStore) SetFailure(err error) {
	s.mu.Lock()
	s.failure = err
	s.mu.Unlock()
}

func (s *CounterStore) Add(ctx context.Context, key string, amount int64, ttl time.Duration) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failure != nil {
		return 0, s.failure
	}
	value := s.currentLocked(key)
	value.value += amount
	if value.expiresAt.IsZero() {
		value.expiresAt = s.now().Add(ttl)
	}
	s.values[key] = value
	return value.value, nil
}

func (s *CounterStore) Get(ctx context.Context, key string) (int64, bool, error) {
	if err := ctx.Err(); err != nil {
		return 0, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failure != nil {
		return 0, false, s.failure
	}
	value := s.currentLocked(key)
	return value.value, !value.expiresAt.IsZero(), nil
}

func (s *CounterStore) Acquire(ctx context.Context, key string, maximum int64, ttl time.Duration) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failure != nil {
		return "", s.failure
	}
	value := s.currentLocked(key)
	if value.value >= maximum {
		return "", llmgateway.ErrCounterLimit
	}
	s.next++
	token := fmt.Sprintf("lease-%d", s.next)
	value.value++
	if value.expiresAt.IsZero() {
		value.expiresAt = s.now().Add(ttl)
	}
	s.values[key] = value
	s.leases[token] = key
	return token, nil
}

func (s *CounterStore) Release(ctx context.Context, token string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failure != nil {
		return s.failure
	}
	key, ok := s.leases[token]
	if !ok {
		return nil
	}
	delete(s.leases, token)
	value := s.currentLocked(key)
	if value.value > 0 {
		value.value--
	}
	if value.value == 0 {
		delete(s.values, key)
	} else {
		s.values[key] = value
	}
	return nil
}

func (s *CounterStore) Delete(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failure != nil {
		return s.failure
	}
	delete(s.values, key)
	return nil
}

func (s *CounterStore) currentLocked(key string) counterValue {
	value := s.values[key]
	if !value.expiresAt.IsZero() && !value.expiresAt.After(s.now()) {
		delete(s.values, key)
		return counterValue{}
	}
	return value
}
