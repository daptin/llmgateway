package accounting

import (
	"math"
	"testing"
	"time"

	"github.com/daptin/llmgateway/catalog"
	"github.com/daptin/llmgateway/contract"
)

func TestCostMicrosRoundsEachComponentUp(t *testing.T) {
	cost, err := CostMicros(contract.Usage{InputTokens: 1, OutputTokens: 1}, catalog.Pricing{InputMicrosPerMillion: 1, OutputMicrosPerMillion: 1})
	if err != nil {
		t.Fatal(err)
	}
	if cost != 2 {
		t.Fatalf("cost=%d want=2", cost)
	}
}

func TestCostMicrosRejectsOverflow(t *testing.T) {
	_, err := CostMicros(contract.Usage{InputTokens: math.MaxInt64}, catalog.Pricing{InputMicrosPerMillion: math.MaxInt64})
	if err != ErrCostOverflow {
		t.Fatalf("expected ErrCostOverflow, got %v", err)
	}
}

func TestReservationsBuildCanonicalWindows(t *testing.T) {
	now := time.Date(2026, 8, 31, 12, 34, 56, 0, time.FixedZone("test", 19800))
	bindings := []Binding{{
		ScopeKind: "key", ScopeID: "key-1",
		Policy: catalog.Policy{ID: "policy-1", Limits: []catalog.Limit{
			{Metric: "requests", Window: "1m", Maximum: 10, Mode: "hard"},
			{Metric: "cost_micros", Window: "calendar_month", Maximum: 100, Mode: "hard"},
			{Metric: "concurrency", Maximum: 2, Mode: "hard"},
		}},
	}}
	reservations, err := Reservations(bindings, Measures{Requests: 1, CostMicros: 7, Concurrency: 1}, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(reservations) != 3 {
		t.Fatalf("got %d reservations", len(reservations))
	}
	if got := reservations[0].WindowStart; !got.Equal(time.Date(2026, 8, 31, 7, 4, 0, 0, time.UTC)) {
		t.Fatalf("minute window start=%s", got)
	}
	if got := reservations[1].WindowStart; !got.Equal(time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("month window start=%s", got)
	}
	if !reservations[2].WindowStart.IsZero() || !reservations[2].WindowEnd.IsZero() {
		t.Fatal("concurrency reservation must not use a time window")
	}
}

func TestReservationsRejectDuplicateEffectiveLimit(t *testing.T) {
	policy := catalog.Policy{ID: "p", Limits: []catalog.Limit{{Metric: "requests", Window: "1m", Maximum: 1, Mode: "hard"}}}
	bindings := []Binding{{ScopeKind: "key", ScopeID: "k", Policy: policy}, {ScopeKind: "key", ScopeID: "k", Policy: policy}}
	if _, err := Reservations(bindings, Measures{Requests: 1}, time.Now()); err == nil {
		t.Fatal("expected duplicate effective limit error")
	}
}
