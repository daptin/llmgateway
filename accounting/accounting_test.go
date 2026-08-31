package accounting

import (
	"math"
	"testing"

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
