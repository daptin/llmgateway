package llmgateway

import (
	"errors"
	"math"

	"github.com/daptin/llmgateway/accounting"
	"github.com/daptin/llmgateway/catalog"
	"github.com/daptin/llmgateway/contract"
	"github.com/daptin/llmgateway/internal/routing"
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

func applyRequestExposure(request *contract.Request) error {
	if request == nil {
		return errors.New("request is required")
	}
	var output int64
	switch request.Operation {
	case contract.OperationChat:
		if request.Chat == nil || request.Chat.N < 0 || request.Chat.MaxCompletionTokens < 0 ||
			(request.Chat.N > 0 && request.Chat.MaxCompletionTokens > math.MaxInt64/int64(request.Chat.N)) {
			return errors.New("chat output exposure overflows")
		}
		output = request.Chat.MaxCompletionTokens * int64(request.Chat.N)
	case contract.OperationResponses:
		output = request.MaxOutputTokens
	}
	if request.EstimatedUsage.OutputTokens < output {
		request.EstimatedUsage.OutputTokens = output
		request.EstimatedUsage.Estimated = true
	}
	return normalizeTokenTotal(&request.EstimatedUsage)
}

func reservationExposure(estimate contract.Usage, attempts []routing.Attempt) (contract.Usage, []contract.Usage, error) {
	if len(attempts) == 0 {
		return contract.Usage{}, nil, errors.New("route plan has no attempts")
	}
	exposure := contract.Usage{Estimated: true}
	perAttempt := make([]contract.Usage, 0, len(attempts))
	for _, attempt := range attempts {
		usage, err := settledUsage(contract.Usage{}, estimate, attempt.Deployment.Pricing)
		if err != nil {
			return contract.Usage{}, nil, err
		}
		perAttempt = append(perAttempt, usage)
		exposure, err = addUsage(exposure, usage)
		if err != nil {
			return contract.Usage{}, nil, err
		}
	}
	return exposure, perAttempt, nil
}

func aggregateAttemptUsage(attempts []contract.Attempt) (contract.Usage, error) {
	var total contract.Usage
	for _, attempt := range attempts {
		var err error
		total, err = addUsage(total, attempt.Usage)
		if err != nil {
			return contract.Usage{}, err
		}
	}
	return total, nil
}

func addUsage(left, right contract.Usage) (contract.Usage, error) {
	values := [][2]int64{
		{left.InputTokens, right.InputTokens}, {left.OutputTokens, right.OutputTokens},
		{left.CacheReadTokens, right.CacheReadTokens}, {left.CacheWriteTokens, right.CacheWriteTokens},
		{left.ReasoningTokens, right.ReasoningTokens}, {left.TotalTokens, right.TotalTokens},
		{left.CostMicros, right.CostMicros},
	}
	sums := make([]int64, len(values))
	for index, pair := range values {
		if pair[0] < 0 || pair[1] < 0 || pair[0] > math.MaxInt64-pair[1] {
			return contract.Usage{}, errors.New("aggregate usage overflows")
		}
		sums[index] = pair[0] + pair[1]
	}
	return contract.Usage{
		InputTokens: sums[0], OutputTokens: sums[1], CacheReadTokens: sums[2], CacheWriteTokens: sums[3],
		ReasoningTokens: sums[4], TotalTokens: sums[5], CostMicros: sums[6], Estimated: left.Estimated || right.Estimated,
	}, nil
}
