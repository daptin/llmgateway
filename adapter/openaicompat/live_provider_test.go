//go:build live

package openaicompat

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	baseadapter "github.com/daptin/llmgateway/adapter"
	"github.com/daptin/llmgateway/catalog"
	"github.com/daptin/llmgateway/contract"
)

type liveMatrix struct {
	Providers []liveProvider `json:"providers"`
}

type liveProvider struct {
	Name               string          `json:"name"`
	APIVersion         string          `json:"api_version"`
	APIKeyEnv          string          `json:"api_key_env"`
	ProviderParameters json.RawMessage `json:"provider_parameters,omitempty"`
	Cases              []liveCase      `json:"cases"`
}

type liveCase struct {
	Name    string `json:"name"`
	Feature string `json:"feature"`
	Model   string `json:"model"`
}

type liveCaseResult struct {
	UsageAvailable bool
}

func TestLiveProviderMatrix(t *testing.T) {
	matrix := loadLiveMatrix(t)
	for _, provider := range matrix.Providers {
		provider := provider
		t.Run(provider.Name, func(t *testing.T) {
			key := os.Getenv(provider.APIKeyEnv)
			if key == "" {
				t.Fatalf("%s is required for the live provider gate", provider.APIKeyEnv)
			}
			built, err := (Factory{}).Build(context.Background(), catalog.Provider{
				ID: contract.ID(provider.Name), Name: provider.Name, Type: provider.Name,
				SecretRef: provider.APIKeyEnv, Parameters: provider.ProviderParameters, Enabled: true,
			}, baseadapter.NewSecret([]byte(key)))
			if err != nil {
				t.Fatal(err)
			}
			upstream := built.(*Adapter)
			defer upstream.CloseIdleConnections()
			for _, testCase := range provider.Cases {
				testCase := testCase
				t.Run(testCase.Name+"/"+testCase.Model, func(t *testing.T) {
					ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
					defer cancel()
					result, err := runLiveCase(ctx, upstream, testCase)
					if err != nil {
						normalized := "unclassified_error"
						var public *contract.Error
						if errors.As(err, &public) {
							normalized = string(public.Code)
						}
						t.Logf("certification provider=%s api_version=%s model=%s feature=%s normalized_result=%s usage_available=%v skip_reason=none",
							provider.Name, provider.APIVersion, testCase.Model, testCase.Feature, normalized, result.UsageAvailable)
						t.Fatalf("provider=%s model=%s feature=%s: %v; provider_cause=%v", provider.Name, testCase.Model, testCase.Feature, err, errors.Unwrap(err))
					}
					t.Logf("certification provider=%s api_version=%s model=%s feature=%s normalized_result=success usage_available=%v skip_reason=none",
						provider.Name, provider.APIVersion, testCase.Model, testCase.Feature, result.UsageAvailable)
				})
			}
			t.Run("invalid-credential", func(t *testing.T) {
				chatModel := ""
				for _, testCase := range provider.Cases {
					if strings.HasPrefix(testCase.Feature, "chat") {
						chatModel = testCase.Model
						break
					}
				}
				if chatModel == "" {
					t.Fatal("live provider has no chat model for credential rejection certification")
				}
				const invalidCredential = "llmgateway-intentionally-invalid"
				invalidBuilt, err := (Factory{}).Build(context.Background(), catalog.Provider{
					ID: contract.ID(provider.Name + "-invalid"), Name: provider.Name, Type: provider.Name,
					SecretRef: "invalid-live-credential", Parameters: provider.ProviderParameters, Enabled: true,
				}, baseadapter.NewSecret([]byte(invalidCredential)))
				if err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				invalidUpstream := invalidBuilt.(*Adapter)
				defer invalidUpstream.CloseIdleConnections()
				_, invokeErr := invalidUpstream.Invoke(ctx,
					catalog.Deployment{ID: "invalid-live", Name: "invalid-live", UpstreamModel: chatModel, RequestTimeout: 20 * time.Second, ConnectTimeout: 10 * time.Second},
					liveChatRequest(chatModel, "chat"),
				)
				var public *contract.Error
				if invokeErr == nil || !errors.As(invokeErr, &public) || public.Retryable ||
					(public.Code != contract.ErrorAuthentication && public.Code != contract.ErrorPermission && public.Code != contract.ErrorInvalidRequest) ||
					strings.Contains(invokeErr.Error(), invalidCredential) {
					t.Fatalf("provider credential rejection was not safely normalized: %v", invokeErr)
				}
				t.Logf("certification provider=%s api_version=%s model=%s feature=invalid_credential normalized_result=%s usage_available=false skip_reason=none",
					provider.Name, provider.APIVersion, chatModel, public.Code)
			})
		})
	}
}

