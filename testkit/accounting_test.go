package testkit

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/daptin/llmgateway/accounting"
	"github.com/daptin/llmgateway/contract"
)

func TestAccountingAdmissionIsAtomicAndTerminalizationIdempotent(t *testing.T) {
	store := NewAccountingStore()
	now := time.Now()
	reservation := contract.LimitReservation{ScopeKind: "key", ScopeID: "k", PolicyID: "p", Metric: "cost_micros", Window: "1m", WindowStart: now.Truncate(time.Minute), WindowEnd: now.Truncate(time.Minute).Add(time.Minute), Maximum: 10, Amount: 7}
	token, err := store.Admit(context.Background(), contract.Admission{RequestID: "r1", StartedAt: now, LimitReservations: []contract.LimitReservation{reservation}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Admit(context.Background(), contract.Admission{RequestID: "r2", StartedAt: now, LimitReservations: []contract.LimitReservation{reservation}})
	if !errors.Is(err, accounting.ErrLimitExceeded) {
		t.Fatalf("expected limit error, got %v", err)
	}
	completion := contract.Completion{Token: token, Status: "succeeded", Usage: contract.Usage{CostMicros: 2}, EndedAt: now.Add(time.Second)}
	if err := store.Finalize(context.Background(), completion); err != nil {
		t.Fatal(err)
	}
	if err := store.Finalize(context.Background(), completion); err != nil {
		t.Fatalf("idempotent finalize failed: %v", err)
	}
	if store.State("r1") != "finalized" {
		t.Fatalf("state=%s", store.State("r1"))
	}
	if _, err := store.Admit(context.Background(), contract.Admission{RequestID: "r2", StartedAt: now, LimitReservations: []contract.LimitReservation{reservation}}); err != nil {
		t.Fatalf("released reservation was not available: %v", err)
	}
}

func TestAccountingReapsExpiredHold(t *testing.T) {
	store := NewAccountingStore()
	now := time.Now()
	_, err := store.Admit(context.Background(), contract.Admission{RequestID: "r", StartedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	result, err := store.ReapExpired(context.Background(), now.Add(2*time.Minute), 10)
	if err != nil {
		t.Fatal(err)
	}
	if result.Released != 1 || store.State("r") != "cancelled" {
		t.Fatalf("result=%+v state=%s", result, store.State("r"))
	}
}
