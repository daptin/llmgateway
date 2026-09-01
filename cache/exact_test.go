package cache

import (
	"testing"

	"github.com/daptin/llmgateway/catalog"
	"github.com/daptin/llmgateway/contract"
)

func TestExactCacheEligibilityIsConservative(t *testing.T) {
	zero := 0.0
	model := catalog.Model{ID: "m", Capabilities: map[string]bool{"exact_cache": true}}
	request := contract.Request{Operation: contract.OperationChat, Chat: &contract.ChatRequest{N: 1, Temperature: &zero}}
	if !Eligible(model, request, true) {
		t.Fatal("deterministic chat request should be eligible")
	}
	request.Chat.Tools = []contract.Tool{{Type: "function"}}
	if Eligible(model, request, true) {
		t.Fatal("tool request must not be eligible")
	}
	request.Chat.Tools = nil
	request.Stream = true
	if Eligible(model, request, true) {
		t.Fatal("stream request must not be eligible")
	}
	request.Stream = false
	if Eligible(model, request, false) {
		t.Fatal("unstable guardrail must disable caching")
	}
}

func TestExactCacheEligibilityPreservesCompletionAndStoreSemantics(t *testing.T) {
	zero := 0.0
	model := catalog.Model{Capabilities: map[string]bool{"exact_cache": true}}
	completion := contract.Request{Operation: contract.OperationTextCompletion, TextCompletion: &contract.TextCompletionRequest{
		Prompt: contract.CompletionPrompt{Texts: []string{"hello"}}, N: 1, BestOf: 1, Temperature: &zero,
	}}
	if !Eligible(model, completion, true) {
		t.Fatal("deterministic native completion should be eligible")
	}
	store := true
	chat := contract.Request{Operation: contract.OperationChat, Chat: &contract.ChatRequest{N: 1, Temperature: &zero, Store: &store}}
	if Eligible(model, chat, true) {
		t.Fatal("stored chat completion must reach the provider")
	}
}

func TestExactCacheKeysAreTenantAndRevisionSafe(t *testing.T) {
	model := catalog.Model{ID: "m", Capabilities: map[string]bool{"exact_cache": true}}
	request := contract.Request{Operation: contract.OperationEmbeddings, PublicModel: "model", Embeddings: &contract.EmbeddingsRequest{Input: contract.EmbeddingInput{Texts: []string{"hello"}}, EncodingFormat: "float"}}
	first, err := Key(1, model, contract.Principal{KeyID: "key-a"}, request)
	if err != nil {
		t.Fatal(err)
	}
	second, _ := Key(1, model, contract.Principal{KeyID: "key-b"}, request)
	newRevision, _ := Key(2, model, contract.Principal{KeyID: "key-a"}, request)
	if first == second || first == newRevision {
		t.Fatalf("cache key crossed tenant or revision: %s %s %s", first, second, newRevision)
	}
	model.Capabilities["public_cache"] = true
	first, _ = Key(1, model, contract.Principal{KeyID: "key-a"}, request)
	second, _ = Key(1, model, contract.Principal{KeyID: "key-b"}, request)
	if first != second {
		t.Fatal("explicit public cache scope did not share keys")
	}
}
