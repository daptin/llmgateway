package llmgateway

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// FallbackCounterStore keeps disposable deployment protection available when
// distributed coordination fails. Durable budgets remain the responsibility
// of AccountingStore and never use this fallback.
type FallbackCounterStore struct {
	primary  CounterStore
	fallback CounterStore
	onError  func(error)
}

func NewFallbackCounterStore(primary, fallback CounterStore, onError func(error)) (*FallbackCounterStore, error) {
	if primary == nil || fallback == nil {
		return nil, errors.New("primary and fallback counter stores are required")
	}
	return &FallbackCounterStore{primary: primary, fallback: fallback, onError: onError}, nil
}

func (s FallbackCounterStore) Add(ctx context.Context, key string, delta int64, ttl time.Duration) (int64, error) {
	value, err := s.primary.Add(ctx, key, delta, ttl)
	if err == nil || errors.Is(err, ErrCounterLimit) {
		return value, err
	}
	s.report(err)
	return s.fallback.Add(ctx, key, delta, ttl)
}

func (s FallbackCounterStore) Get(ctx context.Context, key string) (int64, bool, error) {
	value, found, err := s.primary.Get(ctx, key)
	if err == nil {
		return value, found, nil
	}
	s.report(err)
	return s.fallback.Get(ctx, key)
}

func (s FallbackCounterStore) Acquire(ctx context.Context, key string, maximum int64, ttl time.Duration) (string, error) {
	token, err := s.primary.Acquire(ctx, key, maximum, ttl)
	if err == nil {
		return "primary:" + token, nil
	}
	if errors.Is(err, ErrCounterLimit) {
		return "", err
	}
	s.report(err)
	token, err = s.fallback.Acquire(ctx, key, maximum, ttl)
	if err != nil {
		return "", err
	}
	return "fallback:" + token, nil
}

func (s FallbackCounterStore) Release(ctx context.Context, token string) error {
	switch {
	case strings.HasPrefix(token, "primary:"):
		return s.primary.Release(ctx, strings.TrimPrefix(token, "primary:"))
	case strings.HasPrefix(token, "fallback:"):
		return s.fallback.Release(ctx, strings.TrimPrefix(token, "fallback:"))
	case token == "":
		return nil
	default:
		return errors.New("invalid fallback counter lease")
	}
}

func (s FallbackCounterStore) Delete(ctx context.Context, key string) error {
	err := s.primary.Delete(ctx, key)
	if err != nil {
		s.report(err)
	}
	fallbackErr := s.fallback.Delete(ctx, key)
	if fallbackErr != nil {
		return fallbackErr
	}
	return nil
}

func (s FallbackCounterStore) report(err error) {
	if s.onError != nil {
		s.onError(err)
	}
}

// LocalCounterStore is a bounded-process fallback for transient counters and
// leases. Expired entries are discarded opportunistically on access.
type LocalCounterStore struct {
	mu       sync.Mutex
	values   map[string]localCounter
	leases   map[string]localLease
	sequence atomic.Uint64
	now      func() time.Time
}

type localCounter struct {
	value     int64
	expiresAt time.Time
}

type localLease struct {
	key       string
	expiresAt time.Time
}

func NewLocalCounterStore() *LocalCounterStore {
	return &LocalCounterStore{values: make(map[string]localCounter), leases: make(map[string]localLease), now: time.Now}
}

func (s *LocalCounterStore) Add(ctx context.Context, key string, delta int64, ttl time.Duration) (int64, error) {
	if err := validateLocalCounter(ctx, key, ttl); err != nil {
		return 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.cleanupExpiredLeases(now)
	entry := s.values[key]
	if !entry.expiresAt.After(now) {
		entry = localCounter{expiresAt: now.Add(ttl)}
	}
	entry.value += delta
	s.values[key] = entry
	return entry.value, nil
}

func (s *LocalCounterStore) Get(ctx context.Context, key string) (int64, bool, error) {
	if err := ctx.Err(); err != nil {
		return 0, false, err
	}
	if key == "" {
		return 0, false, errors.New("counter key is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, found := s.values[key]
	if found && !entry.expiresAt.After(s.now()) {
		delete(s.values, key)
		found = false
	}
	return entry.value, found, nil
}

func (s *LocalCounterStore) Acquire(ctx context.Context, key string, maximum int64, ttl time.Duration) (string, error) {
	if maximum < 0 {
		return "", errors.New("counter maximum cannot be negative")
	}
	if err := validateLocalCounter(ctx, key, ttl); err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.cleanupExpiredLeases(now)
	entry := s.values[key]
	if !entry.expiresAt.After(now) {
		entry = localCounter{expiresAt: now.Add(ttl)}
	}
	if entry.value >= maximum {
		return "", ErrCounterLimit
	}
	entry.value++
	entry.expiresAt = now.Add(ttl)
	s.values[key] = entry
	token := fmt.Sprintf("lease-%d", s.sequence.Add(1))
	s.leases[token] = localLease{key: key, expiresAt: entry.expiresAt}
	return token, nil
}

func (s *LocalCounterStore) Release(ctx context.Context, token string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if token == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.cleanupExpiredLeases(now)
	lease, found := s.leases[token]
	if !found {
		return nil
	}
	delete(s.leases, token)
	entry, found := s.values[lease.key]
	if found && entry.expiresAt.After(now) && entry.value > 0 {
		entry.value--
		s.values[lease.key] = entry
	}
	return nil
}

func (s *LocalCounterStore) cleanupExpiredLeases(now time.Time) {
	for token, lease := range s.leases {
		if !lease.expiresAt.After(now) {
			delete(s.leases, token)
		}
	}
}

func (s *LocalCounterStore) Delete(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	delete(s.values, key)
	s.mu.Unlock()
	return nil
}

func validateLocalCounter(ctx context.Context, key string, ttl time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if key == "" || ttl <= 0 {
		return errors.New("counter key and positive TTL are required")
	}
	return nil
}
