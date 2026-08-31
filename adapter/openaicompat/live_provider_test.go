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
	APIKeyEnv          string          `json:"api_key_env"`
	ProviderParameters json.RawMessage `json:"provider_parameters,omitempty"`
	Cases              []liveCase      `json:"cases"`
}

type liveCase struct {
	Name    string `json:"name"`
	Feature string `json:"feature"`
	Model   string `json:"model"`
}

func TestLiveProviderMatrix(t *testing.T) {
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
			for _, testCase := range provider.Cases {
				testCase := testCase
				t.Run(testCase.Name+"/"+testCase.Model, func(t *testing.T) {
					ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
					defer cancel()
					if err := runLiveCase(ctx, upstream, testCase); err != nil {
						t.Fatalf("provider=%s model=%s feature=%s: %v", provider.Name, testCase.Model, testCase.Feature, err)
					}
				})
			}
		})
	}
}

func runLiveCase(ctx context.Context, upstream *Adapter, testCase liveCase) error {
	deployment := catalog.Deployment{ID: "live", Name: "live", UpstreamModel: testCase.Model, RequestTimeout: 90 * time.Second, ConnectTimeout: 10 * time.Second}
	switch testCase.Feature {
	case "chat", "chat_tools", "chat_structured", "chat_vision", "chat_stream":
		request := liveChatRequest(testCase.Model, testCase.Feature)
		if testCase.Feature == "chat_stream" {
			request.Stream = true
			return consumeLiveStream(ctx, upstream, deployment, request)
		}
		response, err := upstream.Invoke(ctx, deployment, request)
		if err != nil {
			return err
		}
		if response.Chat == nil || len(response.Chat.Choices) == 0 || !response.Usage.Valid() {
			return errors.New("invalid chat response")
		}
		if testCase.Feature == "chat_tools" && len(response.Chat.Choices[0].Message.ToolCalls) == 0 {
			return errors.New("model did not return a tool call")
		}
		if testCase.Feature == "chat_structured" {
			if len(response.Chat.Choices[0].Message.Content) == 0 || !json.Valid([]byte(response.Chat.Choices[0].Message.Content[0].Text)) {
				return errors.New("model did not return structured JSON")
			}
		}
		return nil
	case "responses", "responses_stream":
		request := contract.Request{Operation: contract.OperationResponses, Stream: testCase.Feature == "responses_stream", MaxOutputTokens: 64, Responses: &contract.ResponsesRequest{
			Input: []contract.ResponseInputItem{{Type: "message", Role: "user", Content: []contract.ContentPart{{Type: "input_text", Text: "Reply with the single word pong."}}}},
		}}
		if request.Stream {
			return consumeLiveStream(ctx, upstream, deployment, request)
		}
		response, err := upstream.Invoke(ctx, deployment, request)
		if err != nil {
			return err
		}
		if response.Responses == nil || response.Responses.ID == "" || len(response.Responses.Output) == 0 {
			return errors.New("invalid Responses response")
		}
		return nil
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
			return err
		}
		if response.Embeddings == nil || len(response.Embeddings.Data) != len(input.Texts)+len(input.Tokens) {
			return errors.New("invalid embeddings response cardinality")
		}
		return nil
	case "image":
		response, err := upstream.Invoke(ctx, deployment, contract.Request{Operation: contract.OperationImageGeneration, ImageGeneration: &contract.ImageGenerationRequest{Prompt: "A small red circle centered on a white background", N: 1, Size: "1024x1024", ResponseFormat: "b64_json"}})
		if err != nil {
			return err
		}
		if response.ImageGeneration == nil || len(response.ImageGeneration.Data) == 0 {
			return errors.New("invalid image response")
		}
		image := response.ImageGeneration.Data[0]
		if image.Base64 != "" {
			if decoded, err := base64.StdEncoding.DecodeString(image.Base64); err != nil || len(decoded) == 0 {
				return errors.New("invalid base64 image")
			}
		} else if !strings.HasPrefix(image.URL, "http") {
			return errors.New("image response has no usable content")
		}
		return nil
	default:
		return fmt.Errorf("unknown live feature %q", testCase.Feature)
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
	case "chat_structured":
		request.Chat.Messages[0].Content[0].Text = "Return the color red."
		strict := true
		request.Chat.ResponseFormat = &contract.ResponseFormat{Type: "json_schema", JSONSchema: &contract.JSONSchema{Name: "color", Strict: &strict, Schema: json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"}},"required":["name"],"additionalProperties":false}`)}}
	case "chat_vision":
		request.Chat.Messages[0].Content = []contract.ContentPart{
			{Type: "text", Text: "What color is this one-pixel image?"},
			{Type: "image_url", ImageURL: &contract.ImageURL{URL: "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAusB9Wl2nX8AAAAASUVORK5CYII="}},
		}
	}
	return request
}

func consumeLiveStream(ctx context.Context, upstream *Adapter, deployment catalog.Deployment, request contract.Request) error {
	stream, err := upstream.Stream(ctx, deployment, request)
	if err != nil {
		return err
	}
	defer stream.Close()
	semantic := false
	for count := 0; count < 10_000; count++ {
		event, err := stream.Next(ctx)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return errors.New("stream ended without a terminal event")
			}
			return err
		}
		if event.Chat != nil || event.Response != nil {
			semantic = true
		}
		if event.Terminal {
			if !semantic || event.Usage == nil || !event.Usage.Valid() {
				return errors.New("stream terminal event lacks semantic output or usage")
			}
			return nil
		}
	}
	return errors.New("stream exceeded event bound")
}
