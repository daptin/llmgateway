package testkit

import (
	"context"
	"sync"
	"time"
)

type ResponseCache struct {
	mu      sync.Mutex
	now     func() time.Time
	entries map[string]cacheEntry
	failure error
}

type cacheEntry struct {
	payload   []byte
	expiresAt time.Time
}

func NewResponseCache(now func() time.Time) *ResponseCache {
	return &ResponseCache{now: now, entries: make(map[string]cacheEntry)}
}

func (c *ResponseCache) SetFailure(err error) {
	c.mu.Lock()
	c.failure = err
	c.mu.Unlock()
}

func (c *ResponseCache) Get(ctx context.Context, key string) ([]byte, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.failure != nil {
		return nil, false, c.failure
	}
	entry, ok := c.entries[key]
	if ok && !entry.expiresAt.After(c.now()) {
		delete(c.entries, key)
		ok = false
	}
	return append([]byte(nil), entry.payload...), ok, nil
}

func (c *ResponseCache) Set(ctx context.Context, key string, payload []byte, ttl time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.failure != nil {
		return c.failure
	}
	c.entries[key] = cacheEntry{payload: append([]byte(nil), payload...), expiresAt: c.now().Add(ttl)}
	return nil
}

func (c *ResponseCache) Delete(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.failure != nil {
		return c.failure
	}
	delete(c.entries, key)
	return nil
}
