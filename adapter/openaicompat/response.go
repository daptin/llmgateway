package openaicompat

import (
	"bytes"
	"encoding/json"

	"github.com/daptin/llmgateway/contract"
)

type usageWire struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	InputTokens      int64 `json:"input_tokens"`
	OutputTokens     int64 `json:"output_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
	PromptDetails    struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	InputDetails struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"input_tokens_details"`
	OutputDetails struct {
		ReasoningTokens int64 `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
}

type functionCallWire struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type toolCallWire struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function functionCallWire `json:"function"`
}

type chatResponseWire struct {
	ID      string `json:"id"`
	Created int64  `json:"created"`
	Model   string `json:"model"`
	Choices []struct {
		Index   int `json:"index"`
		Message struct {
			Role      string          `json:"role"`
			Content   json.RawMessage `json:"content"`
			ToolCalls []toolCallWire  `json:"tool_calls"`
		} `json:"message"`
		FinishReason string          `json:"finish_reason"`
		Logprobs     json.RawMessage `json:"logprobs"`
	} `json:"choices"`
	Usage usageWire `json:"usage"`
}

type responsesResponseWire struct {
	ID     string `json:"id"`
	Model  string `json:"model"`
	Status string `json:"status"`
	Output []struct {
		Type      string `json:"type"`
		ID        string `json:"id"`
		Role      string `json:"role"`
		Status    string `json:"status"`
		CallID    string `json:"call_id"`
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
		Content   []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Summary []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"summary"`
	} `json:"output"`
	Usage usageWire `json:"usage"`
}

type embeddingsResponseWire struct {
	Model string `json:"model"`
	Data  []struct {
		Index     int             `json:"index"`
		Embedding json.RawMessage `json:"embedding"`
	} `json:"data"`
	Usage usageWire `json:"usage"`
}

type imagesResponseWire struct {
	Created int64 `json:"created"`
	Data    []struct {
		URL           string `json:"url"`
		Base64        string `json:"b64_json"`
		RevisedPrompt string `json:"revised_prompt"`
	} `json:"data"`
}

func decodeResponse(operation contract.Operation, payload []byte) (contract.Response, error) {
	switch operation {
	case contract.OperationChat:
		var wire chatResponseWire
		if err := json.Unmarshal(payload, &wire); err != nil || wire.ID == "" || len(wire.Choices) == 0 {
			return contract.Response{}, providerFailure("upstream returned an invalid chat response", err)
		}
		result := contract.Response{Model: wire.Model, Chat: &contract.ChatResponse{ID: wire.ID, Created: wire.Created}, Usage: canonicalUsage(wire.Usage)}
		for _, choice := range wire.Choices {
			message := contract.Message{Role: choice.Message.Role}
			content := bytes.TrimSpace(choice.Message.Content)
			if len(content) > 0 && !bytes.Equal(content, []byte("null")) {
				var text string
				if err := json.Unmarshal(content, &text); err != nil {
					return contract.Response{}, providerFailure("upstream returned unsupported chat content", err)
				}
				message.Content = []contract.ContentPart{{Type: "text", Text: text}}
			}
			for _, call := range choice.Message.ToolCalls {
				message.ToolCalls = append(message.ToolCalls, contract.ToolCall{ID: call.ID, Type: call.Type, Function: contract.FunctionCall{Name: call.Function.Name, Arguments: call.Function.Arguments}})
			}
			result.Chat.Choices = append(result.Chat.Choices, contract.ChatChoice{Index: choice.Index, Message: message, FinishReason: choice.FinishReason, Logprobs: append([]byte(nil), choice.Logprobs...)})
		}
		return result, nil
	case contract.OperationResponses:
		var wire responsesResponseWire
		if err := json.Unmarshal(payload, &wire); err != nil || wire.ID == "" {
			return contract.Response{}, providerFailure("upstream returned an invalid responses response", err)
		}
		result := contract.Response{Model: wire.Model, Responses: &contract.ResponsesResponse{ID: wire.ID, Status: wire.Status}, Usage: canonicalUsage(wire.Usage)}
		for _, item := range wire.Output {
			output := contract.ResponseOutputItem{Type: item.Type, ID: item.ID, Role: item.Role, Status: item.Status, CallID: item.CallID, Name: item.Name, Arguments: item.Arguments}
			for _, part := range item.Content {
				output.Content = append(output.Content, contract.ContentPart{Type: part.Type, Text: part.Text})
			}
			for _, part := range item.Summary {
				output.Summary = append(output.Summary, contract.ContentPart{Type: part.Type, Text: part.Text})
			}
			result.Responses.Output = append(result.Responses.Output, output)
		}
		return result, nil
	case contract.OperationEmbeddings:
		var wire embeddingsResponseWire
		if err := json.Unmarshal(payload, &wire); err != nil || len(wire.Data) == 0 {
			return contract.Response{}, providerFailure("upstream returned an invalid embeddings response", err)
		}
		result := contract.Response{Model: wire.Model, Embeddings: &contract.EmbeddingsResponse{}, Usage: canonicalUsage(wire.Usage)}
		for _, item := range wire.Data {
			embedding := contract.Embedding{Index: item.Index}
			if len(item.Embedding) > 0 && item.Embedding[0] == '"' {
				if err := json.Unmarshal(item.Embedding, &embedding.Base64); err != nil {
					return contract.Response{}, providerFailure("upstream returned an invalid base64 embedding", err)
				}
			} else if err := json.Unmarshal(item.Embedding, &embedding.Vector); err != nil {
				return contract.Response{}, providerFailure("upstream returned an invalid embedding vector", err)
			}
			result.Embeddings.Data = append(result.Embeddings.Data, embedding)
		}
		return result, nil
	case contract.OperationImageGeneration:
		var wire imagesResponseWire
		if err := json.Unmarshal(payload, &wire); err != nil || len(wire.Data) == 0 {
			return contract.Response{}, providerFailure("upstream returned an invalid image response", err)
		}
		result := contract.Response{ImageGeneration: &contract.ImageGenerationResponse{Created: wire.Created}}
		for _, item := range wire.Data {
			if item.URL == "" && item.Base64 == "" {
				return contract.Response{}, providerFailure("upstream image has no content", nil)
			}
			result.ImageGeneration.Data = append(result.ImageGeneration.Data, contract.GeneratedImage{URL: item.URL, Base64: item.Base64, RevisedPrompt: item.RevisedPrompt})
		}
		return result, nil
	default:
		return contract.Response{}, invalidRequest("unsupported operation", nil)
	}
}

func canonicalUsage(wire usageWire) contract.Usage {
	input := wire.InputTokens
	if input == 0 {
		input = wire.PromptTokens
	}
	output := wire.OutputTokens
	if output == 0 {
		output = wire.CompletionTokens
	}
	total := wire.TotalTokens
	if total == 0 {
		total = input + output
	}
	cacheRead := wire.InputDetails.CachedTokens
	if cacheRead == 0 {
		cacheRead = wire.PromptDetails.CachedTokens
	}
	return contract.Usage{InputTokens: input, OutputTokens: output, TotalTokens: total, CacheReadTokens: cacheRead, ReasoningTokens: wire.OutputDetails.ReasoningTokens}
}
