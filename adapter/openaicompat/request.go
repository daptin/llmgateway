package openaicompat

import (
	"bytes"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"sort"

	"github.com/daptin/llmgateway/catalog"
	"github.com/daptin/llmgateway/contract"
	"github.com/daptin/llmgateway/internal/jsonx"
)

type deploymentParameters struct {
	Chat            map[string]json.RawMessage `json:"chat,omitempty"`
	TextCompletion  map[string]json.RawMessage `json:"text_completion,omitempty"`
	Responses       map[string]json.RawMessage `json:"responses,omitempty"`
	ResponseCompact map[string]json.RawMessage `json:"response_compaction,omitempty"`
	Embeddings      map[string]json.RawMessage `json:"embeddings,omitempty"`
	ImageGeneration map[string]json.RawMessage `json:"image_generation,omitempty"`
	ImageEdit       map[string]json.RawMessage `json:"image_edit,omitempty"`
	ImageVariation  map[string]json.RawMessage `json:"image_variation,omitempty"`
	Moderation      map[string]json.RawMessage `json:"moderation,omitempty"`
	Rerank          map[string]json.RawMessage `json:"rerank,omitempty"`
	AudioSpeech     map[string]json.RawMessage `json:"audio_speech,omitempty"`
	Transcription   map[string]json.RawMessage `json:"audio_transcription,omitempty"`
	Translation     map[string]json.RawMessage `json:"audio_translation,omitempty"`
	Search          map[string]json.RawMessage `json:"search,omitempty"`
	OCR             map[string]json.RawMessage `json:"ocr,omitempty"`
}

type encodedRequest struct {
	body        []byte
	path        string
	contentType string
	accept      string
}

func encodeRequest(deployment catalog.Deployment, request contract.Request) (encodedRequest, error) {
	var value map[string]any
	var path string
	switch request.Operation {
	case contract.OperationChat:
		value = encodeChatRequest(deployment.UpstreamModel, request)
		path = "/chat/completions"
	case contract.OperationTextCompletion:
		value = encodeTextCompletionRequest(deployment.UpstreamModel, request)
		path = "/completions"
	case contract.OperationResponses:
		value = encodeResponsesRequest(deployment.UpstreamModel, request)
		path = "/responses"
	case contract.OperationResponseCompact:
		value = encodeResponseCompactionRequest(deployment.UpstreamModel, request)
		path = "/responses/compact"
	case contract.OperationEmbeddings:
		value = encodeEmbeddingsRequest(deployment.UpstreamModel, request)
		path = "/embeddings"
	case contract.OperationImageGeneration:
		value = encodeImageRequest(deployment.UpstreamModel, request)
		path = "/images/generations"
	case contract.OperationImageEdit:
		return encodeImageEditRequest(deployment, request)
	case contract.OperationImageVariation:
		return encodeImageVariationRequest(deployment, request)
	case contract.OperationModeration:
		value = encodeModerationRequest(deployment.UpstreamModel, request)
		path = "/moderations"
	case contract.OperationRerank:
		value = encodeRerankRequest(deployment.UpstreamModel, request)
		path = "/rerank"
	case contract.OperationAudioSpeech:
		value = encodeAudioSpeechRequest(deployment.UpstreamModel, request)
		path = "/audio/speech"
	case contract.OperationTranscription, contract.OperationTranslation:
		return encodeAudioMultipartRequest(deployment, request)
	case contract.OperationSearch:
		value = encodeSearchRequest(request)
		path = "/search"
	case contract.OperationOCR:
		value = encodeOCRRequest(deployment.UpstreamModel, request)
		path = "/ocr"
	default:
		return encodedRequest{}, invalidRequest("unsupported operation", nil)
	}
	parameters, err := parseDeploymentParameters(deployment.Parameters)
	if err != nil {
		return encodedRequest{}, invalidRequest("invalid deployment parameters", err)
	}
	for key, raw := range parameters.forOperation(request.Operation) {
		if _, exists := value[key]; exists {
			return encodedRequest{}, invalidRequest(fmt.Sprintf("deployment parameter %q conflicts with a canonical request field", key), nil)
		}
		value[key] = raw
	}
	payload, err := json.Marshal(value)
	if err != nil {
		return encodedRequest{}, invalidRequest("failed to encode upstream request", err)
	}
	accept := "application/json"
	if request.Operation == contract.OperationAudioSpeech {
		accept = speechContentType(request.AudioSpeech.ResponseFormat)
	}
	return encodedRequest{body: payload, path: path, contentType: "application/json", accept: accept}, nil
}

