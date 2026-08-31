package llmgateway

import (
	"fmt"

	"github.com/daptin/llmgateway/catalog"
	"github.com/daptin/llmgateway/contract"
)

func validateModelCapabilities(model catalog.Model, request contract.Request) error {
	require := func(capability string) error {
		if model.Capabilities[capability] {
			return nil
		}
		return fmt.Errorf("model %q does not enable %s", model.Name, capability)
	}
	switch request.Operation {
	case contract.OperationChat:
		if len(request.Chat.Tools) != 0 || request.Chat.ToolChoice != nil || messagesUseTools(request.Chat.Messages) {
			if err := require("tools"); err != nil {
				return err
			}
		}
		if messagesUsePart(request.Chat.Messages, "image_url") {
			if err := require("vision"); err != nil {
				return err
			}
		}
		if messagesUsePart(request.Chat.Messages, "input_audio") {
			if err := require("audio"); err != nil {
				return err
			}
		}
		if request.Chat.ResponseFormat != nil && request.Chat.ResponseFormat.Type == "json_schema" {
			if err := require("json_schema"); err != nil {
				return err
			}
		}
		if request.Chat.Logprobs {
			return require("logprobs")
		}
	case contract.OperationResponses:
		if len(request.Responses.Tools) != 0 || request.Responses.ToolChoice != nil || responsesUseTools(request.Responses.Input) {
			if err := require("tools"); err != nil {
				return err
			}
		}
		if responsesUsePart(request.Responses.Input, "input_image") {
			if err := require("vision"); err != nil {
				return err
			}
		}
		if request.Responses.TextFormat != nil && request.Responses.TextFormat.Type == "json_schema" {
			return require("json_schema")
		}
	case contract.OperationEmbeddings:
		if len(request.Embeddings.Input.Tokens) != 0 {
			if err := require("token_ids"); err != nil {
				return err
			}
		}
		if request.Embeddings.Dimensions != 0 {
			return require("dimensions")
		}
	}
	return nil
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
