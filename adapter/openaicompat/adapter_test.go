package openaicompat

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	baseadapter "github.com/daptin/llmgateway/adapter"
	"github.com/daptin/llmgateway/catalog"
	"github.com/daptin/llmgateway/compatibility"
	"github.com/daptin/llmgateway/contract"
	"github.com/daptin/llmgateway/internal/netpolicy"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func deploymentWithParameters(parameters string) catalog.Deployment {
	value := deployment()
	value.Parameters = json.RawMessage(parameters)
	return value
}

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func buildAdapter(t *testing.T, server *httptest.Server, parameters string, options Factory) *Adapter {
	t.Helper()
	provider := catalog.Provider{ID: "p", Name: "provider", Type: "openai-compatible", BaseURL: server.URL + "/v1", AllowInsecure: true, AllowPrivateNetwork: true, Parameters: json.RawMessage(parameters), Enabled: true}
	built, err := options.Build(context.Background(), provider, baseadapter.NewSecret([]byte("secret-key")))
	if err != nil {
		t.Fatal(err)
	}
	return built.(*Adapter)
}

func TestManifestOperationsMatchOpenAICompatibleAdapter(t *testing.T) {
	manifest, err := compatibility.Default()
	if err != nil {
		t.Fatal(err)
	}
	var declared []string
	for _, provider := range manifest.Providers {
		if provider.Name != "openai-compatible" {
			continue
		}
		for operation := range provider.Operations {
			declared = append(declared, operation)
		}
	}
	capabilities := (&Adapter{}).Capabilities()
	implemented := make([]string, 0, len(capabilities.Operations))
	for operation, supported := range capabilities.Operations {
		if supported {
			implemented = append(implemented, string(operation))
		}
	}
	sort.Strings(declared)
	sort.Strings(implemented)
	if !reflect.DeepEqual(declared, implemented) {
		t.Fatalf("manifest operations = %v, adapter capabilities = %v", declared, implemented)
	}
}

func deployment() catalog.Deployment {
	return catalog.Deployment{ID: "d", Name: "deployment", UpstreamModel: "upstream-model", RequestTimeout: time.Second, ConnectTimeout: time.Second}
}

type countedResponseBody struct {
	closes atomic.Int64
	err    error
}

func (*countedResponseBody) Read([]byte) (int, error) { return 0, io.EOF }
func (body *countedResponseBody) Close() error {
	body.closes.Add(1)
	return body.err
}

func TestCancelReadCloserClosesAndCancelsExactlyOnce(t *testing.T) {
	expected := errors.New("close failed")
	body := &countedResponseBody{err: expected}
	var cancellations atomic.Int64
	wrapped := &cancelReadCloser{ReadCloser: body, cancel: func() { cancellations.Add(1) }}
	results := make(chan error, 16)
	var callers sync.WaitGroup
	for range 16 {
		callers.Add(1)
		go func() {
			defer callers.Done()
			results <- wrapped.Close()
		}()
	}
	callers.Wait()
	close(results)
	for err := range results {
		if !errors.Is(err, expected) {
			t.Fatalf("close error=%v want=%v", err, expected)
		}
	}
	if body.closes.Load() != 1 || cancellations.Load() != 1 {
		t.Fatalf("underlying closes=%d cancellations=%d, want 1/1", body.closes.Load(), cancellations.Load())
	}
}

func TestInvokeChatTranslatesCanonicalRequestAndResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/chat/completions" || request.Header.Get("Authorization") != "Bearer secret-key" || request.Header.Get("OpenAI-Organization") != "org" {
			t.Fatalf("unexpected upstream request: %s headers=%v", request.URL.Path, request.Header)
		}
		var body map[string]any
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["model"] != "upstream-model" || body["max_completion_tokens"] != float64(20) || body["stream"] != false ||
			body["frequency_penalty"] != -0.5 || body["presence_penalty"] != 1.25 || body["parallel_tool_calls"] != false || body["reasoning_effort"] != "low" ||
			body["store"] != false || body["prompt_cache_key"] != "tenant-conversation" {
			t.Fatalf("unexpected body: %#v", body)
		}
		messages := body["messages"].([]any)
		content := messages[0].(map[string]any)["content"].([]any)
		audio := content[1].(map[string]any)["input_audio"].(map[string]any)
		if audio["data"] != "AAAA" || audio["format"] != "wav" {
			t.Fatalf("input audio was not translated: %#v", content)
		}
		response.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(response, `{"id":"chatcmpl_1","created":123,"model":"actual-model","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"weather","arguments":"{\"city\":\"Pune\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":4,"total_tokens":14,"prompt_tokens_details":{"cached_tokens":2},"completion_tokens_details":{"reasoning_tokens":3}}}`)
	}))
	defer server.Close()
	adapter := buildAdapter(t, server, `{"organization":"org"}`, Factory{})
	frequency, presence, parallel, store := -0.5, 1.25, false, false
	request := contract.Request{Operation: contract.OperationChat, Chat: &contract.ChatRequest{
		Messages: []contract.Message{{Role: "user", Content: []contract.ContentPart{
			{Type: "text", Text: "weather"}, {Type: "input_audio", Audio: &contract.InputAudio{Data: "AAAA", Format: "wav"}},
		}}}, MaxCompletionTokens: 20,
		Tools:            []contract.Tool{{Type: "function", Function: contract.FunctionDefinition{Name: "weather", Parameters: json.RawMessage(`{"type":"object"}`)}}},
		FrequencyPenalty: &frequency, PresencePenalty: &presence, ParallelToolCalls: &parallel, ReasoningEffort: "low",
		Store: &store, PromptCacheKey: "tenant-conversation",
	}}
	result, err := adapter.Invoke(context.Background(), deployment(), request)
	if err != nil {
		t.Fatal(err)
	}
	if result.Chat == nil || result.Chat.ID != "chatcmpl_1" || result.Chat.Choices[0].Message.ToolCalls[0].Function.Name != "weather" {
		t.Fatalf("unexpected canonical response: %#v", result)
	}
	if result.Usage.TotalTokens != 14 || result.Usage.CacheReadTokens != 2 || result.Usage.ReasoningTokens != 3 {
		t.Fatalf("unexpected usage: %#v", result.Usage)
	}
}

