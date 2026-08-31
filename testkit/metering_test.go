package testkit

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/daptin/llmgateway/contract"
)

func TestMeteringLifecycleIsObservableAndTerminalizationIdempotent(t *testing.T) {
	store := NewMeteringRecorder()
	now := time.Now()
	token, err := store.Admit(context.Background(), contract.Admission{RequestID: "r1", StartedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Admit(context.Background(), contract.Admission{RequestID: "r1", StartedAt: now})
	if !errors.Is(err, errDuplicateAdmission) {
		t.Fatalf("expected duplicate error, got %v", err)
	}
	completion := contract.Completion{Token: token, Status: "succeeded", Usage: contract.Usage{CostMicros: 2}, EndedAt: now.Add(time.Second)}
	if err := store.Complete(context.Background(), completion); err != nil {
		t.Fatal(err)
	}
	if err := store.Complete(context.Background(), completion); err != nil {
		t.Fatalf("idempotent finalize failed: %v", err)
	}
	if store.State("r1") != "finalized" {
		t.Fatalf("state=%s", store.State("r1"))
	}
}

func TestMeteringRecorderForwardsHostDecisionWithoutEvaluatingPolicy(t *testing.T) {
	store := NewMeteringRecorder()
	denied := errors.New("host denied admission")
	store.RejectAdmissions(denied)
	admission := contract.Admission{RequestID: "r1", ModelID: "m", EstimatedUsage: contract.Usage{TotalTokens: 7}}
	if _, err := store.Admit(context.Background(), admission); !errors.Is(err, denied) {
		t.Fatalf("admission error=%v", err)
	}
	got := store.Admissions()
	if len(got) != 1 || got[0].ModelID != "m" || got[0].EstimatedUsage.TotalTokens != 7 {
		t.Fatalf("admissions=%+v", got)
	}
}