func parseDeploymentParameters(raw json.RawMessage) (deploymentParameters, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("{}")) {
		return deploymentParameters{}, nil
	}
	var parameters deploymentParameters
	if err := jsonx.DecodeOne(bytes.NewReader(trimmed), &parameters); err != nil {
		return deploymentParameters{}, fmt.Errorf("invalid OpenAI-compatible deployment parameters: %w", err)
	}
	for operation, values := range map[contract.Operation]map[string]json.RawMessage{
		contract.OperationChat: parameters.Chat, contract.OperationResponses: parameters.Responses,
		contract.OperationResponseCompact: parameters.ResponseCompact,
		contract.OperationTextCompletion:  parameters.TextCompletion,
		contract.OperationEmbeddings:      parameters.Embeddings, contract.OperationImageGeneration: parameters.ImageGeneration,
		contract.OperationImageEdit:      parameters.ImageEdit,
		contract.OperationImageVariation: parameters.ImageVariation,
		contract.OperationModeration:     parameters.Moderation, contract.OperationRerank: parameters.Rerank,
		contract.OperationAudioSpeech: parameters.AudioSpeech, contract.OperationTranscription: parameters.Transcription,
		contract.OperationTranslation: parameters.Translation,
		contract.OperationSearch:      parameters.Search,
		contract.OperationOCR:         parameters.OCR,
	} {
		for key := range values {
			if key == "" {
				return deploymentParameters{}, fmt.Errorf("%s deployment parameters contain an empty field", operation)
			}
			if _, canonical := canonicalRequestFields[operation][key]; canonical {
				return deploymentParameters{}, fmt.Errorf("%s deployment parameter %q conflicts with a canonical request field", operation, key)
			}
		}
	}
	return parameters, nil
}

func (parameters deploymentParameters) forOperation(operation contract.Operation) map[string]json.RawMessage {
	switch operation {
	case contract.OperationChat:
		return parameters.Chat
	case contract.OperationTextCompletion:
		return parameters.TextCompletion
	case contract.OperationResponses:
		return parameters.Responses
	case contract.OperationResponseCompact:
		return parameters.ResponseCompact
	case contract.OperationEmbeddings:
		return parameters.Embeddings
	case contract.OperationImageGeneration:
		return parameters.ImageGeneration
	case contract.OperationImageEdit:
		return parameters.ImageEdit
	case contract.OperationImageVariation:
		return parameters.ImageVariation
	case contract.OperationModeration:
		return parameters.Moderation
	case contract.OperationRerank:
		return parameters.Rerank
	case contract.OperationAudioSpeech:
		return parameters.AudioSpeech
	case contract.OperationTranscription:
		return parameters.Transcription
	case contract.OperationTranslation:
		return parameters.Translation
	case contract.OperationSearch:
		return parameters.Search
	case contract.OperationOCR:
		return parameters.OCR
	default:
		return nil
	}
}