func TestInvokeTextCompletionUsesNativeUpstreamContract(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/completions" {
			t.Fatalf("path=%s", request.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["model"] != "upstream-model" || body["prompt"] != "hello" || body["max_tokens"] != float64(4) ||
			body["n"] != float64(1) || body["echo"] != false || body["logprobs"] != float64(2) {
			t.Fatalf("body=%#v", body)
		}
		response.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(response, `{"id":"cmpl_1","created":10,"model":"actual","choices":[{"text":" world","index":0,"logprobs":null,"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	defer server.Close()
	echo, logprobs := false, 2
	result, err := buildAdapter(t, server, `{}`, Factory{}).Invoke(context.Background(), deployment(), contract.Request{
		Operation: contract.OperationTextCompletion, TextCompletion: &contract.TextCompletionRequest{
			Prompt: contract.CompletionPrompt{Texts: []string{"hello"}}, MaxTokens: 4, N: 1, BestOf: 1, Echo: &echo, Logprobs: &logprobs,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.TextCompletion == nil || result.TextCompletion.ID != "cmpl_1" || result.TextCompletion.Choices[0].Text != " world" || result.Usage.TotalTokens != 2 {
		t.Fatalf("result=%#v", result)
	}
}

func TestInvokeModerationAndRerankUseCanonicalUpstreamContracts(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		response.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/v1/moderations":
			if body["model"] != "upstream-model" || body["input"] != "inspect" {
				t.Fatalf("moderation body=%#v", body)
			}
			_, _ = io.WriteString(response, `{"id":"modr_1","model":"actual","results":[{"flagged":false,"categories":{"violence":false},"category_scores":{"violence":0.01}}]}`)
		case "/v1/rerank":
			if body["model"] != "upstream-model" || body["query"] != "best" || body["top_n"] != float64(1) || len(body["documents"].([]any)) != 2 {
				t.Fatalf("rerank body=%#v", body)
			}
			_, _ = io.WriteString(response, `{"id":"rerank_1","results":[{"index":1,"relevance_score":0.9,"document":{"text":"two"}}],"meta":{"billed_units":{"search_units":1},"tokens":{"input_tokens":4,"output_tokens":0}}}`)
		default:
			t.Fatalf("path=%s", request.URL.Path)
		}
	}))
	defer server.Close()
	built := buildAdapter(t, server, `{}`, Factory{})
	moderation, err := built.Invoke(context.Background(), deployment(), contract.Request{Operation: contract.OperationModeration,
		Moderation: &contract.ModerationRequest{Input: []contract.ModerationInput{{Type: "text", Text: "inspect"}}}})
	if err != nil || moderation.Moderation == nil || len(moderation.Moderation.Results) != 1 {
		t.Fatalf("moderation=%#v err=%v", moderation, err)
	}
	returnDocuments := true
	reranked, err := built.Invoke(context.Background(), deployment(), contract.Request{Operation: contract.OperationRerank,
		Rerank: &contract.RerankRequest{Query: "best", Documents: []contract.RerankDocument{{Text: "one"}, {Text: "two"}}, TopN: 1, ReturnDocuments: &returnDocuments}})
	if err != nil || reranked.Rerank == nil || len(reranked.Rerank.Results) != 1 || reranked.Rerank.Results[0].Index != 1 || reranked.Usage.InputTokens != 4 || reranked.Usage.Measures["search_units"] != 1 {
		t.Fatalf("rerank=%#v err=%v", reranked, err)
	}
	openRouter, err := decodeResponse(contract.OperationRerank, []byte(`{"id":"rerank_2","results":[{"index":0,"relevance_score":0.8,"document":{"text":"one"}}],"usage":{"search_units":2,"total_tokens":7}}`))
	if err != nil || openRouter.Rerank == nil || openRouter.Rerank.Meta.SearchUnits != 2 || openRouter.Usage.TotalTokens != 7 || openRouter.Usage.Measures["search_units"] != 2 {
		t.Fatalf("OpenRouter rerank=%#v err=%v", openRouter, err)
	}
}

func TestEncodeMessagesOmitsEmptyImageDetail(t *testing.T) {
	encoded := encodeMessages([]contract.Message{{Role: "user", Content: []contract.ContentPart{
		{Type: "text", Text: "describe"},
		{Type: "image_url", ImageURL: &contract.ImageURL{URL: "data:image/png;base64,AAAA"}},
	}}})
	parts := encoded[0]["content"].([]map[string]any)
	image := parts[1]["image_url"].(map[string]any)
	if _, exists := image["detail"]; exists {
		t.Fatalf("empty optional image detail was encoded: %#v", image)
	}
}

func TestHealthCheckUsesAuthenticatedModelsEndpoint(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet || request.URL.Path != "/v1/models" || request.Header.Get("Authorization") != "Bearer secret-key" {
			t.Fatalf("unexpected health request: %s %s headers=%v", request.Method, request.URL.Path, request.Header)
		}
		response.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(response, `{"object":"list","data":[]}`)
	}))
	defer server.Close()
	if err := buildAdapter(t, server, `{}`, Factory{}).HealthCheck(context.Background(), deployment()); err != nil {
		t.Fatal(err)
	}
}

func TestHealthCheckUsesValidatedDeploymentPathAndModel(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet || request.RequestURI != "/v1/health/openai%2Fgpt-4o" {
			t.Fatalf("unexpected health request: %s %s", request.Method, request.RequestURI)
		}
		response.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	built := buildAdapter(t, server, `{}`, Factory{})
	configured := deployment()
	configured.HealthCheck = catalog.HealthCheck{Enabled: true, Path: "/health/{model}", Model: "openai/gpt-4o"}
	if err := built.ValidateDeployment(configured); err != nil {
		t.Fatal(err)
	}
	if err := built.HealthCheck(context.Background(), configured); err != nil {
		t.Fatal(err)
	}
	configured.HealthCheck.Path = "https://attacker.example/health"
	if err := built.ValidateDeployment(configured); err == nil {
		t.Fatal("accepted an absolute health-check URL")
	}
}

func TestBuildRequiresExplicitPrivateNetworkOptIn(t *testing.T) {
	provider := catalog.Provider{ID: "p", Name: "provider", Type: "openai-compatible", BaseURL: "https://127.0.0.1/v1", Enabled: true}
	if _, err := (Factory{}).Build(context.Background(), provider, baseadapter.NewSecret([]byte("secret"))); err == nil || !strings.Contains(err.Error(), "private-network") {
		t.Fatalf("private provider error=%v", err)
	}
}

func TestBuildUsesNamedProviderBaseURLsAndPreservesExplicitURL(t *testing.T) {
	for providerType, expected := range providerBaseURLs {
		built, err := (Factory{}).Build(context.Background(), catalog.Provider{
			ID: "p", Name: "provider", Type: providerType, Enabled: true,
		}, baseadapter.NewSecret([]byte("secret")))
		if err != nil {
			t.Fatalf("build %s: %v", providerType, err)
		}
		if actual := built.(*Adapter).baseURL.String(); actual != expected {
			t.Fatalf("%s base URL = %q, want %q", providerType, actual, expected)
		}
	}
	explicit := "https://gateway.example/v1"
	built, err := (Factory{}).Build(context.Background(), catalog.Provider{
		ID: "p", Name: "provider", Type: "google", BaseURL: explicit, Enabled: true,
	}, baseadapter.NewSecret([]byte("secret")))
	if err != nil {
		t.Fatal(err)
	}
	if actual := built.(*Adapter).baseURL.String(); actual != explicit {
		t.Fatalf("explicit base URL = %q, want %q", actual, explicit)
	}
	if _, err := (Factory{}).Build(context.Background(), catalog.Provider{
		ID: "p", Name: "provider", Type: "openai-compatible", Enabled: true,
	}, baseadapter.NewSecret([]byte("secret"))); err == nil {
		t.Fatal("generic OpenAI-compatible provider accepted an empty base URL")
	}
	for _, invalid := range []string{
		"https://user:password@gateway.example/v1",
		"https://gateway.example/v1?tenant=hidden",
		"https://gateway.example/v1#fragment",
	} {
		if _, err := (Factory{}).Build(context.Background(), catalog.Provider{
			ID: "p", Name: "provider", Type: "openai-compatible", BaseURL: invalid, Enabled: true,
		}, baseadapter.NewSecret([]byte("secret"))); err == nil {
			t.Fatalf("accepted ambiguous provider base URL %q", invalid)
		}
	}
}

func TestDeploymentParametersAddOnlyOperationScopedExtensionFields(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		provider, ok := body["provider"].(map[string]any)
		if !ok || provider["order"] != "latency" {
			t.Fatalf("deployment extension was not preserved: %#v", body)
		}
		_, _ = io.WriteString(response, `{"id":"chatcmpl_1","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	defer server.Close()
	built := buildAdapter(t, server, `{}`, Factory{})
	configured := deployment()
	configured.Parameters = json.RawMessage(`{"chat":{"provider":{"order":"latency"}}}`)
	request := contract.Request{Operation: contract.OperationChat, Chat: &contract.ChatRequest{
		Messages: []contract.Message{{Role: "user", Content: []contract.ContentPart{{Type: "text", Text: "hello"}}}},
	}}
	if _, err := built.Invoke(context.Background(), configured, request); err != nil {
		t.Fatal(err)
	}

	for _, raw := range []string{
		`{"unknown":{}}`,
		`{"chat":{"model":"shadow-model"}}`,
		`{"image_variation":{"image":"shadow-image"}}`,
	} {
		configured.Parameters = json.RawMessage(raw)
		if err := built.ValidateDeployment(configured); err == nil {
			t.Fatalf("accepted invalid deployment parameters: %s", raw)
		}
	}
}

func TestInvokeSupportsResponsesEmbeddingsAndImages(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/v1/responses":
			var body map[string]any
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			tool := body["tools"].([]any)[0].(map[string]any)
			if tool["name"] != "weather" || tool["function"] != nil {
				t.Fatalf("Responses tool was not flat: %#v", tool)
			}
			reasoning := body["reasoning"].(map[string]any)
			if body["temperature"] != 0.4 || body["top_p"] != 0.8 || body["parallel_tool_calls"] != false || reasoning["effort"] != "low" || reasoning["summary"] != "concise" ||
				body["prompt_cache_key"] != "tenant-thread" || body["safety_identifier"] != "safe-user" || body["service_tier"] != "priority" ||
				body["truncation"] != "disabled" || body["user"] != "legacy-user" || body["top_logprobs"] != float64(3) || body["store"] != false {
				t.Fatalf("Responses controls were not preserved: %#v", body)
			}
			content := body["input"].([]any)[0].(map[string]any)["content"].([]any)
			file := content[1].(map[string]any)
			if file["type"] != "input_file" || file["filename"] != "brief.pdf" || file["file_data"] != "data:application/pdf;base64,cGRm" {
				t.Fatalf("Responses inline file was not preserved: %#v", content)
			}
			_, _ = io.WriteString(response, `{"id":"resp_1","model":"m","status":"completed","output":[{"type":"reasoning","id":"reason_1","summary":[{"type":"summary_text","text":"brief"}],"content":[{"type":"reasoning_text","text":"worked"}]},{"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"hello"}]}],"usage":{"input_tokens":2,"output_tokens":1,"total_tokens":3}}`)
		case "/v1/embeddings":
			_, _ = io.WriteString(response, `{"model":"m","data":[{"index":0,"embedding":[0.1,0.2]}],"usage":{"prompt_tokens":2,"total_tokens":2}}`)
		case "/v1/images":
			_, _ = io.WriteString(response, `{"created":9,"data":[{"b64_json":"AAAA"}],"usage":{"prompt_tokens":3,"completion_tokens":7,"total_tokens":10}}`)
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()
	adapter := buildAdapter(t, server, `{"image_generation_path":"/images"}`, Factory{})
	temperature, topP, parallel := 0.4, 0.8, false
	topLogprobs := 3
	tests := []struct {
		name    string
		request contract.Request
		check   func(contract.Response) bool
	}{
		{name: "responses", request: contract.Request{Operation: contract.OperationResponses, Responses: &contract.ResponsesRequest{
			Input: []contract.ResponseInputItem{{Type: "message", Role: "user", Content: []contract.ContentPart{
				{Type: "input_text", Text: "hi"},
				{Type: "input_file", File: &contract.InputFile{Data: "data:application/pdf;base64,cGRm", Filename: "brief.pdf"}},
			}}},
			Tools:       []contract.Tool{{Type: "function", Function: contract.FunctionDefinition{Name: "weather", Parameters: json.RawMessage(`{"type":"object"}`)}}},
			Temperature: &temperature, TopP: &topP, ParallelToolCalls: &parallel, ReasoningEffort: "low", ReasoningSummary: "concise",
			PromptCacheKey: "tenant-thread", SafetyIdentifier: "safe-user", ServiceTier: "priority", Truncation: "disabled",
			User: "legacy-user", TopLogprobs: &topLogprobs,
		}}, check: func(value contract.Response) bool {
			return value.Responses != nil && value.Responses.Output[0].Status == "" && value.Responses.Output[0].Summary[0].Text == "brief" &&
				value.Responses.Output[0].Content[0].Text == "worked" && value.Responses.Output[1].Content[0].Text == "hello"
		}},
		{name: "embeddings", request: contract.Request{Operation: contract.OperationEmbeddings, Embeddings: &contract.EmbeddingsRequest{Input: contract.EmbeddingInput{Texts: []string{"hi"}}, EncodingFormat: "float"}}, check: func(value contract.Response) bool {
			return value.Embeddings != nil && len(value.Embeddings.Data[0].Vector) == 2
		}},
		{name: "images", request: contract.Request{Operation: contract.OperationImageGeneration, ImageGeneration: &contract.ImageGenerationRequest{Prompt: "cat", N: 1, ResponseFormat: "b64_json"}}, check: func(value contract.Response) bool {
			return value.Images != nil && value.Images.Data[0].Base64 == "AAAA" && value.Usage.TotalTokens == 10
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result, err := adapter.Invoke(context.Background(), deployment(), test.request)
			if err != nil {
				t.Fatal(err)
			}
			if !test.check(result) {
				t.Fatalf("unexpected result: %#v", result)
			}
		})
	}
}

func TestInvokeSupportsResponseCompaction(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/responses/compact" {
			http.NotFound(response, request)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		options, ok := body["prompt_cache_options"].(map[string]any)
		input, inputOK := body["input"].([]any)
		if !ok || options["mode"] != "explicit" || options["ttl"] != "30m" || body["prompt_cache_retention"] != "24h" ||
			!inputOK || input[1].(map[string]any)["encrypted_content"] != "previous" {
			t.Fatalf("compaction request was not preserved: %#v", body)
		}
		response.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(response, `{"id":"resp_compact_1","object":"response.compaction","created_at":123,"output":[{"type":"message","id":"msg_1","role":"user","content":[{"type":"input_text","text":"hello"},{"type":"input_image","image_url":"https://example.test/image.png","detail":"low"}]},{"type":"compaction","id":"cmp_1","encrypted_content":"opaque","created_by":"service"}],"usage":{"input_tokens":9,"output_tokens":2,"total_tokens":11}}`)
	}))
	defer server.Close()
	adapter := buildAdapter(t, server, `{}`, Factory{})
	result, err := adapter.Invoke(context.Background(), deployment(), contract.Request{Operation: contract.OperationResponseCompact,
		Responses: &contract.ResponsesRequest{Input: []contract.ResponseInputItem{
			{Type: "message", Role: "user", Content: []contract.ContentPart{{Type: "input_text", Text: "hello"}}},
			{Type: "compaction", EncryptedContent: "previous"},
		}, PromptCacheMode: "explicit", PromptCacheTTL: "30m", PromptCacheRetention: "24h"}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Responses == nil || result.Responses.Object != "response.compaction" || result.Responses.Output[0].Role != "user" ||
		result.Responses.Output[0].Content[1].ImageURL == nil || result.Responses.Output[0].Content[1].ImageURL.Detail != "low" ||
		result.Responses.Output[1].EncryptedContent != "opaque" || result.Usage.TotalTokens != 11 {
		t.Fatalf("compaction response = %#v", result)
	}
}

func TestProviderRedirectsAreNotFollowed(t *testing.T) {
	var redirected atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		redirected.Add(1)
	}))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		http.Redirect(response, &http.Request{}, target.URL, http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	adapter := buildAdapter(t, server, `{}`, Factory{})
	_, err := adapter.Invoke(context.Background(), deployment(), contract.Request{Operation: contract.OperationEmbeddings,
		Embeddings: &contract.EmbeddingsRequest{Input: contract.EmbeddingInput{Texts: []string{"hi"}}}})
	var typed *contract.Error
	if !errors.As(err, &typed) || typed.HTTPStatus != http.StatusBadGateway || typed.Retryable || redirected.Load() != 0 {
		t.Fatalf("redirect error=%v followed=%d", err, redirected.Load())
	}
}

