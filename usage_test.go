package llmgateway

import (
	"math"
	"testing"

	"github.com/daptin/llmgateway/catalog"
	"github.com/daptin/llmgateway/contract"
)

func TestSettledUsageUsesConservativeTotals(t *testing.T) {
	usage, err := settledUsage(contract.Usage{InputTokens: 7, OutputTokens: 5, TotalTokens: 1}, contract.Usage{}, catalog.Pricing{})
	if err != nil {
		t.Fatal(err)
	}
	if usage.TotalTokens != 12 || usage.Estimated {
		t.Fatalf("usage = %+v", usage)
	}
}

func TestSettledUsageRejectsTokenOverflow(t *testing.T) {
	_, err := settledUsage(contract.Usage{InputTokens: math.MaxInt64, OutputTokens: 1}, contract.Usage{}, catalog.Pricing{})
	if err == nil {
		t.Fatal("expected token overflow rejection")
	}
}