var canonicalRequestFields = map[contract.Operation]map[string]struct{}{
	contract.OperationChat:            fieldSet("model", "messages", "stream", "stream_options", "tools", "tool_choice", "response_format", "n", "temperature", "top_p", "frequency_penalty", "presence_penalty", "max_completion_tokens", "stop", "user", "seed", "logprobs", "top_logprobs", "parallel_tool_calls", "reasoning_effort", "store", "prompt_cache_key"),
	contract.OperationTextCompletion:  fieldSet("model", "prompt", "best_of", "echo", "frequency_penalty", "logit_bias", "logprobs", "max_tokens", "n", "presence_penalty", "seed", "stop", "stream", "stream_options", "suffix", "temperature", "top_p", "user"),
	contract.OperationResponses:       fieldSet("model", "input", "stream", "store", "background", "instructions", "tools", "tool_choice", "text", "max_output_tokens", "temperature", "top_p", "parallel_tool_calls", "reasoning", "prompt_cache_key", "safety_identifier", "service_tier", "truncation", "user", "top_logprobs", "previous_response_id"),
	contract.OperationResponseCompact: fieldSet("model", "input", "instructions", "previous_response_id", "prompt_cache_key", "prompt_cache_options", "prompt_cache_retention", "service_tier"),
	contract.OperationEmbeddings:      fieldSet("model", "input", "encoding_format", "dimensions", "user"),
	contract.OperationImageGeneration: fieldSet("model", "prompt", "n", "response_format", "size", "quality"),
	contract.OperationImageEdit:       fieldSet("model", "image", "image[]", "mask", "prompt", "n", "size", "quality", "response_format", "user", "background", "input_fidelity", "output_format", "output_compression", "moderation"),
	contract.OperationImageVariation:  fieldSet("model", "image", "n", "size", "response_format", "user"),
	contract.OperationModeration:      fieldSet("model", "input"),
	contract.OperationRerank:          fieldSet("model", "query", "documents", "top_n", "rank_fields", "return_documents", "max_chunks_per_doc", "max_tokens_per_doc", "instruction"),
	contract.OperationAudioSpeech:     fieldSet("model", "input", "voice", "instructions", "speed", "response_format"),
	contract.OperationTranscription:   fieldSet("model", "file", "language", "prompt", "temperature", "response_format", "timestamp_granularities[]"),
	contract.OperationTranslation:     fieldSet("model", "file", "prompt", "temperature", "response_format"),
	contract.OperationSearch:          fieldSet("query", "max_results", "search_domain_filter", "max_tokens_per_page", "country"),
	contract.OperationOCR:             fieldSet("model", "document", "pages", "include_image_base64", "image_limit", "image_min_size", "bbox_annotation_format", "document_annotation_format", "document_annotation_prompt", "extract_header", "extract_footer", "table_format", "confidence_scores_granularity", "include_blocks", "id"),
}

func encodeOCRRequest(model string, request contract.Request) map[string]any {
	ocr := request.OCR
	document := map[string]any{"type": ocr.Document.Type, ocr.Document.Type: ocr.Document.URL}
	value := map[string]any{"model": model, "document": document}
	if len(ocr.Pages) != 0 {
		value["pages"] = ocr.Pages
	}
	if ocr.IncludeImageBase64 != nil {
		value["include_image_base64"] = *ocr.IncludeImageBase64
	}
	if ocr.ImageLimit != nil {
		value["image_limit"] = *ocr.ImageLimit
	}
	if ocr.ImageMinSize != nil {
		value["image_min_size"] = *ocr.ImageMinSize
	}
	if len(ocr.BBoxAnnotationFormat) != 0 {
		value["bbox_annotation_format"] = json.RawMessage(ocr.BBoxAnnotationFormat)
	}
	if len(ocr.DocumentAnnotationFormat) != 0 {
		value["document_annotation_format"] = json.RawMessage(ocr.DocumentAnnotationFormat)
	}
	if ocr.DocumentAnnotationPrompt != "" {
		value["document_annotation_prompt"] = ocr.DocumentAnnotationPrompt
	}
	if ocr.ExtractHeader != nil {
		value["extract_header"] = *ocr.ExtractHeader
	}
	if ocr.ExtractFooter != nil {
		value["extract_footer"] = *ocr.ExtractFooter
	}
	if ocr.TableFormat != "" {
		value["table_format"] = ocr.TableFormat
	}
	if ocr.ConfidenceScoresGranularity != "" {
		value["confidence_scores_granularity"] = ocr.ConfidenceScoresGranularity
	}
	if ocr.IncludeBlocks != nil {
		value["include_blocks"] = *ocr.IncludeBlocks
	}
	if ocr.ID != "" {
		value["id"] = ocr.ID
	}
	return value
}

func encodeSearchRequest(request contract.Request) map[string]any {
	search := request.Search
	var query any = search.Queries
	if len(search.Queries) == 1 {
		query = search.Queries[0]
	}
	value := map[string]any{"query": query, "max_results": search.MaxResults, "max_tokens_per_page": search.MaxTokensPerPage}
	if len(search.DomainFilter) != 0 {
		value["search_domain_filter"] = search.DomainFilter
	}
	if search.Country != "" {
		value["country"] = search.Country
	}
	return value
}

func encodeAudioSpeechRequest(model string, request contract.Request) map[string]any {
	speech := request.AudioSpeech
	value := map[string]any{"model": model, "input": speech.Input, "voice": speech.Voice, "response_format": speech.ResponseFormat}
	if speech.Instructions != "" {
		value["instructions"] = speech.Instructions
	}
	if speech.Speed != nil {
		value["speed"] = *speech.Speed
	}
	return value
}