func TestPrivateIPRejectsCarrierGradeNATAndMulticast(t *testing.T) {
	for _, value := range []string{"100.64.0.1", "100.127.255.254", "224.0.0.1", "ff02::1"} {
		if !netpolicy.PrivateHost(value) {
			t.Fatalf("%s was not classified as private", value)
		}
	}
}

func TestCustomTransportRequiresExplicitPrivateNetworkOptIn(t *testing.T) {
	provider := catalog.Provider{ID: "p", Name: "custom", Type: "openai-compatible", BaseURL: "https://example.test/v1", Enabled: true}
	if _, err := (Factory{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("unused")
	})}).Build(context.Background(), provider, baseadapter.NewSecret([]byte("key"))); err == nil {
		t.Fatal("custom transport bypassed private-network enforcement without explicit opt-in")
	}
	provider.AllowPrivateNetwork = true
	if _, err := (Factory{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("unused")
	})}).Build(context.Background(), provider, baseadapter.NewSecret([]byte("key"))); err != nil {
		t.Fatalf("explicit custom transport opt-in: %v", err)
	}
}

func TestRetryAfterSaturatesWithoutOverflow(t *testing.T) {
	if got := retryAfter("9223372036854775807", time.Now()); got != time.Duration(1<<63-1) {
		t.Fatalf("retry-after duration = %s", got)
	}
}

