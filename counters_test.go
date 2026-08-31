package llmgateway_test

import (
	"context"
	"errors"
	"testing"
	"time"

	llmgateway "github.com/daptin/llmgateway"
	"github.com/daptin/llmgateway/testkit"
)

func TestFallbackCounterStoreUsesLocalProtectionOnPrimaryFailure(t *testing.T) {
	primary := testkit.NewCounterStore(time.Now)
	primary.SetFailure(errors.New("distributed store unavailable"))
	local := llmgateway.NewLocalCounterStore()
	reported := 0
	store, err := llmgateway.NewFallbackCounterStore(primary, local, func(error) { reported++ })
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if value, err := store.Add(ctx, "rpm", 1, time.Minute); err != nil || value != 1 {
		t.Fatalf("value=%d err=%v", value, err)
	}
	lease, err := store.Acquire(ctx, "concurrency", 1, time.Minute)
	if err != nil || lease == "" {
		t.Fatalf("lease=%q err=%v", lease, err)
	}
	if _, err := store.Acquire(ctx, "concurrency", 1, time.Minute); !errors.Is(err, llmgateway.ErrCounterLimit) {
		t.Fatalf("expected conservative local limit, got %v", err)
	}
	if err := store.Release(ctx, lease); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Acquire(ctx, "concurrency", 1, time.Minute); err != nil {
		t.Fatalf("released fallback lease remained held: %v", err)
	}
	if reported < 3 {
		t.Fatalf("primary failures reported=%d", reported)
	}
}

func TestFallbackCounterStoreDoesNotWidenPrimaryLimit(t *testing.T) {
	primary := testkit.NewCounterStore(time.Now)
	store, err := llmgateway.NewFallbackCounterStore(primary, llmgateway.NewLocalCounterStore(), nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := store.Acquire(ctx, "concurrency", 1, time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Acquire(ctx, "concurrency", 1, time.Minute); !errors.Is(err, llmgateway.ErrCounterLimit) {
		t.Fatalf("primary hard limit was bypassed: %v", err)
	}
}