func encodeAudioMultipartRequest(deployment catalog.Deployment, request contract.Request) (encodedRequest, error) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	err := writeMultipartFile(writer, "file", request.Transcription.File)
	if err == nil {
		err = writer.WriteField("model", deployment.UpstreamModel)
	}
	fields := map[string]string{"prompt": request.Transcription.Prompt, "response_format": request.Transcription.ResponseFormat}
	if request.Operation == contract.OperationTranscription {
		fields["language"] = request.Transcription.Language
	}
	if request.Transcription.Temperature != nil {
		fields["temperature"] = fmt.Sprintf("%g", *request.Transcription.Temperature)
	}
	for name, value := range fields {
		if err == nil && value != "" {
			err = writer.WriteField(name, value)
		}
	}
	if err == nil {
		for _, granularity := range request.Transcription.TimestampGranularities {
			if err = writer.WriteField("timestamp_granularities[]", granularity); err != nil {
				break
			}
		}
	}
	if err == nil {
		err = writeMultipartDeploymentFields(writer, deployment.Parameters, request.Operation)
	}
	closeErr := writer.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return encodedRequest{}, invalidRequest("failed to encode upstream audio request", err)
	}
	path := "/audio/transcriptions"
	if request.Operation == contract.OperationTranslation {
		path = "/audio/translations"
	}
	return encodedRequest{body: body.Bytes(), path: path, contentType: writer.FormDataContentType(), accept: "application/json, text/plain"}, nil
}

func encodeImageEditRequest(deployment catalog.Deployment, request contract.Request) (encodedRequest, error) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	edit := request.ImageEdit
	imageField := "image"
	if len(edit.Images) > 1 {
		imageField = "image[]"
	}
	var err error
	for _, image := range edit.Images {
		if err == nil {
			err = writeMultipartFile(writer, imageField, image)
		}
	}
	if err == nil && edit.Mask != nil {
		err = writeMultipartFile(writer, "mask", *edit.Mask)
	}
	fields := map[string]string{"model": deployment.UpstreamModel, "prompt": edit.Prompt, "size": edit.Size, "quality": edit.Quality,
		"response_format": edit.ResponseFormat, "user": edit.User, "background": edit.Background, "input_fidelity": edit.InputFidelity,
		"output_format": edit.OutputFormat, "moderation": edit.Moderation}
	if edit.N > 0 {
		fields["n"] = fmt.Sprint(edit.N)
	}
	if edit.OutputCompression != nil {
		fields["output_compression"] = fmt.Sprint(*edit.OutputCompression)
	}
	fieldNames := make([]string, 0, len(fields))
	for name := range fields {
		fieldNames = append(fieldNames, name)
	}
	sort.Strings(fieldNames)
	for _, name := range fieldNames {
		if err == nil && fields[name] != "" {
			err = writer.WriteField(name, fields[name])
		}
	}
	if err == nil {
		err = writeMultipartDeploymentFields(writer, deployment.Parameters, request.Operation)
	}
	closeErr := writer.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return encodedRequest{}, invalidRequest("failed to encode upstream image edit request", err)
	}
	return encodedRequest{body: body.Bytes(), path: "/images/edits", contentType: writer.FormDataContentType(), accept: "application/json"}, nil
}

func encodeImageVariationRequest(deployment catalog.Deployment, request contract.Request) (encodedRequest, error) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	variation := request.ImageVariation
	err := writeMultipartFile(writer, "image", variation.Image)
	fields := map[string]string{"model": deployment.UpstreamModel, "size": variation.Size,
		"response_format": variation.ResponseFormat, "user": variation.User}
	if variation.N > 0 {
		fields["n"] = fmt.Sprint(variation.N)
	}
	for _, name := range []string{"model", "n", "response_format", "size", "user"} {
		if err == nil && fields[name] != "" {
			err = writer.WriteField(name, fields[name])
		}
	}
	if err == nil {
		err = writeMultipartDeploymentFields(writer, deployment.Parameters, request.Operation)
	}
	closeErr := writer.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return encodedRequest{}, invalidRequest("failed to encode upstream image variation request", err)
	}
	return encodedRequest{body: body.Bytes(), path: "/images/variations", contentType: writer.FormDataContentType(), accept: "application/json"}, nil
}