func TestProviderErrorsAreNormalizedAndSecretSafe(t *testing.T) {
	tests := []struct {
		name      string
		status    int
		code      contract.ErrorCode
		retryable bool
	}{
		{name: "bad request", status: http.StatusBadRequest, code: contract.ErrorInvalidRequest},
		{name: "authentication", status: http.StatusUnauthorized, code: contract.ErrorAuthentication},
		{name: "permission", status: http.StatusForbidden, code: contract.ErrorPermission},
		{name: "quota", status: http.StatusPaymentRequired, code: contract.ErrorInsufficientQuota},
		{name: "model", status: http.StatusNotFound, code: contract.ErrorModelNotFound},
		{name: "request timeout", status: http.StatusRequestTimeout, code: contract.ErrorTimeout, retryable: true},
		{name: "conflict", status: http.StatusConflict, code: contract.ErrorProvider, retryable: true},
		{name: "unprocessable", status: http.StatusUnprocessableEntity, code: contract.ErrorInvalidRequest},
		{name: "rate limit", status: http.StatusTooManyRequests, code: contract.ErrorRateLimit, retryable: true},
		{name: "gateway timeout", status: http.StatusGatewayTimeout, code: contract.ErrorTimeout, retryable: true},
		{name: "provider failure", status: http.StatusBadGateway, code: contract.ErrorProvider, retryable: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
				response.Header().Set("Retry-After", "7")
				response.WriteHeader(test.status)
				_, _ = io.WriteString(response, `{"error":{"message":"raw provider secret detail"}}`)
			}))
			defer server.Close()
			adapter := buildAdapter(t, server, `{}`, Factory{})
			_, err := adapter.Invoke(context.Background(), deployment(), contract.Request{Operation: contract.OperationEmbeddings,
				Embeddings: &contract.EmbeddingsRequest{Input: contract.EmbeddingInput{Texts: []string{"hi"}}}})
			var typed *contract.Error
			if !errors.As(err, &typed) || typed.Code != test.code || typed.HTTPStatus != test.status || typed.Retryable != test.retryable {
				t.Fatalf("normalized error = %#v, want code=%s status=%d retryable=%v", err, test.code, test.status, test.retryable)
			}
			wantRetryAfter := time.Duration(0)
			if test.retryable {
				wantRetryAfter = 7 * time.Second
			}
			if typed.RetryAfter != wantRetryAfter {
				t.Fatalf("retry-after = %s, want %s", typed.RetryAfter, wantRetryAfter)
			}
			if unwrapped := errors.Unwrap(typed); typed.Cause != nil || unwrapped != nil && strings.Contains(unwrapped.Error(), "raw provider") {
				t.Fatalf("normalized error retained provider body data: %#v", typed)
			}
			safe := typed.Safe()
			if safe.Cause != nil || strings.Contains(safe.Error(), "raw provider") || strings.Contains(safe.Error(), "secret-key") {
				t.Fatalf("public error leaked provider data: %#v", safe)
			}
		})
	}
}

