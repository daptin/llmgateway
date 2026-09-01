package llmgateway

import (
	"encoding/binary"
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

func FuzzAddUsageRejectsOverflowAndPreservesFields(f *testing.F) {
	f.Add(make([]byte, 14*8))
	overflow := make([]byte, 14*8)
	binary.LittleEndian.PutUint64(overflow[0:8], math.MaxInt64)
	binary.LittleEndian.PutUint64(overflow[7*8:8*8], 1)
	f.Add(overflow)
	f.Fuzz(func(t *testing.T, payload []byte) {
		if len(payload) < 14*8 {
			return
		}
		var fields [14]int64
		for index := range fields {
			fields[index] = int64(binary.LittleEndian.Uint64(payload[index*8 : (index+1)*8]))
		}
		left := contract.Usage{
			InputTokens: fields[0], OutputTokens: fields[1], CacheReadTokens: fields[2], CacheWriteTokens: fields[3],
			ReasoningTokens: fields[4], TotalTokens: fields[5], CostMicros: fields[6], Estimated: payload[0]&1 == 1,
		}
		right := contract.Usage{
			InputTokens: fields[7], OutputTokens: fields[8], CacheReadTokens: fields[9], CacheWriteTokens: fields[10],
			ReasoningTokens: fields[11], TotalTokens: fields[12], CostMicros: fields[13], Estimated: payload[1]&1 == 1,
		}
		result, err := addUsage(left, right)
		invalid := false
		for index := 0; index < 7; index++ {
			invalid = invalid || fields[index] < 0 || fields[index+7] < 0 || fields[index] > math.MaxInt64-fields[index+7]
		}
		if invalid {
			if err == nil {
				t.Fatalf("accepted invalid usage addition: left=%+v right=%+v result=%+v", left, right, result)
			}
			return
		}
		if err != nil {
			t.Fatalf("rejected valid usage addition: left=%+v right=%+v err=%v", left, right, err)
		}
		actual := [...]int64{result.InputTokens, result.OutputTokens, result.CacheReadTokens, result.CacheWriteTokens,
			result.ReasoningTokens, result.TotalTokens, result.CostMicros}
		for index := range actual {
			if actual[index] != fields[index]+fields[index+7] {
				t.Fatalf("field %d=%d want=%d", index, actual[index], fields[index]+fields[index+7])
			}
		}
		if result.Estimated != (left.Estimated || right.Estimated) {
			t.Fatalf("estimated=%v want=%v", result.Estimated, left.Estimated || right.Estimated)
		}
	})
}