func TestLiveMatrixDefinition(t *testing.T) {
	loadLiveMatrix(t)
}

func loadLiveMatrix(t *testing.T) liveMatrix {
	t.Helper()
	path := os.Getenv("LLMGATEWAY_LIVE_MATRIX")
	if path == "" {
		path = filepath.Join("testdata", "live_matrix.json")
	}
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var matrix liveMatrix
	decoder := json.NewDecoder(strings.NewReader(string(payload)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&matrix); err != nil || len(matrix.Providers) == 0 {
		t.Fatalf("invalid live provider matrix: %v", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		t.Fatalf("invalid trailing live provider matrix content: %v", err)
	}
	validFeatures := map[string]bool{
		"chat": true, "chat_tools": true, "chat_parallel_tools": true, "chat_structured": true,
		"chat_reasoning": true, "chat_vision": true, "chat_stream": true,
		"responses": true, "responses_stream": true, "embedding_string": true, "embedding_array": true,
		"embedding_tokens": true, "embedding_token_arrays": true, "image": true,
	}
	providerNames := make(map[string]bool, len(matrix.Providers))
	for _, provider := range matrix.Providers {
		if provider.Name == "" || provider.APIVersion == "" || provider.APIKeyEnv == "" || len(provider.Cases) == 0 {
			t.Fatalf("incomplete live provider definition: %+v", provider)
		}
		if providerNames[provider.Name] {
			t.Fatalf("duplicate live provider %q", provider.Name)
		}
		providerNames[provider.Name] = true
		caseNames := make(map[string]bool, len(provider.Cases))
		for _, testCase := range provider.Cases {
			if testCase.Name == "" || testCase.Model == "" || !validFeatures[testCase.Feature] {
				t.Fatalf("invalid %s live case: %+v", provider.Name, testCase)
			}
			if caseNames[testCase.Name] {
				t.Fatalf("duplicate %s live case %q", provider.Name, testCase.Name)
			}
			caseNames[testCase.Name] = true
		}
	}
	return matrix
}

func runLiveCase(ctx context.Context, upstream *Adapter, testCase liveCase) (liveCaseResult, error) {
	deployment := catalog.Deployment{ID: "live", Name: "live", UpstreamModel: testCase.Model, RequestTimeout: 90 * time.Second, ConnectTimeout: 10 * time.Second}
	switch testCase.Feature {
	case "chat", "chat_tools", "chat_parallel_tools", "chat_structured", "chat_reasoning", "chat_vision", "chat_stream":
		request := liveChatRequest(testCase.Model, testCase.Feature)
		if testCase.Feature == "chat_stream" {
			request.Stream = true
			return consumeLiveStream(ctx, upstream, deployment, request)
		}
		response, err := upstream.Invoke(ctx, deployment, request)
		if err != nil {
			return liveCaseResult{}, err
		}
		if response.Chat == nil || len(response.Chat.Choices) == 0 || !response.Usage.Valid() {
			return liveCaseResult{}, errors.New("invalid chat response")
		}
		if testCase.Feature == "chat_tools" && len(response.Chat.Choices[0].Message.ToolCalls) == 0 {
			return liveCaseResult{}, errors.New("model did not return a tool call")
		}
		if testCase.Feature == "chat_parallel_tools" && len(response.Chat.Choices[0].Message.ToolCalls) < 2 {
			return liveCaseResult{}, errors.New("model did not return parallel tool calls")
		}
		if testCase.Feature == "chat_structured" {
			if len(response.Chat.Choices[0].Message.Content) == 0 || !json.Valid([]byte(response.Chat.Choices[0].Message.Content[0].Text)) {
				return liveCaseResult{}, errors.New("model did not return structured JSON")
			}
		}
		return liveCaseResult{UsageAvailable: liveUsageAvailable(response.Usage)}, nil
	case "responses", "responses_stream":
		request := contract.Request{Operation: contract.OperationResponses, Stream: testCase.Feature == "responses_stream", MaxOutputTokens: 64, Responses: &contract.ResponsesRequest{
			Input: []contract.ResponseInputItem{{Type: "message", Role: "user", Content: []contract.ContentPart{{Type: "input_text", Text: "Reply with the single word pong."}}}},
		}}
		if request.Stream {
			return consumeLiveStream(ctx, upstream, deployment, request)
		}
		response, err := upstream.Invoke(ctx, deployment, request)
		if err != nil {
			return liveCaseResult{}, err
		}
		if response.Responses == nil || response.Responses.ID == "" || len(response.Responses.Output) == 0 || !response.Usage.Valid() {
			return liveCaseResult{}, errors.New("invalid Responses response")
		}
		return liveCaseResult{UsageAvailable: liveUsageAvailable(response.Usage)}, nil
	case "embedding_string", "embedding_array", "embedding_tokens", "embedding_token_arrays":
		input := contract.EmbeddingInput{Texts: []string{"hello"}}
		switch testCase.Feature {
		case "embedding_array":
			input.Texts = []string{"hello", "world"}
		case "embedding_tokens":
			input = contract.EmbeddingInput{Tokens: [][]int64{{15339}}}
		case "embedding_token_arrays":
			input = contract.EmbeddingInput{Tokens: [][]int64{{15339}, {14957}}}
		}
		response, err := upstream.Invoke(ctx, deployment, contract.Request{Operation: contract.OperationEmbeddings, Embeddings: &contract.EmbeddingsRequest{Input: input, EncodingFormat: "float"}})
		if err != nil {
			return liveCaseResult{}, err
		}
		if response.Embeddings == nil || len(response.Embeddings.Data) != len(input.Texts)+len(input.Tokens) || !response.Usage.Valid() {
			return liveCaseResult{}, errors.New("invalid embeddings response cardinality")
		}
		return liveCaseResult{UsageAvailable: liveUsageAvailable(response.Usage)}, nil
	case "image":
		response, err := upstream.Invoke(ctx, deployment, contract.Request{Operation: contract.OperationImageGeneration, ImageGeneration: &contract.ImageGenerationRequest{Prompt: "A small red circle centered on a white background", N: 1, Size: "1024x1024", ResponseFormat: "b64_json"}})
		if err != nil {
			return liveCaseResult{}, err
		}
		if response.ImageGeneration == nil || len(response.ImageGeneration.Data) == 0 {
			return liveCaseResult{}, errors.New("invalid image response")
		}
		image := response.ImageGeneration.Data[0]
		if image.Base64 != "" {
			if decoded, err := base64.StdEncoding.DecodeString(image.Base64); err != nil || len(decoded) == 0 {
				return liveCaseResult{}, errors.New("invalid base64 image")
			}
		} else if !strings.HasPrefix(image.URL, "http") {
			return liveCaseResult{}, errors.New("image response has no usable content")
		}
		return liveCaseResult{UsageAvailable: liveUsageAvailable(response.Usage)}, nil
	default:
		return liveCaseResult{}, fmt.Errorf("unknown live feature %q", testCase.Feature)
	}
}

func liveChatRequest(_ string, feature string) contract.Request {
	zero := 0.0
	message := contract.Message{Role: "user", Content: []contract.ContentPart{{Type: "text", Text: "Reply with the single word pong."}}}
	request := contract.Request{Operation: contract.OperationChat, MaxOutputTokens: 64, Chat: &contract.ChatRequest{Messages: []contract.Message{message}, N: 1, Temperature: &zero, MaxCompletionTokens: 64}}
	switch feature {
	case "chat_tools":
		request.Chat.Messages[0].Content[0].Text = "Call get_weather for Pune. Do not answer directly."
		request.Chat.Tools = []contract.Tool{{Type: "function", Function: contract.FunctionDefinition{Name: "get_weather", Description: "Get weather", Parameters: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`)}}}
		request.Chat.ToolChoice = &contract.ToolChoice{Mode: "function", FunctionName: "get_weather"}
	case "chat_parallel_tools":
		request.Chat.Messages[0].Content[0].Text = "Call get_weather once for Pune and once for Mumbai. Return both calls together and do not answer directly."
		request.Chat.Tools = []contract.Tool{{Type: "function", Function: contract.FunctionDefinition{Name: "get_weather", Description: "Get weather for one city", Parameters: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`)}}}
		parallel := true
		request.Chat.ParallelToolCalls = &parallel
	case "chat_reasoning":
		request.Chat.Messages[0].Content[0].Text = "Reply with the single word pong after checking that 17 plus 25 equals 42."
		request.Chat.ReasoningEffort = "low"
	case "chat_structured":
		request.MaxOutputTokens = 512
		request.Chat.MaxCompletionTokens = 512
		request.Chat.Messages[0].Content[0].Text = "Return the color red."
		strict := true
		request.Chat.ResponseFormat = &contract.ResponseFormat{Type: "json_schema", JSONSchema: &contract.JSONSchema{Name: "color", Strict: &strict, Schema: json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"}},"required":["name"],"additionalProperties":false}`)}}
	case "chat_vision":
		request.Chat.Messages[0].Content = []contract.ContentPart{
			{Type: "text", Text: "What colors are in this image?"},
			{Type: "image_url", ImageURL: &contract.ImageURL{URL: "data:image/jpeg;base64,/9j/4AAQSkZJRgABAQAAAQABAAD/2wBDABcQERQRDhcUEhQaGBcbIjklIh8fIkYyNSk5UkhXVVFIUE5bZoNvW2F8Yk5QcptzfIeLkpSSWG2grJ+OqoOPko3/2wBDARgaGiIeIkMlJUONXlBejY2NjY2NjY2NjY2NjY2NjY2NjY2NjY2NjY2NjY2NjY2NjY2NjY2NjY2NjY2NjY2NjY3/wAARCAIAAgADASIAAhEBAxEB/8QAFgABAQEAAAAAAAAAAAAAAAAAAAEF/8QAFBABAAAAAAAAAAAAAAAAAAAAAP/EABgBAQEBAQEAAAAAAAAAAAAAAAACAQQG/8QAFREBAQAAAAAAAAAAAAAAAAAAABH/2gAMAwEAAhEDEQA/AM0BwvVAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAIApIAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAApgAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAACAKSAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAKYAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAgDUgAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAACmAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAIApIAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAIKSogCiAKIAogCiAKIAogCiAKIAogCiAKIAogCiAKIAogCiAKIAogCiAKIAogCiAKIAogCiAKIAogCiAKIAogAA1gAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAApIAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAACAKjAAgAEAAgAEAAgAEAAgAEAAgAEAAgAEAAgAEAAgAEAAgAEAAgAEAAgAEAAgAEAAgAEAAgAEAAgAEAAgCCkqIAogCiAKIAogCiAKIAogCiAKIAogCiAKIAogCiAKIAogCiAKIAogCiAKIAogCiAKIAogCiAKIAogCiAKIAAKYAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAACDUqIAogCiAKIAogCiAKIAogCiAKIAogCiAKIAogCiAKIAogCiAKIAogCiAKIAogCiAKIAogCiAKIAogCiAKIAAKYAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAACCkqIAogCiAKIAogCiAKIAogCiAKIAogCiAKIAogCiAKIAogCiAKIAogCiAKIAogCiAKIAogCiAKIAogCiAKIAAKSAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAACCmKIAogCiAKIAogCiAKIAogCiAKIAogCiAKIAogCiAKIAogCiAKIAogCiAKIAogCiAKIAogCiAKIAogCiAKIAANSAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAACCmKIAogCiAKIAogCiAKIAogCiAKIAogCiAKIAogCiAKIAogCiAKIAogCiAKIAogCiAKIAogCiAKIAogCiAKIAAKSAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAACCmKIAogCiAKIAogCiAKIAogCiAKIAogCiAKIAogCiAKIAogCiAKIAogCiAKIAogCiAKIAogCiAKIAogCiAKIAANSAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAACCmKIAogCiAKIAogCiAKIAogCiAKIAogCiAKIAogCiAKIAogCiAKIAogCiAKIAogCiAKIAogCiAKIAogCiAKIAAKSAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAACCmKIAogCiAKIAogCiAKIAogCiAKIAogCiAKIAogCiAKIAogCiAKIAogCiAKIAogCiAKIAogCiAKIAogCiAKIAAKSAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAgDUgAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAACmAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAIApIAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAApgAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAD/9k="}},
		}
	}
	return request
}

func consumeLiveStream(ctx context.Context, upstream *Adapter, deployment catalog.Deployment, request contract.Request) (liveCaseResult, error) {
	stream, err := upstream.Stream(ctx, deployment, request)
	if err != nil {
		return liveCaseResult{}, err
	}
	defer stream.Close()
	semantic := false
	for count := 0; count < 10_000; count++ {
		event, err := stream.Next(ctx)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return liveCaseResult{}, errors.New("stream ended without a terminal event")
			}
			return liveCaseResult{}, err
		}
		if event.Chat != nil || event.Response != nil {
			semantic = true
		}
		if event.Terminal {
			if !semantic || event.Usage == nil || !event.Usage.Valid() {
				return liveCaseResult{}, errors.New("stream terminal event lacks semantic output or usage")
			}
			return liveCaseResult{UsageAvailable: liveUsageAvailable(*event.Usage)}, nil
		}
	}
	return liveCaseResult{}, errors.New("stream exceeded event bound")
}

func liveUsageAvailable(usage contract.Usage) bool {
	return usage.InputTokens != 0 || usage.OutputTokens != 0 || usage.CacheReadTokens != 0 || usage.CacheWriteTokens != 0 ||
		usage.ReasoningTokens != 0 || usage.TotalTokens != 0 || usage.CostMicros != 0
}
