package llmgateway

import (
	"fmt"
	"sort"

	"github.com/daptin/llmgateway/catalog"
	"github.com/daptin/llmgateway/contract"
)

func validateModelCapabilities(model catalog.Model, request contract.Request) error {
	for _, capability := range requiredAdapterFeatures(request) {
		if capability == "streaming" {
			continue
		}
		if !model.Capabilities[capability] {
			return fmt.Errorf("model %q does not enable %s", model.Name, capability)
		}
	}
	return nil
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
