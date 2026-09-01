package accounting

import (
	"math"
	"testing"

	"github.com/daptin/llmgateway/catalog"
	"github.com/daptin/llmgateway/contract"
)

func TestCostMicrosRoundsEachComponentUp(t *testing.T) {
	cost, err := CostMicros(contract.Usage{InputTokens: 1, OutputTokens: 1}, catalog.Pricing{Rates: map[string]int64{"input_tokens": 1, "output_tokens": 1}})
	if err != nil {
		t.Fatal(err)
	}
	if cost != 2 {
		t.Fatalf("cost=%d want=2", cost)
	}
}

func TestCostMicrosRejectsOverflow(t *testing.T) {
	_, err := CostMicros(contract.Usage{InputTokens: math.MaxInt64}, catalog.Pricing{Rates: map[string]int64{"input_tokens": math.MaxInt64}})
	if err != ErrCostOverflow {
		t.Fatalf("expected ErrCostOverflow, got %v", err)
	}
}

func FuzzCostMicrosMatchesUnsignedReference(f *testing.F) {
	f.Add(uint32(1), uint32(1), uint32(0), uint32(0), uint32(0), uint32(0), uint32(0), uint32(0), uint32(0), uint32(0))
	f.Add(^uint32(0), ^uint32(0), ^uint32(0), ^uint32(0), ^uint32(0), ^uint32(0), ^uint32(0), ^uint32(0), ^uint32(0), ^uint32(0))
	f.Fuzz(func(t *testing.T, inputTokens, inputRate, outputTokens, outputRate, cacheReadTokens, cacheReadRate,
		cacheWriteTokens, cacheWriteRate, reasoningTokens, reasoningRate uint32) {
		usage := contract.Usage{
			InputTokens: int64(inputTokens), OutputTokens: int64(outputTokens), CacheReadTokens: int64(cacheReadTokens),
			CacheWriteTokens: int64(cacheWriteTokens), ReasoningTokens: int64(reasoningTokens),
		}
		pricing := catalog.Pricing{Rates: map[string]int64{
			"input_tokens": int64(inputRate), "output_tokens": int64(outputRate), "cache_read_tokens": int64(cacheReadRate),
			"cache_write_tokens": int64(cacheWriteRate), "reasoning_tokens": int64(reasoningRate),
		}}
		cost, err := CostMicros(usage, pricing)
		if err != nil {
			t.Fatal(err)
		}
		var expected uint64
		for _, component := range [][2]uint32{
			{inputTokens, inputRate}, {outputTokens, outputRate}, {cacheReadTokens, cacheReadRate},
			{cacheWriteTokens, cacheWriteRate}, {reasoningTokens, reasoningRate},
		} {
			product := uint64(component[0]) * uint64(component[1])
			expected += product / uint64(tokenRateDenominator)
			if product%uint64(tokenRateDenominator) != 0 {
				expected++
			}
		}
		if cost != int64(expected) {
			t.Fatalf("cost=%d want=%d usage=%+v pricing=%+v", cost, expected, usage, pricing)
		}
	})
}

func TestCostMicrosPricesSupplementalMeasures(t *testing.T) {
	cost, err := CostMicros(
		contract.Usage{Measures: map[string]int64{"ocr_pages": 2}},
		catalog.Pricing{Rates: map[string]int64{"ocr_pages": 500_000}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if cost != 1 {
		t.Fatalf("cost=%d want=1", cost)
	}
}
