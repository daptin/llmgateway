package guardrail

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/daptin/llmgateway/catalog"
	"github.com/daptin/llmgateway/contract"
)

func TestPhraseRegexAndBoundsCheckers(t *testing.T) {
	request := contract.Request{
		MaxOutputTokens: 20, EstimatedUsage: contract.Usage{InputTokens: 5},
		Chat: &contract.ChatRequest{Messages: []contract.Message{{Role: "user", Content: []contract.ContentPart{{Type: "text", Text: "My secret is here"}}}}},
	}
	phrase, err := (PhraseFactory{}).Build(catalog.Guardrail{Config: json.RawMessage(`{"mode":"deny","patterns":["SECRET"]}`)})
	if err != nil {
		t.Fatal(err)
	}
	if decision, err := phrase.CheckInput(context.Background(), request); err != nil || decision.Allowed || decision.Reason != "phrase_policy" {
		t.Fatalf("phrase decision=%#v err=%v", decision, err)
	}
	regex, err := (RegexFactory{}).Build(catalog.Guardrail{Config: json.RawMessage(`{"mode":"allow","patterns":["secret\\s+is"]}`)})
	if err != nil {
		t.Fatal(err)
	}
	if decision, err := regex.CheckInput(context.Background(), request); err != nil || !decision.Allowed {
		t.Fatalf("regex decision=%#v err=%v", decision, err)
	}
	bounds, err := (BoundsFactory{}).Build(catalog.Guardrail{Config: json.RawMessage(`{"max_input_tokens":4,"max_output_tokens":20}`)})
	if err != nil {
		t.Fatal(err)
	}
	if decision, err := bounds.CheckInput(context.Background(), request); err != nil || decision.Allowed || !bounds.SupportsStreaming() {
		t.Fatalf("bounds decision=%#v streaming=%v err=%v", decision, bounds.SupportsStreaming(), err)
	}
}

func TestBuiltinsRejectUnknownOrInvalidConfiguration(t *testing.T) {
	for _, build := range []func() (Checker, error){
		func() (Checker, error) {
			return (PhraseFactory{}).Build(catalog.Guardrail{Config: json.RawMessage(`{"mode":"deny","patterns":[],"unknown":true}`)})
		},
		func() (Checker, error) {
			return (RegexFactory{}).Build(catalog.Guardrail{Config: json.RawMessage(`{"mode":"deny","patterns":["["]}`)})
		},
		func() (Checker, error) {
			return (BoundsFactory{}).Build(catalog.Guardrail{Config: json.RawMessage(`{}`)})
		},
	} {
		if _, err := build(); err == nil {
			t.Fatal("expected invalid config rejection")
		}
	}
}

func TestPhraseGuardrailCoversNativeTextCompletionInputOutputAndStream(t *testing.T) {
	checker, err := (PhraseFactory{}).Build(catalog.Guardrail{Config: json.RawMessage(`{"mode":"deny","patterns":["SECRET"]}`)})
	if err != nil {
		t.Fatal(err)
	}
	request := contract.Request{TextCompletion: &contract.TextCompletionRequest{Prompt: contract.CompletionPrompt{Texts: []string{"secret input"}}}}
	if decision, err := checker.CheckInput(context.Background(), request); err != nil || decision.Allowed {
		t.Fatalf("completion input decision=%#v err=%v", decision, err)
	}
	response := contract.Response{TextCompletion: &contract.TextCompletionResponse{Choices: []contract.TextCompletionChoice{{Text: "secret output"}}}}
	if decision, err := checker.CheckOutput(context.Background(), request, response); err != nil || decision.Allowed {
		t.Fatalf("completion output decision=%#v err=%v", decision, err)
	}
	event := contract.StreamEvent{TextCompletion: &contract.TextCompletionDelta{Text: "secret stream"}}
	if decision, err := checker.CheckStream(context.Background(), request, event); err != nil || decision.Allowed {
		t.Fatalf("completion stream decision=%#v err=%v", decision, err)
	}
}