func writeMultipartFile(writer *multipart.Writer, field string, file contract.MediaFile) error {
	part, err := writer.CreateFormFile(field, file.Name)
	if err != nil {
		return err
	}
	_, err = part.Write(file.Data)
	return err
}

func writeMultipartDeploymentFields(writer *multipart.Writer, raw json.RawMessage, operation contract.Operation) error {
	parameters, err := parseDeploymentParameters(raw)
	if err != nil {
		return err
	}
	values := parameters.forOperation(operation)
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		var value any
		if err := json.Unmarshal(values[name], &value); err != nil {
			return err
		}
		switch typed := value.(type) {
		case string:
			err = writer.WriteField(name, typed)
		case float64, bool:
			err = writer.WriteField(name, fmt.Sprint(typed))
		default:
			return fmt.Errorf("%s deployment parameter %q must be a scalar", operation, name)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func speechContentType(format string) string {
	switch format {
	case "opus":
		return "audio/ogg"
	case "aac":
		return "audio/aac"
	case "flac":
		return "audio/flac"
	case "wav":
		return "audio/wav"
	case "pcm":
		return "application/octet-stream"
	default:
		return "audio/mpeg"
	}
}

func encodeModerationRequest(model string, request contract.Request) map[string]any {
	input := make([]any, 0, len(request.Moderation.Input))
	allText := true
	for _, part := range request.Moderation.Input {
		if part.Type != "text" {
			allText = false
			break
		}
		input = append(input, part.Text)
	}
	if !allText {
		input = input[:0]
		for _, part := range request.Moderation.Input {
			switch part.Type {
			case "text":
				input = append(input, map[string]any{"type": "text", "text": part.Text})
			case "image_url":
				image := map[string]any{"url": part.ImageURL.URL}
				if part.ImageURL.Detail != "" {
					image["detail"] = part.ImageURL.Detail
				}
				input = append(input, map[string]any{"type": "image_url", "image_url": image})
			}
		}
	}
	var encodedInput any = input
	if allText && len(input) == 1 {
		encodedInput = input[0]
	}
	return map[string]any{"model": model, "input": encodedInput}
}

func encodeRerankRequest(model string, request contract.Request) map[string]any {
	rerank := request.Rerank
	documents := make([]any, 0, len(rerank.Documents))
	for _, document := range rerank.Documents {
		if document.Text != "" {
			documents = append(documents, document.Text)
		} else {
			documents = append(documents, json.RawMessage(document.Object))
		}
	}
	value := map[string]any{"model": model, "query": rerank.Query, "documents": documents, "top_n": rerank.TopN}
	if len(rerank.RankFields) != 0 {
		value["rank_fields"] = rerank.RankFields
	}
	if rerank.ReturnDocuments != nil {
		value["return_documents"] = *rerank.ReturnDocuments
	}
	if rerank.MaxChunksPerDoc != 0 {
		value["max_chunks_per_doc"] = rerank.MaxChunksPerDoc
	}
	if rerank.MaxTokensPerDoc != 0 {
		value["max_tokens_per_doc"] = rerank.MaxTokensPerDoc
	}
	if rerank.Instruction != "" {
		value["instruction"] = rerank.Instruction
	}
	return value
}

func encodeTextCompletionRequest(model string, request contract.Request) map[string]any {
	completion := request.TextCompletion
	value := map[string]any{"model": model, "prompt": encodeCompletionPrompt(completion.Prompt), "stream": request.Stream,
		"max_tokens": completion.MaxTokens, "n": completion.N}
	if request.Stream {
		value["stream_options"] = map[string]any{"include_usage": true}
	}
	if completion.BestOf != completion.N {
		value["best_of"] = completion.BestOf
	}
	if completion.Echo != nil {
		value["echo"] = *completion.Echo
	}
	if completion.FrequencyPenalty != nil {
		value["frequency_penalty"] = *completion.FrequencyPenalty
	}
	if len(completion.LogitBias) != 0 {
		value["logit_bias"] = completion.LogitBias
	}
	if completion.Logprobs != nil {
		value["logprobs"] = *completion.Logprobs
	}
	if completion.PresencePenalty != nil {
		value["presence_penalty"] = *completion.PresencePenalty
	}
	if completion.Seed != nil {
		value["seed"] = *completion.Seed
	}
	if len(completion.Stop) != 0 {
		value["stop"] = completion.Stop
	}
	if completion.Suffix != "" {
		value["suffix"] = completion.Suffix
	}
	if completion.Temperature != nil {
		value["temperature"] = *completion.Temperature
	}
	if completion.TopP != nil {
		value["top_p"] = *completion.TopP
	}
	if completion.User != "" {
		value["user"] = completion.User
	}
	return value
}

func encodeCompletionPrompt(prompt contract.CompletionPrompt) any {
	if len(prompt.Texts) == 1 {
		return prompt.Texts[0]
	}
	if len(prompt.Texts) > 1 {
		return prompt.Texts
	}
	if len(prompt.Tokens) == 1 {
		return prompt.Tokens[0]
	}
	return prompt.Tokens
}

func fieldSet(fields ...string) map[string]struct{} {
	result := make(map[string]struct{}, len(fields))
	for _, field := range fields {
		result[field] = struct{}{}
	}
	return result
}

func encodeChatRequest(model string, request contract.Request) map[string]any {
	chat := request.Chat
	value := map[string]any{"model": model, "messages": encodeMessages(chat.Messages), "stream": request.Stream}
	if request.Stream {
		value["stream_options"] = map[string]any{"include_usage": true}
	}
	if len(chat.Tools) > 0 {
		value["tools"] = encodeTools(chat.Tools)
	}
	if chat.ToolChoice != nil {
		value["tool_choice"] = encodeToolChoice(*chat.ToolChoice)
	}
	if chat.ResponseFormat != nil {
		value["response_format"] = encodeChatResponseFormat(*chat.ResponseFormat)
	}
	if chat.N != 0 {
		value["n"] = chat.N
	}
	if chat.Temperature != nil {
		value["temperature"] = *chat.Temperature
	}
	if chat.TopP != nil {
		value["top_p"] = *chat.TopP
	}
	if chat.FrequencyPenalty != nil {
		value["frequency_penalty"] = *chat.FrequencyPenalty
	}
	if chat.PresencePenalty != nil {
		value["presence_penalty"] = *chat.PresencePenalty
	}
	if chat.MaxCompletionTokens > 0 {
		value["max_completion_tokens"] = chat.MaxCompletionTokens
	}
	if len(chat.Stop) > 0 {
		value["stop"] = chat.Stop
	}
	if chat.User != "" {
		value["user"] = chat.User
	}
	if chat.Seed != nil {
		value["seed"] = *chat.Seed
	}
	if chat.Logprobs {
		value["logprobs"] = true
		value["top_logprobs"] = chat.TopLogprobs
	}
	if chat.ParallelToolCalls != nil {
		value["parallel_tool_calls"] = *chat.ParallelToolCalls
	}
	if chat.ReasoningEffort != "" {
		value["reasoning_effort"] = chat.ReasoningEffort
	}
	if chat.Store != nil {
		value["store"] = *chat.Store
	}
	if chat.PromptCacheKey != "" {
		value["prompt_cache_key"] = chat.PromptCacheKey
	}
	return value
}

func encodeMessages(messages []contract.Message) []map[string]any {
	result := make([]map[string]any, 0, len(messages))
	for _, message := range messages {
		value := map[string]any{"role": message.Role}
		if message.Name != "" {
			value["name"] = message.Name
		}
		if message.ToolCallID != "" {
			value["tool_call_id"] = message.ToolCallID
		}
		if len(message.Content) == 1 && message.Content[0].Type == "text" {
			value["content"] = message.Content[0].Text
		} else if len(message.Content) > 0 {
			parts := make([]map[string]any, 0, len(message.Content))
			for _, part := range message.Content {
				encoded := map[string]any{"type": part.Type}
				switch part.Type {
				case "text":
					encoded["text"] = part.Text
				case "image_url":
					image := map[string]any{"url": part.ImageURL.URL}
					if part.ImageURL.Detail != "" {
						image["detail"] = part.ImageURL.Detail
					}
					encoded["image_url"] = image
				case "input_audio":
					encoded["input_audio"] = map[string]any{"data": part.Audio.Data, "format": part.Audio.Format}
				}
				parts = append(parts, encoded)
			}
			value["content"] = parts
		} else {
			value["content"] = nil
		}
		if len(message.ToolCalls) > 0 {
			calls := make([]map[string]any, 0, len(message.ToolCalls))
			for _, call := range message.ToolCalls {
				calls = append(calls, map[string]any{"id": call.ID, "type": call.Type, "function": map[string]any{"name": call.Function.Name, "arguments": call.Function.Arguments}})
			}
			value["tool_calls"] = calls
		}
		result = append(result, value)
	}
	return result
}

func encodeTools(tools []contract.Tool) []map[string]any {
	result := make([]map[string]any, 0, len(tools))
	for _, tool := range tools {
		function := map[string]any{"name": tool.Function.Name, "parameters": json.RawMessage(tool.Function.Parameters)}
		if tool.Function.Description != "" {
			function["description"] = tool.Function.Description
		}
		if tool.Function.Strict != nil {
			function["strict"] = *tool.Function.Strict
		}
		result = append(result, map[string]any{"type": tool.Type, "function": function})
	}
	return result
}

func encodeToolChoice(choice contract.ToolChoice) any {
	if choice.Mode != "function" {
		return choice.Mode
	}
	return map[string]any{"type": "function", "function": map[string]any{"name": choice.FunctionName}}
}

func encodeChatResponseFormat(format contract.ResponseFormat) map[string]any {
	value := map[string]any{"type": format.Type}
	if format.JSONSchema != nil {
		schema := map[string]any{"name": format.JSONSchema.Name, "schema": json.RawMessage(format.JSONSchema.Schema)}
		if format.JSONSchema.Description != "" {
			schema["description"] = format.JSONSchema.Description
		}
		if format.JSONSchema.Strict != nil {
			schema["strict"] = *format.JSONSchema.Strict
		}
		value["json_schema"] = schema
	}
	return value
}

func encodeResponsesRequest(model string, request contract.Request) map[string]any {
	responses := request.Responses
	value := map[string]any{"model": model, "input": encodeResponseInput(responses.Input), "stream": request.Stream, "store": false}
	if responses.Instructions != "" {
		value["instructions"] = responses.Instructions
	}
	if len(responses.Tools) > 0 {
		value["tools"] = encodeResponseTools(responses.Tools)
	}
	if responses.ToolChoice != nil {
		value["tool_choice"] = encodeResponseToolChoice(*responses.ToolChoice)
	}
	if responses.TextFormat != nil {
		value["text"] = map[string]any{"format": encodeResponseTextFormat(*responses.TextFormat)}
	}
	if request.MaxOutputTokens > 0 {
		value["max_output_tokens"] = request.MaxOutputTokens
	}
	if responses.Temperature != nil {
		value["temperature"] = *responses.Temperature
	}
	if responses.TopP != nil {
		value["top_p"] = *responses.TopP
	}
	if responses.ParallelToolCalls != nil {
		value["parallel_tool_calls"] = *responses.ParallelToolCalls
	}
	if responses.ReasoningEffort != "" || responses.ReasoningSummary != "" {
		reasoning := map[string]any{}
		if responses.ReasoningEffort != "" {
			reasoning["effort"] = responses.ReasoningEffort
		}
		if responses.ReasoningSummary != "" {
			reasoning["summary"] = responses.ReasoningSummary
		}
		value["reasoning"] = reasoning
	}
	if responses.PromptCacheKey != "" {
		value["prompt_cache_key"] = responses.PromptCacheKey
	}
	if responses.SafetyIdentifier != "" {
		value["safety_identifier"] = responses.SafetyIdentifier
	}
	if responses.ServiceTier != "" {
		value["service_tier"] = responses.ServiceTier
	}
	if responses.Truncation != "" {
		value["truncation"] = responses.Truncation
	}
	if responses.User != "" {
		value["user"] = responses.User
	}
	if responses.TopLogprobs != nil {
		value["top_logprobs"] = *responses.TopLogprobs
	}
	return value
}

func encodeResponseCompactionRequest(model string, request contract.Request) map[string]any {
	responses := request.Responses
	value := map[string]any{"model": model, "input": encodeResponseInput(responses.Input)}
	if responses.Instructions != "" {
		value["instructions"] = responses.Instructions
	}
	if responses.PromptCacheKey != "" {
		value["prompt_cache_key"] = responses.PromptCacheKey
	}
	if responses.PromptCacheMode != "" || responses.PromptCacheTTL != "" {
		options := map[string]any{}
		if responses.PromptCacheMode != "" {
			options["mode"] = responses.PromptCacheMode
		}
		if responses.PromptCacheTTL != "" {
			options["ttl"] = responses.PromptCacheTTL
		}
		value["prompt_cache_options"] = options
	}
	if responses.PromptCacheRetention != "" {
		value["prompt_cache_retention"] = responses.PromptCacheRetention
	}
	if responses.ServiceTier != "" {
		value["service_tier"] = responses.ServiceTier
	}
	return value
}

func encodeResponseTools(tools []contract.Tool) []map[string]any {
	result := make([]map[string]any, 0, len(tools))
	for _, tool := range tools {
		value := map[string]any{"type": tool.Type, "name": tool.Function.Name, "parameters": json.RawMessage(tool.Function.Parameters)}
		if tool.Function.Description != "" {
			value["description"] = tool.Function.Description
		}
		if tool.Function.Strict != nil {
			value["strict"] = *tool.Function.Strict
		}
		result = append(result, value)
	}
	return result
}

func encodeResponseToolChoice(choice contract.ToolChoice) any {
	if choice.Mode != "function" {
		return choice.Mode
	}
	return map[string]any{"type": "function", "name": choice.FunctionName}
}

func encodeResponseInput(items []contract.ResponseInputItem) []map[string]any {
	result := make([]map[string]any, 0, len(items))
	for _, item := range items {
		value := map[string]any{"type": item.Type}
		switch item.Type {
		case "message":
			value["role"] = item.Role
			parts := make([]map[string]any, 0, len(item.Content))
			for _, part := range item.Content {
				encoded := map[string]any{"type": part.Type}
				if part.Type == "input_text" {
					encoded["text"] = part.Text
				} else if part.Type == "input_image" {
					encoded["image_url"] = part.ImageURL.URL
					if part.ImageURL.Detail != "" {
						encoded["detail"] = part.ImageURL.Detail
					}
				} else if part.Type == "input_file" {
					encoded["file_data"] = part.File.Data
					if part.File.Filename != "" {
						encoded["filename"] = part.File.Filename
					}
				}
				parts = append(parts, encoded)
			}
			value["content"] = parts
		case "function_call":
			value["call_id"], value["name"], value["arguments"] = item.CallID, item.Name, item.Arguments
		case "function_call_output":
			value["call_id"], value["output"] = item.CallID, item.Output
		case "compaction":
			value["encrypted_content"] = item.EncryptedContent
		}
		result = append(result, value)
	}
	return result
}

func encodeResponseTextFormat(format contract.ResponseFormat) map[string]any {
	value := map[string]any{"type": format.Type}
	if format.JSONSchema != nil {
		value["name"] = format.JSONSchema.Name
		value["schema"] = json.RawMessage(format.JSONSchema.Schema)
		if format.JSONSchema.Description != "" {
			value["description"] = format.JSONSchema.Description
		}
		if format.JSONSchema.Strict != nil {
			value["strict"] = *format.JSONSchema.Strict
		}
	}
	return value
}

func encodeEmbeddingsRequest(model string, request contract.Request) map[string]any {
	embeddings := request.Embeddings
	value := map[string]any{"model": model, "encoding_format": embeddings.EncodingFormat}
	if len(embeddings.Input.Texts) == 1 {
		value["input"] = embeddings.Input.Texts[0]
	} else if len(embeddings.Input.Texts) > 0 {
		value["input"] = embeddings.Input.Texts
	} else if len(embeddings.Input.Tokens) == 1 {
		value["input"] = embeddings.Input.Tokens[0]
	} else {
		value["input"] = embeddings.Input.Tokens
	}
	if embeddings.Dimensions > 0 {
		value["dimensions"] = embeddings.Dimensions
	}
	if embeddings.User != "" {
		value["user"] = embeddings.User
	}
	return value
}

func encodeImageRequest(model string, request contract.Request) map[string]any {
	image := request.ImageGeneration
	value := map[string]any{"model": model, "prompt": image.Prompt, "n": image.N, "response_format": image.ResponseFormat}
	if image.Size != "" {
		value["size"] = image.Size
	}
	if image.Quality != "" {
		value["quality"] = image.Quality
	}
	return value
}
