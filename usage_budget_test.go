package llmgateway

import (
	"strings"
	"testing"

	"github.com/daptin/llmgateway/catalog"
	"github.com/daptin/llmgateway/contract"
)

func TestBoundedExposureIncludesHistoryToolsAndOpaqueMediaIsUnbounded(t *testing.T) {
	request := contract.Request{Operation: contract.OperationChat, Chat: &contract.ChatRequest{N: 1, MaxCompletionTokens: 10,
		Messages: []contract.Message{{Role: "user", Content: []contract.ContentPart{{Type: "text", Text: strings.Repeat("界", 100)}}}},
		Tools:    []contract.Tool{{Type: "function", Function: contract.FunctionDefinition{Name: "lookup", Parameters: []byte(`{"type":"object"}`)}}}}}
	if err := applyBoundedTextExposure(&request); err != nil {
		t.Fatal(err)
	}
	if err := applyRequestExposure(&request); err != nil {
		t.Fatal(err)
	}
	first := request.EstimatedUsage.InputTokens
	if first < 300 || request.EstimatedUsage.TotalTokens != first+10 {
		t.Fatalf("exposure: %+v", request.EstimatedUsage)
	}
	request.Chat.Messages = append(request.Chat.Messages, contract.Message{Role: "tool", Content: []contract.ContentPart{{Type: "text", Text: strings.Repeat("x", 1000)}}})
	if err := applyBoundedTextExposure(&request); err != nil {
		t.Fatal(err)
	}
	if request.EstimatedUsage.InputTokens < first+1000 {
		t.Fatal("new tool result was omitted")
	}
	request.Chat.Messages[0].Content = []contract.ContentPart{{Type: "image_url", ImageURL: &contract.ImageURL{URL: "https://example.test/image"}}}
	if textExposureBounded(request) {
		t.Fatal("opaque image was treated as bounded text")
	}
}

func TestBudgetPricingDistinguishesExplicitFreeFromMissingAndAmbiguousRates(t *testing.T) {
	for _, test := range []struct {
		rates map[string]int64
		known bool
	}{
		{nil, false}, {map[string]int64{"input_tokens": 1}, false},
		{map[string]int64{"input_tokens": 0, "output_tokens": 0}, true},
		{map[string]int64{"total_tokens": 0}, true},
		{map[string]int64{"input_tokens": 1, "output_tokens": 2}, true},
		{map[string]int64{"total_tokens": 1, "input_tokens": 1}, false},
		{map[string]int64{"input_tokens": 1, "output_tokens": 1, "reasoning_tokens": 2}, false},
	} {
		if got := tokenPricesKnown(catalog.Pricing{Rates: test.rates}); got != test.known {
			t.Fatalf("rates=%v known=%v", test.rates, got)
		}
	}
}

func TestBudgetSettlementDoesNotTreatMissingTokenCategoriesAsFree(t *testing.T) {
	prepared := preparedRequest{boundedTokens: true, request: contract.Request{EstimatedUsage: contract.Usage{InputTokens: 100, OutputTokens: 20, TotalTokens: 120}}}
	usage, err := prepared.settledUsage(contract.Usage{InputTokens: 10, TotalTokens: 10}, catalog.Pricing{Rates: map[string]int64{"input_tokens": 1000000, "output_tokens": 1000000}})
	if err != nil || usage.TotalTokens != 30 || usage.CostMicros != 30 || !usage.Estimated {
		t.Fatalf("settlement=%+v err=%v", usage, err)
	}
	prepared.boundedTokens = false
	usage, err = prepared.settledUsage(contract.Usage{InputTokens: 10, TotalTokens: 10}, catalog.Pricing{})
	if err != nil || usage.TotalTokens != 10 || usage.Estimated {
		t.Fatalf("ordinary settlement changed: %+v %v", usage, err)
	}
}