func TestMalformedProviderResponsesFailExplicitly(t *testing.T) {
	tests := []struct {
		name      string
		operation contract.Operation
		payload   string
	}{
		{name: "chat missing choices", operation: contract.OperationChat, payload: `{"id":"chat"}`},
		{name: "chat duplicate indices", operation: contract.OperationChat, payload: `{"id":"chat","choices":[{"index":0,"message":{"role":"assistant","content":"one"}},{"index":0,"message":{"role":"assistant","content":"two"}}]}`},
		{name: "chat unsupported content", operation: contract.OperationChat, payload: `{"id":"chat","choices":[{"message":{"role":"assistant","content":{"text":"hidden"}}}]}`},
		{name: "responses missing id", operation: contract.OperationResponses, payload: `{"status":"completed"}`},
		{name: "responses missing status", operation: contract.OperationResponses, payload: `{"id":"resp","output":[]}`},
		{name: "responses missing output", operation: contract.OperationResponses, payload: `{"id":"resp","status":"completed"}`},
		{name: "compaction wrong object", operation: contract.OperationResponseCompact, payload: `{"id":"resp","object":"response","created_at":1,"output":[]}`},
		{name: "compaction missing creation time", operation: contract.OperationResponseCompact, payload: `{"id":"resp","object":"response.compaction","output":[]}`},
		{name: "compaction missing encrypted content", operation: contract.OperationResponseCompact, payload: `{"id":"resp","object":"response.compaction","created_at":1,"output":[{"type":"compaction","id":"cmp"}]}`},
		{name: "embeddings missing data", operation: contract.OperationEmbeddings, payload: `{"data":[]}`},
		{name: "embedding null value", operation: contract.OperationEmbeddings, payload: `{"data":[{"index":0,"embedding":null}]}`},
		{name: "embedding empty vector", operation: contract.OperationEmbeddings, payload: `{"data":[{"index":0,"embedding":[]}]}`},
		{name: "embedding empty base64", operation: contract.OperationEmbeddings, payload: `{"data":[{"index":0,"embedding":""}]}`},
		{name: "embedding non-contiguous index", operation: contract.OperationEmbeddings, payload: `{"data":[{"index":1,"embedding":[0.1]}]}`},
		{name: "image missing content", operation: contract.OperationImageGeneration, payload: `{"data":[{}]}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := decodeResponse(test.operation, []byte(test.payload))
			var typed *contract.Error
			if !errors.As(err, &typed) || typed.Code != contract.ErrorProvider || typed.HTTPStatus != http.StatusBadGateway || !typed.Retryable {
				t.Fatalf("malformed response error = %#v", err)
			}
			if typed.Safe().Cause != nil {
				t.Fatalf("safe malformed-response error retained cause: %#v", typed.Safe())
			}
		})
	}
}

func TestProviderResponseCardinalityMustMatchRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/v1/chat/completions":
			_, _ = io.WriteString(response, `{"id":"chat","choices":[{"index":0,"message":{"role":"assistant","content":"one"}}],"usage":{"total_tokens":1}}`)
		case "/v1/embeddings":
			_, _ = io.WriteString(response, `{"data":[{"index":0,"embedding":[0.1]}],"usage":{"total_tokens":1}}`)
		case "/v1/images/generations":
			_, _ = io.WriteString(response, `{"data":[{"b64_json":"AAAA"}],"usage":{"total_tokens":1}}`)
		}
	}))
	defer server.Close()
	upstream := buildAdapter(t, server, `{}`, Factory{})
	requests := []contract.Request{
		{Operation: contract.OperationChat, Chat: &contract.ChatRequest{N: 2, Messages: []contract.Message{{Role: "user", Content: []contract.ContentPart{{Type: "text", Text: "hi"}}}}}},
		{Operation: contract.OperationEmbeddings, Embeddings: &contract.EmbeddingsRequest{Input: contract.EmbeddingInput{Texts: []string{"one", "two"}}, EncodingFormat: "float"}},
		{Operation: contract.OperationImageGeneration, ImageGeneration: &contract.ImageGenerationRequest{Prompt: "circle", N: 2, ResponseFormat: "b64_json"}},
	}
	for _, request := range requests {
		if _, err := upstream.Invoke(context.Background(), deployment(), request); err == nil {
			t.Fatalf("%s response cardinality mismatch was accepted", request.Operation)
		}
	}
}

func TestResponseBodyAndSSEFramesAreBounded(t *testing.T) {
	t.Run("body", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(response, strings.Repeat("x", 65))
		}))
		defer server.Close()
		adapter := buildAdapter(t, server, `{}`, Factory{MaxResponseBytes: 64})
		_, err := adapter.Invoke(context.Background(), deployment(), contract.Request{Operation: contract.OperationEmbeddings, Embeddings: &contract.EmbeddingsRequest{Input: contract.EmbeddingInput{Texts: []string{"hi"}}}})
		if err == nil {
			t.Fatal("expected bounded response failure")
		}
	})
	t.Run("SSE", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
			response.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(response, "data: "+strings.Repeat("x", 65)+"\n\n")
		}))
		defer server.Close()
		adapter := buildAdapter(t, server, `{}`, Factory{MaxEventBytes: 64})
		stream, err := adapter.Stream(context.Background(), deployment(), contract.Request{Operation: contract.OperationChat, Stream: true, Chat: &contract.ChatRequest{Messages: []contract.Message{{Role: "user", Content: []contract.ContentPart{{Type: "text", Text: "hi"}}}}}})
		if err != nil {
			t.Fatal(err)
		}
		defer stream.Close()
		if _, err := stream.Next(context.Background()); err == nil {
			t.Fatal("expected bounded SSE failure")
		}
	})
}

func TestChatStreamPreservesToolDeltasUsageAndDone(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "text/event-stream")
		flusher := response.(http.Flusher)
		for _, frame := range []string{
			`data: {"id":"chat_s","created":7,"choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"weather","arguments":"{\"city\":"}}]},"finish_reason":null}]}` + "\n\n",
			`data: {"id":"chat_s","created":7,"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"Pune\"}"}}]},"finish_reason":"tool_calls"}]}` + "\n\n",
			`data: {"choices":[],"usage":{"prompt_tokens":4,"completion_tokens":2,"total_tokens":6}}` + "\n\n",
			"data: [DONE]\n\n",
		} {
			_, _ = io.WriteString(response, frame)
			flusher.Flush()
		}
	}))
	defer server.Close()
	adapter := buildAdapter(t, server, `{}`, Factory{})
	stream, err := adapter.Stream(context.Background(), deployment(), contract.Request{Operation: contract.OperationChat, Stream: true, Chat: &contract.ChatRequest{Messages: []contract.Message{{Role: "user", Content: []contract.ContentPart{{Type: "text", Text: "hi"}}}}}})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	first, err := stream.Next(context.Background())
	if err != nil || first.Type != "tool_call_delta" || first.Chat.ToolCalls[0].Function.Name != "weather" {
		t.Fatalf("first=%#v err=%v", first, err)
	}
	if _, err := stream.Next(context.Background()); err != nil {
		t.Fatal(err)
	}
	usage, err := stream.Next(context.Background())
	if err != nil || usage.Usage.TotalTokens != 6 {
		t.Fatalf("usage=%#v err=%v", usage, err)
	}
	done, err := stream.Next(context.Background())
	if err != nil || !done.Terminal || done.Usage.TotalTokens != 6 {
		t.Fatalf("done=%#v err=%v", done, err)
	}
	if _, err := stream.Next(context.Background()); !errors.Is(err, io.EOF) {
		t.Fatalf("expected EOF, got %v", err)
	}
}

func TestChatStreamQueuesEveryChoiceFromOneFrame(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(response, "data: {\"id\":\"chat_n\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"one\"}},{\"index\":1,\"delta\":{\"content\":\"two\"}}],\"usage\":{\"total_tokens\":2}}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	upstream := buildAdapter(t, server, `{}`, Factory{})
	stream, err := upstream.Stream(context.Background(), deployment(), contract.Request{
		Operation: contract.OperationChat, Stream: true,
		Chat: &contract.ChatRequest{N: 2, Messages: []contract.Message{{Role: "user", Content: []contract.ContentPart{{Type: "text", Text: "hi"}}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	first, err := stream.Next(context.Background())
	if err != nil || first.Chat == nil || first.Chat.Index != 0 || first.Usage != nil {
		t.Fatalf("first=%#v err=%v", first, err)
	}
	second, err := stream.Next(context.Background())
	if err != nil || second.Chat == nil || second.Chat.Index != 1 || second.Usage == nil || second.Usage.TotalTokens != 2 {
		t.Fatalf("second=%#v err=%v", second, err)
	}
	done, err := stream.Next(context.Background())
	if err != nil || !done.Terminal || done.Usage == nil || done.Usage.TotalTokens != 2 {
		t.Fatalf("done=%#v err=%v", done, err)
	}
}

func TestMalformedStreamEventsFailWithoutRetainingProviderPayload(t *testing.T) {
	events, err := decodeChatEvents([]byte(`{"id":"chat","choices":[{"index":0,"delta":{"content":"one"}},{"index":1,"delta":{"content":"two"}}],"usage":{"total_tokens":2}}`))
	if err != nil || len(events) != 2 || events[0].Chat.Index != 0 || events[1].Chat.Index != 1 || events[0].Usage != nil || events[1].Usage.TotalTokens != 2 {
		t.Fatalf("multi-choice chat frame = %#v, %v", events, err)
	}
	for _, payload := range []string{
		`{"choices":[{"index":0},{"index":0}]}`,
		`{"choices":[{"index":-1}]}`,
	} {
		if _, err := decodeChatEvents([]byte(payload)); err == nil {
			t.Fatalf("accepted invalid chat choice indices: %s", payload)
		}
	}
	_, err = decodeResponsesEvent("error", []byte(`{"type":"error","sequence_number":1,"error":{"message":"raw provider secret"}}`))
	var typed *contract.Error
	if !errors.As(err, &typed) || typed.Cause != nil || strings.Contains(typed.Error(), "raw provider") {
		t.Fatalf("responses stream error retained provider payload: %#v", err)
	}
	for _, test := range []struct {
		name    string
		payload string
	}{
		{name: "unknown type", payload: `{"type":"response.vendor.delta","sequence_number":1}`},
		{name: "conflicting type", payload: `{"type":"response.output_text.done","sequence_number":1,"item_id":"msg","output_index":0,"content_index":0}`},
		{name: "missing coordinates", payload: `{"type":"response.output_text.delta","sequence_number":1,"delta":"secret"}`},
		{name: "unsupported item", payload: `{"type":"response.output_item.added","sequence_number":1,"output_index":0,"item":{"id":"item","type":"computer_call","status":"in_progress"}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			name := ""
			if test.name == "conflicting type" {
				name = "response.output_text.delta"
			}
			if _, decodeErr := decodeResponsesEvent(name, []byte(test.payload)); decodeErr == nil {
				t.Fatalf("accepted malformed Responses event: %s", test.payload)
			}
		})
	}
}

