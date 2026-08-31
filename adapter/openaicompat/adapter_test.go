package openaicompat

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	baseadapter "github.com/daptin/llmgateway/adapter"
	"github.com/daptin/llmgateway/catalog"
	"github.com/daptin/llmgateway/compatibility"
	"github.com/daptin/llmgateway/contract"
)

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
			body["frequency_penalty"] != -0.5 || body["presence_penalty"] != 1.25 || body["parallel_tool_calls"] != false || body["reasoning_effort"] != "low" {
			t.Fatalf("unexpected body: %#v", body)
		}
		response.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(response, `{"id":"chatcmpl_1","created":123,"model":"actual-model","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"weather","arguments":"{\"city\":\"Pune\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":4,"total_tokens":14,"prompt_tokens_details":{"cached_tokens":2}}}`)
	}))
	defer server.Close()
	adapter := buildAdapter(t, server, `{"organization":"org"}`, Factory{})
	frequency, presence, parallel := -0.5, 1.25, false
	request := contract.Request{Operation: contract.OperationChat, Chat: &contract.ChatRequest{
		Messages: []contract.Message{{Role: "user", Content: []contract.ContentPart{{Type: "text", Text: "weather"}}}}, MaxCompletionTokens: 20,
		Tools:            []contract.Tool{{Type: "function", Function: contract.FunctionDefinition{Name: "weather", Parameters: json.RawMessage(`{"type":"object"}`)}}},
		FrequencyPenalty: &frequency, PresencePenalty: &presence, ParallelToolCalls: &parallel, ReasoningEffort: "low",
	}}
	result, err := adapter.Invoke(context.Background(), deployment(), request)
	if err != nil {
		t.Fatal(err)
	}
	if result.Chat == nil || result.Chat.ID != "chatcmpl_1" || result.Chat.Choices[0].Message.ToolCalls[0].Function.Name != "weather" {
		t.Fatalf("unexpected canonical response: %#v", result)
	}
	if result.Usage.TotalTokens != 14 || result.Usage.CacheReadTokens != 2 {
		t.Fatalf("unexpected usage: %#v", result.Usage)
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
		if request.Method != http.MethodGet || request.URL.Path != "/v1/health/probe-model" {
			t.Fatalf("unexpected health request: %s %s", request.Method, request.URL.Path)
		}
		response.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	built := buildAdapter(t, server, `{}`, Factory{})
	configured := deployment()
	configured.HealthCheck = catalog.HealthCheck{Enabled: true, Path: "/health/{model}", Model: "probe-model"}
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
			if body["temperature"] != 0.4 || body["top_p"] != 0.8 || body["parallel_tool_calls"] != false || reasoning["effort"] != "low" {
				t.Fatalf("Responses controls were not preserved: %#v", body)
			}
			_, _ = io.WriteString(response, `{"id":"resp_1","model":"m","status":"completed","output":[{"type":"reasoning","id":"reason_1","status":"completed","summary":[{"type":"summary_text","text":"brief"}]},{"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"hello"}]}],"usage":{"input_tokens":2,"output_tokens":1,"total_tokens":3}}`)
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
	tests := []struct {
		name    string
		request contract.Request
		check   func(contract.Response) bool
	}{
		{name: "responses", request: contract.Request{Operation: contract.OperationResponses, Responses: &contract.ResponsesRequest{Input: []contract.ResponseInputItem{{Type: "message", Role: "user", Content: []contract.ContentPart{{Type: "input_text", Text: "hi"}}}}, Tools: []contract.Tool{{Type: "function", Function: contract.FunctionDefinition{Name: "weather", Parameters: json.RawMessage(`{"type":"object"}`)}}}, Temperature: &temperature, TopP: &topP, ParallelToolCalls: &parallel, ReasoningEffort: "low"}}, check: func(value contract.Response) bool {
			return value.Responses != nil && value.Responses.Output[0].Summary[0].Text == "brief" && value.Responses.Output[1].Content[0].Text == "hello"
		}},
		{name: "embeddings", request: contract.Request{Operation: contract.OperationEmbeddings, Embeddings: &contract.EmbeddingsRequest{Input: contract.EmbeddingInput{Texts: []string{"hi"}}, EncodingFormat: "float"}}, check: func(value contract.Response) bool {
			return value.Embeddings != nil && len(value.Embeddings.Data[0].Vector) == 2
		}},
		{name: "images", request: contract.Request{Operation: contract.OperationImageGeneration, ImageGeneration: &contract.ImageGenerationRequest{Prompt: "cat", N: 1, ResponseFormat: "b64_json"}}, check: func(value contract.Response) bool {
			return value.ImageGeneration != nil && value.ImageGeneration.Data[0].Base64 == "AAAA" && value.Usage.TotalTokens == 10
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
	if err == nil || redirected.Load() != 0 {
		t.Fatalf("redirect error=%v followed=%d", err, redirected.Load())
	}
}

func TestPrivateIPRejectsCarrierGradeNATAndMulticast(t *testing.T) {
	for _, value := range []string{"100.64.0.1", "100.127.255.254", "224.0.0.1", "ff02::1"} {
		if !privateIP(net.ParseIP(value)) {
			t.Fatalf("%s was not classified as private", value)
		}
	}
}

func TestRetryAfterSaturatesWithoutOverflow(t *testing.T) {
	if got := retryAfter("9223372036854775807", time.Now()); got != time.Duration(1<<63-1) {
		t.Fatalf("retry-after duration = %s", got)
	}
}

func TestProviderErrorsAreNormalizedAndSecretSafe(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(response, `{"error":{"message":"raw provider secret detail"}}`)
	}))
	defer server.Close()
	adapter := buildAdapter(t, server, `{}`, Factory{})
	_, err := adapter.Invoke(context.Background(), deployment(), contract.Request{Operation: contract.OperationEmbeddings, Embeddings: &contract.EmbeddingsRequest{Input: contract.EmbeddingInput{Texts: []string{"hi"}}}})
	var typed *contract.Error
	if !errors.As(err, &typed) || typed.Code != contract.ErrorRateLimit || !typed.Retryable {
		t.Fatalf("unexpected error: %#v", err)
	}
	if strings.Contains(typed.Safe().Error(), "raw provider") || strings.Contains(typed.Safe().Error(), "secret-key") {
		t.Fatalf("public error leaked provider data: %v", typed.Safe())
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
	if err == nil {
		t.Fatal("expected response-body timeout")
	}
}
