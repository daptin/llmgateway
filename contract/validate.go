package contract

import (
	"encoding/json"
	"errors"
	"fmt"
)

func (r Request) Validate() error {
	if r.ID == "" || r.PublicModel == "" || !r.Operation.Valid() {
		return errors.New("request id, model, and operation are required")
	}
	if r.MaxOutputTokens < 0 || !r.EstimatedUsage.Valid() {
		return errors.New("request token bounds cannot be negative")
	}
	count := 0
	if r.Chat != nil {
		count++
	}
	if r.TextCompletion != nil {
		count++
	}
	if r.Responses != nil {
		count++
	}
	if r.Embeddings != nil {
		count++
	}
	if r.ImageGeneration != nil {
		count++
	}
	if r.ImageEdit != nil {
		count++
	}
	if r.ImageVariation != nil {
		count++
	}
	if r.Moderation != nil {
		count++
	}
	if r.Rerank != nil {
		count++
	}
	if r.AudioSpeech != nil {
		count++
	}
	if r.Transcription != nil {
		count++
	}
	if r.Search != nil {
		count++
	}
	if r.OCR != nil {
		count++
	}
	if count != 1 {
		return errors.New("request must contain exactly one operation payload")
	}
	if r.Stream && r.Operation != OperationChat && r.Operation != OperationTextCompletion && r.Operation != OperationResponses {
		return errors.New("operation does not support streaming")
	}
	switch r.Operation {
	case OperationChat:
		if r.Chat == nil || len(r.Chat.Messages) == 0 {
			return errors.New("chat request requires messages")
		}
		if err := validateMessages(r.Chat.Messages); err != nil {
			return err
		}
		if err := validateTools(r.Chat.Tools, r.Chat.ToolChoice, r.Chat.ResponseFormat); err != nil {
			return err
		}
		if r.Chat.N < 1 || r.Chat.MaxCompletionTokens < 1 {
			return errors.New("chat n and maximum completion tokens must be positive")
		}
		if !validOptionalRange(r.Chat.Temperature, 0, 2) || !validOptionalRange(r.Chat.TopP, 0, 1) ||
			!validOptionalRange(r.Chat.FrequencyPenalty, -2, 2) || !validOptionalRange(r.Chat.PresencePenalty, -2, 2) {
			return errors.New("chat sampling parameters are out of range")
		}
		if r.Chat.ReasoningEffort != "" && r.Chat.ReasoningEffort != "none" && r.Chat.ReasoningEffort != "minimal" &&
			r.Chat.ReasoningEffort != "low" && r.Chat.ReasoningEffort != "medium" && r.Chat.ReasoningEffort != "high" && r.Chat.ReasoningEffort != "xhigh" {
			return errors.New("invalid chat reasoning effort")
		}
	case OperationTextCompletion:
		completion := r.TextCompletion
		if completion == nil || (len(completion.Prompt.Texts) == 0 && len(completion.Prompt.Tokens) == 0) {
			return errors.New("text completion request requires a prompt")
		}
		if len(completion.Prompt.Texts) > 0 && len(completion.Prompt.Tokens) > 0 {
			return errors.New("text completion prompt must use text or tokens, not both")
		}
		for _, prompt := range completion.Prompt.Texts {
			if prompt == "" {
				return errors.New("text completion prompt cannot be empty")
			}
		}
		for _, prompt := range completion.Prompt.Tokens {
			if len(prompt) == 0 {
				return errors.New("text completion token prompt cannot be empty")
			}
			for _, token := range prompt {
				if token < 0 {
					return errors.New("text completion token IDs cannot be negative")
				}
			}
		}
		if completion.N < 1 || completion.N > 128 || completion.MaxTokens < 1 || completion.BestOf < completion.N || completion.BestOf > 128 {
			return errors.New("invalid text completion choice or token bounds")
		}
		if r.Stream && completion.BestOf > completion.N {
			return errors.New("streaming text completion does not support best_of greater than n")
		}
		if !validOptionalRange(completion.Temperature, 0, 2) || !validOptionalRange(completion.TopP, 0, 1) ||
			!validOptionalRange(completion.FrequencyPenalty, -2, 2) || !validOptionalRange(completion.PresencePenalty, -2, 2) {
			return errors.New("text completion sampling parameters are out of range")
		}
		if completion.Logprobs != nil && (*completion.Logprobs < 0 || *completion.Logprobs > 5) {
			return errors.New("text completion logprobs must be between 0 and 5")
		}
		for token, bias := range completion.LogitBias {
			if token == "" || bias < -100 || bias > 100 {
				return errors.New("invalid text completion logit bias")
			}
		}
	case OperationResponses, OperationResponseCompact:
		if r.Responses == nil || len(r.Responses.Input) == 0 {
			return errors.New("responses request requires input")
		}
		for _, item := range r.Responses.Input {
			if err := validateResponseInput(item); err != nil {
				return err
			}
		}
		if r.Operation == OperationResponseCompact {
			if len(r.Responses.Tools) != 0 || r.Responses.ToolChoice != nil || r.Responses.TextFormat != nil ||
				r.Responses.Temperature != nil || r.Responses.TopP != nil || r.Responses.ParallelToolCalls != nil ||
				r.Responses.ReasoningEffort != "" || r.Responses.ReasoningSummary != "" || r.Responses.SafetyIdentifier != "" ||
				r.Responses.Truncation != "" || r.Responses.User != "" || r.Responses.TopLogprobs != nil || r.MaxOutputTokens != 0 {
				return errors.New("response compaction contains unsupported response controls")
			}
			if r.Responses.PromptCacheMode != "" && r.Responses.PromptCacheMode != "implicit" && r.Responses.PromptCacheMode != "explicit" {
				return errors.New("invalid response compaction prompt cache mode")
			}
			if r.Responses.PromptCacheTTL != "" && r.Responses.PromptCacheTTL != "30m" {
				return errors.New("invalid response compaction prompt cache TTL")
			}
			if r.Responses.PromptCacheRetention != "" && r.Responses.PromptCacheRetention != "in_memory" && r.Responses.PromptCacheRetention != "24h" {
				return errors.New("invalid response compaction prompt cache retention")
			}
		} else {
			if err := validateTools(r.Responses.Tools, r.Responses.ToolChoice, r.Responses.TextFormat); err != nil {
				return err
			}
			if r.MaxOutputTokens < 1 {
				return errors.New("responses maximum output tokens must be positive")
			}
		}
		if !validOptionalRange(r.Responses.Temperature, 0, 2) || !validOptionalRange(r.Responses.TopP, 0, 1) {
			return errors.New("responses sampling parameters are out of range")
		}
		if r.Responses.ReasoningEffort != "" && r.Responses.ReasoningEffort != "none" && r.Responses.ReasoningEffort != "minimal" &&
			r.Responses.ReasoningEffort != "low" && r.Responses.ReasoningEffort != "medium" && r.Responses.ReasoningEffort != "high" && r.Responses.ReasoningEffort != "xhigh" {
			return errors.New("invalid responses reasoning effort")
		}
		if r.Responses.ReasoningSummary != "" && r.Responses.ReasoningSummary != "auto" && r.Responses.ReasoningSummary != "concise" && r.Responses.ReasoningSummary != "detailed" {
			return errors.New("invalid responses reasoning summary")
		}
		if len(r.Responses.SafetyIdentifier) > 64 {
			return errors.New("responses safety identifier exceeds 64 characters")
		}
		if r.Responses.ServiceTier != "" && r.Responses.ServiceTier != "auto" && r.Responses.ServiceTier != "default" && r.Responses.ServiceTier != "flex" &&
			r.Responses.ServiceTier != "scale" && r.Responses.ServiceTier != "priority" && r.Responses.ServiceTier != "fast" && r.Responses.ServiceTier != "ultrafast" {
			return errors.New("invalid responses service tier")
		}
		if r.Responses.Truncation != "" && r.Responses.Truncation != "auto" && r.Responses.Truncation != "disabled" {
			return errors.New("invalid responses truncation")
		}
		if r.Responses.TopLogprobs != nil && (*r.Responses.TopLogprobs < 0 || *r.Responses.TopLogprobs > 20) {
			return errors.New("responses top logprobs must be between 0 and 20")
		}
	case OperationEmbeddings:
		if r.Embeddings == nil || (len(r.Embeddings.Input.Texts) == 0 && len(r.Embeddings.Input.Tokens) == 0) {
			return errors.New("embeddings request requires input")
		}
		if len(r.Embeddings.Input.Texts) > 0 && len(r.Embeddings.Input.Tokens) > 0 {
			return errors.New("embeddings input must use text or tokens, not both")
		}
		for _, text := range r.Embeddings.Input.Texts {
			if text == "" {
				return errors.New("embedding text cannot be empty")
			}
		}
		for _, tokens := range r.Embeddings.Input.Tokens {
			if len(tokens) == 0 {
				return errors.New("embedding token input cannot be empty")
			}
			for _, token := range tokens {
				if token < 0 {
					return errors.New("embedding token IDs cannot be negative")
				}
			}
		}
		if r.Embeddings.Dimensions < 0 || (r.Embeddings.EncodingFormat != "float" && r.Embeddings.EncodingFormat != "base64") {
			return errors.New("invalid embeddings dimensions or encoding format")
		}
	case OperationImageGeneration:
		if r.ImageGeneration == nil || r.ImageGeneration.Prompt == "" {
			return errors.New("image generation request requires a prompt")
		}
		if r.ImageGeneration.N < 1 || r.ImageGeneration.N > 10 || (r.ImageGeneration.ResponseFormat != "url" && r.ImageGeneration.ResponseFormat != "b64_json") {
			return errors.New("invalid image count or response format")
		}
	case OperationImageEdit:
		if r.ImageEdit == nil || len(r.ImageEdit.Images) == 0 || r.ImageEdit.Prompt == "" {
			return errors.New("image edit requires images and a prompt")
		}
		for _, image := range r.ImageEdit.Images {
			if image.Name == "" || len(image.Data) == 0 {
				return errors.New("image edit contains an invalid image")
			}
		}
		if r.ImageEdit.Mask != nil && (r.ImageEdit.Mask.Name == "" || len(r.ImageEdit.Mask.Data) == 0) {
			return errors.New("image edit mask is invalid")
		}
		if r.ImageEdit.N < 1 || r.ImageEdit.N > 10 || (r.ImageEdit.ResponseFormat != "url" && r.ImageEdit.ResponseFormat != "b64_json") {
			return errors.New("invalid image edit count or response format")
		}
		if r.ImageEdit.OutputCompression != nil && (*r.ImageEdit.OutputCompression < 0 || *r.ImageEdit.OutputCompression > 100) {
			return errors.New("image edit output compression must be between 0 and 100")
		}
	case OperationImageVariation:
		if r.ImageVariation == nil || r.ImageVariation.Image.Name == "" || len(r.ImageVariation.Image.Data) == 0 {
			return errors.New("image variation requires an image")
		}
		if r.ImageVariation.N < 1 || r.ImageVariation.N > 10 ||
			(r.ImageVariation.ResponseFormat != "url" && r.ImageVariation.ResponseFormat != "b64_json") {
			return errors.New("invalid image variation count or response format")
		}
	case OperationModeration:
		if r.Moderation == nil || len(r.Moderation.Input) == 0 {
			return errors.New("moderation request requires input")
		}
		for _, input := range r.Moderation.Input {
			switch input.Type {
			case "text":
				if input.Text == "" {
					return errors.New("moderation text cannot be empty")
				}
			case "image_url":
				if input.ImageURL == nil || input.ImageURL.URL == "" {
					return errors.New("moderation image requires a URL")
				}
			default:
				return errors.New("unsupported moderation input type")
			}
		}
	case OperationRerank:
		if r.Rerank == nil || r.Rerank.Query == "" || len(r.Rerank.Documents) == 0 {
			return errors.New("rerank request requires query and documents")
		}
		if r.Rerank.TopN < 1 || r.Rerank.TopN > len(r.Rerank.Documents) || r.Rerank.MaxChunksPerDoc < 0 || r.Rerank.MaxTokensPerDoc < 0 {
			return errors.New("invalid rerank bounds")
		}
		for _, document := range r.Rerank.Documents {
			if (document.Text == "") == (len(document.Object) == 0) {
				return errors.New("rerank document must contain exactly one text or object value")
			}
			if len(document.Object) != 0 && !ValidJSONObject(document.Object) {
				return errors.New("rerank document object is invalid")
			}
		}
		for _, field := range r.Rerank.RankFields {
			if field == "" {
				return errors.New("rerank rank fields cannot be empty")
			}
		}
	case OperationAudioSpeech:
		if r.AudioSpeech == nil || r.AudioSpeech.Input == "" || r.AudioSpeech.Voice == "" || !validSpeechFormat(r.AudioSpeech.ResponseFormat) {
			return errors.New("audio speech requires input, voice, and a valid response format")
		}
		if !validOptionalRange(r.AudioSpeech.Speed, 0.25, 4) {
			return errors.New("audio speech speed must be between 0.25 and 4")
		}
	case OperationTranscription, OperationTranslation:
		if r.Transcription == nil || r.Transcription.File.Name == "" || len(r.Transcription.File.Data) == 0 ||
			!validTranscriptionFormat(r.Transcription.ResponseFormat) || !validOptionalRange(r.Transcription.Temperature, 0, 1) {
			return errors.New("audio transcription requires a file and valid controls")
		}
		if r.Operation == OperationTranslation && r.Transcription.Language != "" {
			return errors.New("audio translation does not accept a source language")
		}
		for _, granularity := range r.Transcription.TimestampGranularities {
			if granularity != "word" && granularity != "segment" {
				return errors.New("invalid timestamp granularity")
			}
		}
		if len(r.Transcription.TimestampGranularities) != 0 && r.Transcription.ResponseFormat != "verbose_json" {
			return errors.New("timestamp granularities require verbose_json")
		}
	case OperationSearch:
		if r.Search == nil || len(r.Search.Queries) == 0 || r.Search.MaxResults < 1 || r.Search.MaxResults > 20 || len(r.Search.DomainFilter) > 20 || r.Search.MaxTokensPerPage < 1 {
			return errors.New("search requires queries and valid result bounds")
		}
		for _, query := range r.Search.Queries {
			if query == "" {
				return errors.New("search queries cannot be empty")
			}
		}
		for _, domain := range r.Search.DomainFilter {
			if domain == "" {
				return errors.New("search domains cannot be empty")
			}
		}
	case OperationOCR:
		if r.OCR == nil || !validOCRDocument(r.OCR.Document) {
			return errors.New("OCR requires a document or image URL")
		}
		seenPages := make(map[int]struct{}, len(r.OCR.Pages))
		for _, page := range r.OCR.Pages {
			if page < 0 {
				return errors.New("OCR pages cannot be negative")
			}
			if _, exists := seenPages[page]; exists {
				return errors.New("OCR pages cannot contain duplicates")
			}
			seenPages[page] = struct{}{}
		}
		if (r.OCR.ImageLimit != nil && *r.OCR.ImageLimit < 0) || (r.OCR.ImageMinSize != nil && *r.OCR.ImageMinSize < 0) {
			return errors.New("OCR image bounds cannot be negative")
		}
		if len(r.OCR.BBoxAnnotationFormat) != 0 && !ValidJSONObject(r.OCR.BBoxAnnotationFormat) {
			return errors.New("OCR bbox annotation format must be an object")
		}
		if len(r.OCR.DocumentAnnotationFormat) != 0 && !ValidJSONObject(r.OCR.DocumentAnnotationFormat) {
			return errors.New("OCR document annotation format must be an object")
		}
		if r.OCR.TableFormat != "" && r.OCR.TableFormat != "markdown" && r.OCR.TableFormat != "html" {
			return errors.New("OCR table format must be markdown or html")
		}
		if r.OCR.ConfidenceScoresGranularity != "" && r.OCR.ConfidenceScoresGranularity != "word" && r.OCR.ConfidenceScoresGranularity != "page" {
			return errors.New("OCR confidence score granularity must be word or page")
		}
	}
	return nil
}