func TestResponsesStreamEventsPreserveTypedFields(t *testing.T) {
	delta, err := decodeResponsesEvent("response.output_text.delta", []byte(`{
		"type":"response.output_text.delta","sequence_number":3,"item_id":"msg_1","output_index":1,"content_index":2,
		"delta":"hello","logprobs":[{"token":"hello","logprob":-0.1,"top_logprobs":[]}]
	}`))
	if err != nil || delta.Response == nil || delta.Response.Sequence != 3 || delta.Response.ItemID != "msg_1" ||
		delta.Response.OutputIndex != 1 || delta.Response.ContentIndex != 2 || delta.Response.Delta != "hello" || len(delta.Response.Logprobs) == 0 {
		t.Fatalf("text delta=%#v err=%v", delta, err)
	}

	item, err := decodeResponsesEvent("", []byte(`{
		"type":"response.output_item.added","sequence_number":4,"output_index":0,
		"item":{"id":"call_1","type":"function_call","status":"in_progress","call_id":"call_public","name":"weather","arguments":""}
	}`))
	if err != nil || item.Response == nil || item.Response.Item == nil || item.Response.Item.CallID != "call_public" || item.Response.Item.Name != "weather" {
		t.Fatalf("output item=%#v err=%v", item, err)
	}

	part, err := decodeResponsesEvent("", []byte(`{
		"type":"response.content_part.done","sequence_number":5,"item_id":"msg_1","output_index":0,"content_index":0,
		"part":{"type":"refusal","refusal":"cannot comply"}
	}`))
	if err != nil || part.Response == nil || part.Response.Part == nil || part.Response.Part.Type != "refusal" || part.Response.Part.Refusal != "cannot comply" {
		t.Fatalf("content part=%#v err=%v", part, err)
	}

	reasoningPart, err := decodeResponsesEvent("", []byte(`{
		"type":"response.reasoning_part.added","sequence_number":6,"item_id":"reason_1","output_index":0,"content_index":0,
		"part":{"type":"reasoning_text","text":""}
	}`))
	if err != nil || reasoningPart.Response == nil || reasoningPart.Response.Part == nil || reasoningPart.Response.Part.Type != "reasoning_text" {
		t.Fatalf("reasoning part=%#v err=%v", reasoningPart, err)
	}

	completed, err := decodeResponsesEvent("", []byte(`{
		"type":"response.completed","sequence_number":7,
		"response":{"id":"resp_1","model":"upstream","status":"completed","output":[
			{"id":"msg_1","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"hello","annotations":[],"logprobs":[]}]}
		],"usage":{"input_tokens":2,"output_tokens":1,"total_tokens":3}}
	}`))
	if err != nil || !completed.Terminal || completed.Usage == nil || completed.Usage.TotalTokens != 3 || completed.Response == nil ||
		completed.Response.Snapshot == nil || completed.Response.Snapshot.Responses.Output[0].Content[0].Text != "hello" {
		t.Fatalf("completed=%#v err=%v", completed, err)
	}
}

