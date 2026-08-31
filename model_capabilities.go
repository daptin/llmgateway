package llmgateway

import (
	"fmt"
	"sort"

	"github.com/daptin/llmgateway/catalog"
	"github.com/daptin/llmgateway/contract"
)

func applyModelParameterPolicy(model catalog.Model, request contract.Request) (contract.Request, error) {
	missing := make([]string, 0)
	for _, capability := range requiredAdapterFeatures(request) {
		if capability != "streaming" && !model.Capabilities[capability] {
			missing = append(missing, capability)
		}
	}
	if len(missing) == 0 || model.UnsupportedParameterPolicy == "passthrough" {
		return request, nil
	}
	if model.UnsupportedParameterPolicy == "reject" {
		return contract.Request{}, fmt.Errorf("model %q does not enable %s", model.Name, missing[0])
	}
	if model.UnsupportedParameterPolicy != "drop" {
		return contract.Request{}, fmt.Errorf("model %q has invalid unsupported parameter policy", model.Name)
	}
	for _, capability := range missing {
		var err error
		request, err = dropOptionalCapability(request, capability)
		if err != nil {
			return contract.Request{}, fmt.Errorf("model %q cannot drop %s: %w", model.Name, capability, err)
		}
	}
	return request, nil
}

func dropOptionalCapability(request contract.Request, capability string) (contract.Request, error) {
	switch capability {
	case "logprobs":
		chat := *request.Chat
		chat.Logprobs = false
		chat.TopLogprobs = 0
		request.Chat = &chat
	case "json_schema":
		switch request.Operation {
		case contract.OperationChat:
			chat := *request.Chat
			chat.ResponseFormat = nil
			request.Chat = &chat
		case contract.OperationResponses:
			responses := *request.Responses
			responses.TextFormat = nil
			request.Responses = &responses
		}
	case "dimensions":
		embeddings := *request.Embeddings
		embeddings.Dimensions = 0
		request.Embeddings = &embeddings
	case "tools":
		switch request.Operation {
		case contract.OperationChat:
			if messagesUseTools(request.Chat.Messages) {
				return contract.Request{}, fmt.Errorf("tool-call history is semantic input")
			}
			chat := *request.Chat
			chat.Tools = nil
			chat.ToolChoice = nil
			request.Chat = &chat
		case contract.OperationResponses:
			if responsesUseTools(request.Responses.Input) {
				return contract.Request{}, fmt.Errorf("function-call input is semantic input")
			}
			responses := *request.Responses
			responses.Tools = nil
			responses.ToolChoice = nil
			request.Responses = &responses
		}
	case "vision", "audio", "token_ids":
		return contract.Request{}, fmt.Errorf("semantic input cannot be removed")
	default:
		return contract.Request{}, fmt.Errorf("unsupported capability cannot be removed")
	}
	return request, nil
}

func requiredAdapterFeatures(request contract.Request) []string {
	features := make(map[string]struct{})
	add := func(feature string) { features[feature] = struct{}{} }
	if request.Stream {
		add("streaming")
	}
	switch request.Operation {
	case contract.OperationChat:
		if len(request.Chat.Tools) != 0 || request.Chat.ToolChoice != nil || messagesUseTools(request.Chat.Messages) {
			add("tools")
		}
		if messagesUsePart(request.Chat.Messages, "image_url") {
			add("vision")
		}
		if messagesUsePart(request.Chat.Messages, "input_audio") {
			add("audio")
		}
		if request.Chat.ResponseFormat != nil && request.Chat.ResponseFormat.Type == "json_schema" {
			add("json_schema")
		}
		if request.Chat.Logprobs {
			add("logprobs")
		}
		if request.Chat.FrequencyPenalty != nil || request.Chat.PresencePenalty != nil {
			add("penalties")
		}
		if request.Chat.ParallelToolCalls != nil {
			add("parallel_tools")
		}
		if request.Chat.ReasoningEffort != "" {
			add("reasoning")
		}
	case contract.OperationResponses:
		if len(request.Responses.Tools) != 0 || request.Responses.ToolChoice != nil || responsesUseTools(request.Responses.Input) {
			add("tools")
		}
		if responsesUsePart(request.Responses.Input, "input_image") {
			add("vision")
		}
		if request.Responses.TextFormat != nil && request.Responses.TextFormat.Type == "json_schema" {
			add("json_schema")
		}
		if request.Responses.ParallelToolCalls != nil {
			add("parallel_tools")
		}
		if request.Responses.ReasoningEffort != "" {
			add("reasoning")
		}
	case contract.OperationEmbeddings:
		if len(request.Embeddings.Input.Tokens) != 0 {
			add("token_ids")
		}
		if request.Embeddings.Dimensions != 0 {
			add("dimensions")
		}
	}
	result := make([]string, 0, len(features))
	for feature := range features {
		result = append(result, feature)
	}
	sort.Strings(result)
	return result
}

func messagesUseTools(messages []contract.Message) bool {
	for _, message := range messages {
		if message.ToolCallID != "" || len(message.ToolCalls) != 0 {
			return true
		}
	}
	return false
}

func messagesUsePart(messages []contract.Message, kind string) bool {
	for _, message := range messages {
		for _, part := range message.Content {
			if part.Type == kind {
				return true
			}
		}
	}
	return false
}

func responsesUseTools(items []contract.ResponseInputItem) bool {
	for _, item := range items {
		if item.Type == "function_call" || item.Type == "function_call_output" {
			return true
		}
	}
	return false
}

func responsesUsePart(items []contract.ResponseInputItem, kind string) bool {
	for _, item := range items {
		for _, part := range item.Content {
			if part.Type == kind {
				return true
			}
		}
	}
	return false
}