func validSpeechFormat(value string) bool {
	switch value {
	case "mp3", "opus", "aac", "flac", "wav", "pcm":
		return true
	default:
		return false
	}
}

func validTranscriptionFormat(value string) bool {
	switch value {
	case "json", "text", "srt", "verbose_json", "vtt", "diarized_json":
		return true
	default:
		return false
	}
}

func validOptionalRange(value *float64, minimum, maximum float64) bool {
	return value == nil || (*value >= minimum && *value <= maximum)
}

func validateMessages(messages []Message) error {
	for _, message := range messages {
		switch message.Role {
		case "system", "developer", "user", "assistant", "tool":
		default:
			return fmt.Errorf("invalid message role %q", message.Role)
		}
		if len(message.Content) == 0 && len(message.ToolCalls) == 0 {
			return errors.New("message requires content or tool calls")
		}
		for _, part := range message.Content {
			switch part.Type {
			case "text":
				if part.Text == "" {
					return errors.New("text content cannot be empty")
				}
			case "image_url":
				if part.ImageURL == nil || part.ImageURL.URL == "" {
					return errors.New("image content requires a URL")
				}
			case "input_audio":
				if part.Audio == nil || part.Audio.Data == "" || part.Audio.Format == "" {
					return errors.New("audio content requires data and format")
				}
			default:
				return fmt.Errorf("invalid message content type %q", part.Type)
			}
		}
		if message.Role == "tool" && message.ToolCallID == "" {
			return errors.New("tool message requires a tool call ID")
		}
		for _, call := range message.ToolCalls {
			if call.ID == "" || call.Type != "function" || call.Function.Name == "" {
				return errors.New("invalid message tool call")
			}
		}
	}
	return nil
}