func TestResponsesStreamRejectsNonIncreasingSequenceNumbers(t *testing.T) {
	stream := newEventStream(io.NopCloser(strings.NewReader(
		"event: response.output_text.delta\n"+
			"data: {\"type\":\"response.output_text.delta\",\"sequence_number\":1,\"item_id\":\"msg_1\",\"output_index\":0,\"content_index\":0,\"delta\":\"one\"}\n\n"+
			"event: response.output_text.done\n"+
			"data: {\"type\":\"response.output_text.done\",\"sequence_number\":1,\"item_id\":\"msg_1\",\"output_index\":0,\"content_index\":0,\"text\":\"one\"}\n\n",
	)), contract.OperationResponses, 4096)
	defer stream.Close()
	if _, err := stream.Next(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Next(context.Background()); err == nil {
		t.Fatal("accepted a duplicate Responses sequence number")
	}
}

func TestStreamEOFBeforeDoneIsNormalizedAsTransientProviderFailure(t *testing.T) {
	stream := newEventStream(io.NopCloser(strings.NewReader("")), contract.OperationChat, 1024)
	_, err := stream.Next(context.Background())
	var typed *contract.Error
	if !errors.As(err, &typed) || typed.Code != contract.ErrorProvider || typed.HTTPStatus != http.StatusBadGateway || !typed.Retryable {
		t.Fatalf("premature stream EOF = %#v", err)
	}
}

func TestRequestTimeoutCoversResponseBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		response.WriteHeader(http.StatusOK)
		response.(http.Flusher).Flush()
		<-time.After(100 * time.Millisecond)
		_, _ = io.WriteString(response, `{}`)
	}))
	defer server.Close()
	adapter := buildAdapter(t, server, `{}`, Factory{})
	deployment := deployment()
	deployment.RequestTimeout = 10 * time.Millisecond
	_, err := adapter.Invoke(context.Background(), deployment, contract.Request{Operation: contract.OperationEmbeddings, Embeddings: &contract.EmbeddingsRequest{Input: contract.EmbeddingInput{Texts: []string{"hi"}}}})
	var typed *contract.Error
	if !errors.As(err, &typed) || typed.Code != contract.ErrorTimeout || typed.HTTPStatus != http.StatusGatewayTimeout || !typed.Retryable {
		t.Fatalf("response-body timeout = %#v, want retryable timeout error", err)
	}
}

