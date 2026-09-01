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
	usedEstimate := false
	if tokenUsageEmpty(usage) {
		usage.InputTokens = estimate.InputTokens
		usage.OutputTokens = estimate.OutputTokens
		usage.CacheReadTokens = estimate.CacheReadTokens
		usage.CacheWriteTokens = estimate.CacheWriteTokens
		usage.ReasoningTokens = estimate.ReasoningTokens
		usage.TotalTokens = estimate.TotalTokens
		usedEstimate = !tokenUsageEmpty(estimate)
	}
	usage.Measures = mergedMeasures(estimate.Measures, reported.Measures)
	for name := range estimate.Measures {
		if _, reportedMeasure := reported.Measures[name]; !reportedMeasure {
			usedEstimate = true
		}
	}
	usage.Estimated = reported.Estimated || usedEstimate
	if !usage.Valid() {
		return contract.Usage{}, errors.New("usage is invalid")
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

func mergedMeasures(estimate, reported map[string]int64) map[string]int64 {
	if len(estimate) == 0 && len(reported) == 0 {
		return nil
	}
	result := make(map[string]int64, len(estimate)+len(reported))
	for name, value := range estimate {
		result[name] = value
	}
	for name, value := range reported {
		result[name] = value
	}
	return result
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
	case contract.OperationTextCompletion:
		if request.TextCompletion == nil || request.TextCompletion.BestOf < 0 || request.TextCompletion.MaxTokens < 0 ||
			(request.TextCompletion.BestOf > 0 && request.TextCompletion.MaxTokens > math.MaxInt64/int64(request.TextCompletion.BestOf)) {
			return errors.New("text completion output exposure overflows")
		}
		output = request.TextCompletion.MaxTokens * int64(request.TextCompletion.BestOf)
		request.MaxOutputTokens = output
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
	if !left.Valid() || !right.Valid() {
		return contract.Usage{}, errors.New("aggregate usage is invalid")
	}
	values := [][2]int64{
		{left.InputTokens, right.InputTokens}, {left.OutputTokens, right.OutputTokens},
		{left.CacheReadTokens, right.CacheReadTokens}, {left.CacheWriteTokens, right.CacheWriteTokens},
		{left.ReasoningTokens, right.ReasoningTokens}, {left.TotalTokens, right.TotalTokens},
		{left.CostMicros, right.CostMicros},
	}
	sums := make([]int64, len(values))
	for index, pair := range values {
		if pair[0] > math.MaxInt64-pair[1] {
			return contract.Usage{}, errors.New("aggregate usage overflows")
		}
		sums[index] = pair[0] + pair[1]
	}
	measures := make(map[string]int64, len(left.Measures)+len(right.Measures))
	for name, value := range left.Measures {
		measures[name] = value
	}
	for name, value := range right.Measures {
		if measures[name] > math.MaxInt64-value {
			return contract.Usage{}, errors.New("aggregate usage measure overflows")
		}
		measures[name] += value
	}
	if len(measures) == 0 {
		measures = nil
	}
	return contract.Usage{
		InputTokens: sums[0], OutputTokens: sums[1], CacheReadTokens: sums[2], CacheWriteTokens: sums[3],
		ReasoningTokens: sums[4], TotalTokens: sums[5], CostMicros: sums[6], Estimated: left.Estimated || right.Estimated,
		Measures: measures,
	}, nil
}