func validateTools(tools []Tool, choice *ToolChoice, format *ResponseFormat) error {
	for _, tool := range tools {
		if tool.Type != "function" || tool.Function.Name == "" || !ValidJSONObject(tool.Function.Parameters) {
			return errors.New("invalid function tool")
		}
	}
	if choice != nil {
		switch choice.Mode {
		case "none", "auto", "required":
		case "function":
			if choice.FunctionName == "" {
				return errors.New("named tool choice requires a function")
			}
		default:
			return errors.New("invalid tool choice")
		}
	}
	if format != nil {
		switch format.Type {
		case "text", "json_object":
		case "json_schema":
			if format.JSONSchema == nil || format.JSONSchema.Name == "" || !ValidJSONObject(format.JSONSchema.Schema) {
				return errors.New("invalid JSON schema response format")
			}
		default:
			return errors.New("invalid response format")
		}
	}
	return nil
}

func validateResponseInput(item ResponseInputItem) error {
	switch item.Type {
	case "message":
		if item.Role == "" || len(item.Content) == 0 {
			return errors.New("response message requires role and content")
		}
		for _, part := range item.Content {
			switch part.Type {
			case "input_text":
				if part.Text == "" {
					return errors.New("response input text cannot be empty")
				}
			case "input_image":
				if part.ImageURL == nil || part.ImageURL.URL == "" {
					return errors.New("response input image requires a URL")
				}
			case "input_file":
				if part.File == nil || part.File.Data == "" {
					return errors.New("response input file requires inline data")
				}
			default:
				return errors.New("unsupported response input content")
			}
		}
	case "function_call":
		if item.CallID == "" || item.Name == "" || item.Arguments == "" {
			return errors.New("response function call is incomplete")
		}
	case "function_call_output":
		if item.CallID == "" || item.Output == "" {
			return errors.New("response function output is incomplete")
		}
	case "compaction":
		if item.EncryptedContent == "" {
			return errors.New("response compaction item requires encrypted content")
		}
	default:
		return errors.New("unsupported response input item")
	}
	return nil
}

func ValidJSONObject(raw json.RawMessage) bool {
	var value map[string]any
	return len(raw) > 0 && json.Unmarshal(raw, &value) == nil && value != nil
}
