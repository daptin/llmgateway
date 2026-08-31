package llmgateway

import (
	"errors"
	"math"

	"github.com/daptin/llmgateway/accounting"
	"github.com/daptin/llmgateway/catalog"
	"github.com/daptin/llmgateway/contract"
)

// settledUsage gives provider-reported token counts precedence. Providers that
// omit usage must never turn a billable request into a zero-usage record, so the
// admission estimate becomes the conservative terminal value and is marked as
// estimated.
func settledUsage(reported, estimate contract.Usage, pricing catalog.Pricing) (contract.Usage, error) {
	usage := reported
	if tokenUsageEmpty(usage) {
		usage = estimate
		usage.Estimated = true
	}
	if err := normalizeTokenTotal(&usage); err != nil {
		return contract.Usage{}, err
	}
	usage.CostMicros = 0
	cost, err := accounting.CostMicros(usage, pricing)
	if err != nil {
		return contract.Usage{}, err
	}
	usage.CostMicros = cost
	return usage, nil
}

func normalizeTokenTotal(usage *contract.Usage) error {
	if usage == nil || usage.InputTokens < 0 || usage.OutputTokens < 0 || usage.InputTokens > math.MaxInt64-usage.OutputTokens {
		return errors.New("token usage is invalid or overflows")
	}
	minimum := usage.InputTokens + usage.OutputTokens
	if usage.TotalTokens < minimum {
		usage.TotalTokens = minimum
	}
	return nil
}

func tokenUsageEmpty(usage contract.Usage) bool {
	return usage.InputTokens == 0 && usage.OutputTokens == 0 && usage.CacheReadTokens == 0 &&
		usage.CacheWriteTokens == 0 && usage.ReasoningTokens == 0 && usage.TotalTokens == 0
}
