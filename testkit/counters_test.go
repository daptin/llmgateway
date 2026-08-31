package testkit

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/daptin/llmgateway"
)

func TestCounterStoreFixedExpiryAndLeases(t *testing.T) {
	clock := NewClock(time.Unix(0, 0))
	store := NewCounterStore(clock.Now)
	if value, err := store.Add(context.Background(), "rpm", 1, time.Minute); err != nil || value != 1 {
		t.Fatalf("value=%d err=%v", value, err)
	}
	clock.Advance(30 * time.Second)
	if value, err := store.Add(context.Background(), "rpm", 1, time.Minute); err != nil || value != 2 {
		t.Fatalf("value=%d err=%v", value, err)
	}
	clock.Advance(30 * time.Second)
	if value, exists, err := store.Get(context.Background(), "rpm"); err != nil || exists || value != 0 {
		t.Fatalf("counter expiration extended unexpectedly: value=%d exists=%v err=%v", value, exists, err)
	}
	lease, err := store.Acquire(context.Background(), "concurrency", 1, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Acquire(context.Background(), "concurrency", 1, time.Minute); !errors.Is(err, llmgateway.ErrCounterLimit) {
		t.Fatalf("expected limit error, got %v", err)
	}
	if err := store.Release(context.Background(), lease); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Acquire(context.Background(), "concurrency", 1, time.Minute); err != nil {
		t.Fatalf("lease was not released: %v", err)
	}
}