func TestConnectTimeoutDoesNotLimitInferenceTime(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		time.Sleep(30 * time.Millisecond)
		response.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(response, `{"data":[{"index":0,"embedding":[0.1]}]}`)
	}))
	defer server.Close()
	adapter := buildAdapter(t, server, `{}`, Factory{})
	configured := deployment()
	configured.ConnectTimeout = 5 * time.Millisecond
	configured.RequestTimeout = time.Second
	result, err := adapter.Invoke(context.Background(), configured, contract.Request{Operation: contract.OperationEmbeddings,
		Embeddings: &contract.EmbeddingsRequest{Input: contract.EmbeddingInput{Texts: []string{"hi"}}}})
	if err != nil || result.Embeddings == nil {
		t.Fatalf("connect timeout incorrectly bounded inference: result=%#v err=%v", result, err)
	}
}

func TestRequestCancellationCoversResponseBody(t *testing.T) {
	headersWritten := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		response.WriteHeader(http.StatusOK)
		response.(http.Flusher).Flush()
		close(headersWritten)
		<-request.Context().Done()
	}))
	defer server.Close()
	adapter := buildAdapter(t, server, `{}`, Factory{})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-headersWritten
		cancel()
	}()
	_, err := adapter.Invoke(ctx, deployment(), contract.Request{Operation: contract.OperationEmbeddings,
		Embeddings: &contract.EmbeddingsRequest{Input: contract.EmbeddingInput{Texts: []string{"hi"}}}})
	var typed *contract.Error
	if !errors.As(err, &typed) || typed.Code != contract.ErrorProvider || typed.HTTPStatus != 499 || typed.Retryable {
		t.Fatalf("response-body cancellation = %#v, want non-retryable cancellation", err)
	}
}
