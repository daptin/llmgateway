package openaicompat

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"sort"
	"strings"
	"unicode/utf8"

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
	CompletionDetails struct {
		ReasoningTokens int64 `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
	OutputDetails struct {
		ReasoningTokens int64 `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
}

func decodeAudioResponse(request contract.Request, payload []byte, contentType string) (contract.Response, error) {
	if len(payload) == 0 {
		return contract.Response{}, providerFailure("upstream returned an empty audio response", nil)
	}
	if request.Operation == contract.OperationAudioSpeech {
		mediaType := strings.ToLower(strings.TrimSpace(strings.SplitN(contentType, ";", 2)[0]))
		if !strings.HasPrefix(mediaType, "audio/") && mediaType != "application/octet-stream" {
			return contract.Response{}, providerFailure("upstream returned a non-audio speech response", nil)
		}
		return contract.Response{AudioSpeech: &contract.AudioSpeechResponse{Data: append([]byte(nil), payload...), ContentType: mediaType}}, nil
	}
	if request.Transcription == nil {
		return contract.Response{}, invalidRequest("audio transcription payload is required", nil)
	}
	format := request.Transcription.ResponseFormat
	result := contract.Response{Transcription: &contract.AudioTranscriptionResponse{Format: format}}
	if format == "json" || format == "verbose_json" || format == "diarized_json" {
		var wire struct {
			Text  string    `json:"text"`
			Usage usageWire `json:"usage"`
		}
		if err := json.Unmarshal(payload, &wire); err != nil || wire.Text == "" || !json.Valid(payload) || bytes.TrimSpace(payload)[0] != '{' {
			return contract.Response{}, providerFailure("upstream returned an invalid transcription JSON response", err)
		}
		result.Transcription.Text = wire.Text
		result.Transcription.JSON = append([]byte(nil), payload...)
		result.Transcription.Usage = canonicalUsage(wire.Usage)
		result.Usage = result.Transcription.Usage
		return result, nil
	}
	if !utf8.Valid(payload) {
		return contract.Response{}, providerFailure("upstream returned invalid transcription text", nil)
	}
	result.Transcription.Text = string(payload)
	return result, nil
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

type textCompletionResponseWire struct {
	ID      string `json:"id"`
	Created int64  `json:"created"`
	Model   string `json:"model"`
	Choices []struct {
		Text         string          `json:"text"`
		Index        int             `json:"index"`
		Logprobs     json.RawMessage `json:"logprobs"`
		FinishReason string          `json:"finish_reason"`
	} `json:"choices"`
	Usage usageWire `json:"usage"`
}

type responsesResponseWire struct {
	ID        string            `json:"id"`
	Object    string            `json:"object"`
	CreatedAt int64             `json:"created_at"`
	Model     string            `json:"model"`
	Status    string            `json:"status"`
	Output    []json.RawMessage `json:"output"`
	Usage     usageWire         `json:"usage"`
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
	Usage usageWire `json:"usage"`
}

type moderationResponseWire struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Results []struct {
		Flagged        bool               `json:"flagged"`
		Categories     map[string]bool    `json:"categories"`
		CategoryScores map[string]float64 `json:"category_scores"`
	} `json:"results"`
}

type rerankResponseWire struct {
	ID      string `json:"id"`
	Results []struct {
		Index          int             `json:"index"`
		RelevanceScore float64         `json:"relevance_score"`
		Document       json.RawMessage `json:"document"`
	} `json:"results"`
	Meta struct {
		BilledUnits struct {
			SearchUnits int64 `json:"search_units"`
		} `json:"billed_units"`
		Tokens struct {
			InputTokens  int64 `json:"input_tokens"`
			OutputTokens int64 `json:"output_tokens"`
		} `json:"tokens"`
	} `json:"meta"`
	Usage struct {
		SearchUnits  int64 `json:"search_units"`
		InputTokens  int64 `json:"input_tokens"`
		OutputTokens int64 `json:"output_tokens"`
		TotalTokens  int64 `json:"total_tokens"`
	} `json:"usage"`
}

type searchResponseWire struct {
	Object  string `json:"object"`
	Results []struct {
		Title       string `json:"title"`
		URL         string `json:"url"`
		Snippet     string `json:"snippet"`
		Date        string `json:"date"`
		LastUpdated string `json:"last_updated"`
	} `json:"results"`
}

type ocrResponseWire struct {
	Object string `json:"object"`
	Pages  []struct {
		Index    int    `json:"index"`
		Markdown string `json:"markdown"`
		Images   []struct {
			ImageBase64 string          `json:"image_base64"`
			BBox        json.RawMessage `json:"bbox"`
		} `json:"images"`
		Dimensions *struct {
			DPI    int `json:"dpi"`
			Height int `json:"height"`
			Width  int `json:"width"`
		} `json:"dimensions"`
	} `json:"pages"`
	Model              string            `json:"model"`
	DocumentAnnotation json.RawMessage   `json:"document_annotation"`
	UsageInfo          *ocrUsageInfoWire `json:"usage_info"`
	Content            string            `json:"content"`
	Tables             []json.RawMessage `json:"tables"`
	KeyValuePairs      []json.RawMessage `json:"keyValuePairs"`
}

type ocrUsageInfoWire struct {
	PagesProcessed int64   `json:"pages_processed"`
	Credits        float64 `json:"credits"`
	DocSizeBytes   int64   `json:"doc_size_bytes"`
}

func decodeResponse(operation contract.Operation, payload []byte) (contract.Response, error) {
	switch operation {
	case contract.OperationChat:
		var wire chatResponseWire
		if err := json.Unmarshal(payload, &wire); err != nil || wire.ID == "" || len(wire.Choices) == 0 {
			return contract.Response{}, providerFailure("upstream returned an invalid chat response", err)
		}
		result := contract.Response{Model: wire.Model, Chat: &contract.ChatResponse{ID: wire.ID, Created: wire.Created}, Usage: canonicalUsage(wire.Usage)}
		indexes := make(map[int]bool, len(wire.Choices))
		for _, choice := range wire.Choices {
			if choice.Index < 0 || indexes[choice.Index] {
				return contract.Response{}, providerFailure("upstream returned invalid chat choice indices", nil)
			}
			indexes[choice.Index] = true
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
		sort.Slice(result.Chat.Choices, func(i, j int) bool { return result.Chat.Choices[i].Index < result.Chat.Choices[j].Index })
		for index := range result.Chat.Choices {
			if result.Chat.Choices[index].Index != index {
				return contract.Response{}, providerFailure("upstream returned non-contiguous chat choice indices", nil)
			}
		}
		return result, nil
	case contract.OperationTextCompletion:
		var wire textCompletionResponseWire
		if err := json.Unmarshal(payload, &wire); err != nil || wire.ID == "" || len(wire.Choices) == 0 {
			return contract.Response{}, providerFailure("upstream returned an invalid text completion response", err)
		}
		result := contract.Response{Model: wire.Model, TextCompletion: &contract.TextCompletionResponse{ID: wire.ID, Created: wire.Created}, Usage: canonicalUsage(wire.Usage)}
		indexes := make(map[int]bool, len(wire.Choices))
		for _, choice := range wire.Choices {
			if choice.Index < 0 || indexes[choice.Index] {
				return contract.Response{}, providerFailure("upstream returned invalid text completion choice indices", nil)
			}
			indexes[choice.Index] = true
			result.TextCompletion.Choices = append(result.TextCompletion.Choices, contract.TextCompletionChoice{
				Text: choice.Text, Index: choice.Index, Logprobs: append([]byte(nil), choice.Logprobs...), FinishReason: choice.FinishReason,
			})
		}
		sort.Slice(result.TextCompletion.Choices, func(i, j int) bool {
			return result.TextCompletion.Choices[i].Index < result.TextCompletion.Choices[j].Index
		})
		for index := range result.TextCompletion.Choices {
			if result.TextCompletion.Choices[index].Index != index {
				return contract.Response{}, providerFailure("upstream returned non-contiguous text completion choice indices", nil)
			}
		}
		return result, nil
	case contract.OperationResponses:
		var wire responsesResponseWire
		if err := json.Unmarshal(payload, &wire); err != nil || wire.ID == "" || wire.Status == "" || wire.Output == nil {
			return contract.Response{}, providerFailure("upstream returned an invalid responses response", err)
		}
		result := contract.Response{Model: wire.Model, Responses: &contract.ResponsesResponse{ID: wire.ID, Status: wire.Status}, Usage: canonicalUsage(wire.Usage)}
		for _, raw := range wire.Output {
			item, itemErr := decodeResponseOutputItem(raw, false)
			if itemErr != nil {
				return contract.Response{}, providerFailure("upstream returned an invalid response output item", itemErr)
			}
			result.Responses.Output = append(result.Responses.Output, item)
		}
		return result, nil
	case contract.OperationResponseCompact:
		var wire responsesResponseWire
		if err := json.Unmarshal(payload, &wire); err != nil || wire.ID == "" || wire.Object != "response.compaction" || wire.CreatedAt <= 0 || wire.Output == nil {
			return contract.Response{}, providerFailure("upstream returned an invalid compacted response", err)
		}
		result := contract.Response{Responses: &contract.ResponsesResponse{ID: wire.ID, Object: wire.Object, CreatedAt: wire.CreatedAt}, Usage: canonicalUsage(wire.Usage)}
		for _, raw := range wire.Output {
			item, itemErr := decodeResponseOutputItem(raw, true)
			if itemErr != nil {
				return contract.Response{}, providerFailure("upstream returned an invalid compaction output item", itemErr)
			}
			result.Responses.Output = append(result.Responses.Output, item)
		}
		return result, nil
	case contract.OperationEmbeddings:
		var wire embeddingsResponseWire
		if err := json.Unmarshal(payload, &wire); err != nil || len(wire.Data) == 0 {
			return contract.Response{}, providerFailure("upstream returned an invalid embeddings response", err)
		}
		result := contract.Response{Model: wire.Model, Embeddings: &contract.EmbeddingsResponse{}, Usage: canonicalUsage(wire.Usage)}
		indexes := make(map[int]bool, len(wire.Data))
		for _, item := range wire.Data {
			if item.Index < 0 || indexes[item.Index] {
				return contract.Response{}, providerFailure("upstream returned invalid embedding indices", nil)
			}
			indexes[item.Index] = true
			embedding := contract.Embedding{Index: item.Index}
			if len(item.Embedding) > 0 && item.Embedding[0] == '"' {
				if err := json.Unmarshal(item.Embedding, &embedding.Base64); err != nil || embedding.Base64 == "" {
					return contract.Response{}, providerFailure("upstream returned an invalid base64 embedding", err)
				}
			} else {
				if err := json.Unmarshal(item.Embedding, &embedding.Vector); err != nil || len(embedding.Vector) == 0 {
					return contract.Response{}, providerFailure("upstream returned an invalid embedding vector", err)
				}
			}
			result.Embeddings.Data = append(result.Embeddings.Data, embedding)
		}
		sort.Slice(result.Embeddings.Data, func(i, j int) bool { return result.Embeddings.Data[i].Index < result.Embeddings.Data[j].Index })
		for index := range result.Embeddings.Data {
			if result.Embeddings.Data[index].Index != index {
				return contract.Response{}, providerFailure("upstream returned non-contiguous embedding indices", nil)
			}
		}
		return result, nil
	case contract.OperationImageGeneration, contract.OperationImageEdit, contract.OperationImageVariation:
		var wire imagesResponseWire
		if err := json.Unmarshal(payload, &wire); err != nil || len(wire.Data) == 0 {
			return contract.Response{}, providerFailure("upstream returned an invalid image response", err)
		}
		result := contract.Response{Images: &contract.ImageResponse{Created: wire.Created}, Usage: canonicalUsage(wire.Usage)}
		for _, item := range wire.Data {
			if item.URL == "" && item.Base64 == "" {
				return contract.Response{}, providerFailure("upstream image has no content", nil)
			}
			result.Images.Data = append(result.Images.Data, contract.GeneratedImage{URL: item.URL, Base64: item.Base64, RevisedPrompt: item.RevisedPrompt})
		}
		return result, nil
	case contract.OperationModeration:
		var wire moderationResponseWire
		if err := json.Unmarshal(payload, &wire); err != nil || len(wire.Results) == 0 {
			return contract.Response{}, providerFailure("upstream returned an invalid moderation response", err)
		}
		result := contract.Response{Model: wire.Model, Moderation: &contract.ModerationResponse{ID: wire.ID}}
		for _, item := range wire.Results {
			if item.Categories == nil || item.CategoryScores == nil {
				return contract.Response{}, providerFailure("upstream moderation result is incomplete", nil)
			}
			result.Moderation.Results = append(result.Moderation.Results, contract.ModerationResult{
				Flagged: item.Flagged, Categories: item.Categories, CategoryScores: item.CategoryScores,
			})
		}
		return result, nil
	case contract.OperationRerank:
		var wire rerankResponseWire
		if err := json.Unmarshal(payload, &wire); err != nil || len(wire.Results) == 0 {
			return contract.Response{}, providerFailure("upstream returned an invalid rerank response", err)
		}
		searchUnits, inputTokens, outputTokens := wire.Meta.BilledUnits.SearchUnits, wire.Meta.Tokens.InputTokens, wire.Meta.Tokens.OutputTokens
		if searchUnits == 0 {
			searchUnits = wire.Usage.SearchUnits
		}
		if inputTokens == 0 {
			inputTokens = wire.Usage.InputTokens
		}
		if outputTokens == 0 {
			outputTokens = wire.Usage.OutputTokens
		}
		totalTokens := wire.Usage.TotalTokens
		if totalTokens == 0 {
			totalTokens = inputTokens + outputTokens
		}
		result := contract.Response{Rerank: &contract.RerankResponse{ID: wire.ID, Meta: contract.RerankMeta{
			SearchUnits: searchUnits, InputTokens: inputTokens, OutputTokens: outputTokens,
		}}, Usage: contract.Usage{InputTokens: inputTokens, OutputTokens: outputTokens, TotalTokens: totalTokens,
			Measures: map[string]int64{"search_units": searchUnits}}}
		indexes := make(map[int]bool, len(wire.Results))
		for _, item := range wire.Results {
			if item.Index < 0 || indexes[item.Index] || item.RelevanceScore < 0 {
				return contract.Response{}, providerFailure("upstream returned invalid rerank results", nil)
			}
			indexes[item.Index] = true
			reranked := contract.RerankResult{Index: item.Index, RelevanceScore: item.RelevanceScore}
			if len(bytes.TrimSpace(item.Document)) != 0 && !bytes.Equal(bytes.TrimSpace(item.Document), []byte("null")) {
				document, err := decodeRerankDocument(item.Document)
				if err != nil {
					return contract.Response{}, providerFailure("upstream returned an invalid rerank document", err)
				}
				reranked.Document = &document
			}
			result.Rerank.Results = append(result.Rerank.Results, reranked)
		}
		sort.Slice(result.Rerank.Results, func(i, j int) bool {
			return result.Rerank.Results[i].RelevanceScore > result.Rerank.Results[j].RelevanceScore
		})
		return result, nil
	case contract.OperationSearch:
		var wire searchResponseWire
		if err := json.Unmarshal(payload, &wire); err != nil || wire.Object != "search" || wire.Results == nil {
			return contract.Response{}, providerFailure("upstream returned an invalid search response", err)
		}
		result := contract.Response{Search: &contract.SearchResponse{Results: make([]contract.SearchResult, 0, len(wire.Results))},
			Usage: contract.Usage{Measures: map[string]int64{"search_results": int64(len(wire.Results))}}}
		for _, item := range wire.Results {
			if item.URL == "" {
				return contract.Response{}, providerFailure("upstream search result has no URL", nil)
			}
			result.Search.Results = append(result.Search.Results, contract.SearchResult{Title: item.Title, URL: item.URL, Snippet: item.Snippet, Date: item.Date, LastUpdated: item.LastUpdated})
		}
		return result, nil
	case contract.OperationOCR:
		return decodeOCRResponse(payload)
	default:
		return contract.Response{}, invalidRequest("unsupported operation", nil)
	}
}

func decodeOCRResponse(payload []byte) (contract.Response, error) {
	var wire ocrResponseWire
	if err := json.Unmarshal(payload, &wire); err != nil || wire.Pages == nil || wire.Model == "" || (wire.Object != "" && wire.Object != "ocr") {
		return contract.Response{}, providerFailure("upstream returned an invalid OCR response", err)
	}
	result := contract.Response{Model: wire.Model, OCR: &contract.OCRResponse{Model: wire.Model, Content: wire.Content}}
	seen := make(map[int]struct{}, len(wire.Pages))
	for _, page := range wire.Pages {
		if page.Index < 0 {
			return contract.Response{}, providerFailure("upstream returned an invalid OCR page index", nil)
		}
		if _, exists := seen[page.Index]; exists {
			return contract.Response{}, providerFailure("upstream returned duplicate OCR page indices", nil)
		}
		seen[page.Index] = struct{}{}
		canonical := contract.OCRPage{Index: page.Index, Markdown: page.Markdown}
		if page.Images != nil {
			canonical.Images = make([]contract.OCRPageImage, 0, len(page.Images))
			for _, image := range page.Images {
				if len(image.BBox) != 0 && !bytes.Equal(bytes.TrimSpace(image.BBox), []byte("null")) && !contract.ValidJSONObject(image.BBox) {
					return contract.Response{}, providerFailure("upstream returned an invalid OCR image bounding box", nil)
				}
				canonical.Images = append(canonical.Images, contract.OCRPageImage{ImageBase64: image.ImageBase64, BBox: cloneJSONValue(image.BBox)})
			}
		}
		if page.Dimensions != nil {
			if page.Dimensions.DPI < 0 || page.Dimensions.Height < 0 || page.Dimensions.Width < 0 {
				return contract.Response{}, providerFailure("upstream returned invalid OCR page dimensions", nil)
			}
			canonical.Dimensions = &contract.OCRPageDimensions{DPI: page.Dimensions.DPI, Height: page.Dimensions.Height, Width: page.Dimensions.Width}
		}
		result.OCR.Pages = append(result.OCR.Pages, canonical)
	}
	if len(wire.DocumentAnnotation) != 0 && !bytes.Equal(bytes.TrimSpace(wire.DocumentAnnotation), []byte("null")) {
		if !json.Valid(wire.DocumentAnnotation) {
			return contract.Response{}, providerFailure("upstream returned an invalid OCR document annotation", nil)
		}
		result.OCR.DocumentAnnotation = cloneJSONValue(wire.DocumentAnnotation)
	}
	var err error
	result.OCR.Tables, err = validJSONObjectList(wire.Tables)
	if err != nil {
		return contract.Response{}, providerFailure("upstream returned invalid OCR tables", err)
	}
	result.OCR.KeyValuePairs, err = validJSONObjectList(wire.KeyValuePairs)
	if err != nil {
		return contract.Response{}, providerFailure("upstream returned invalid OCR key-value pairs", err)
	}
	measures := map[string]int64{"ocr_pages": int64(len(wire.Pages))}
	if wire.UsageInfo != nil {
		if wire.UsageInfo.PagesProcessed < 0 || wire.UsageInfo.DocSizeBytes < 0 || math.IsNaN(wire.UsageInfo.Credits) || math.IsInf(wire.UsageInfo.Credits, 0) || wire.UsageInfo.Credits < 0 || wire.UsageInfo.Credits > float64(math.MaxInt64)/1_000_000 {
			return contract.Response{}, providerFailure("upstream returned invalid OCR usage", nil)
		}
		result.OCR.UsageInfo = &contract.OCRUsageInfo{PagesProcessed: wire.UsageInfo.PagesProcessed, Credits: wire.UsageInfo.Credits, DocSizeBytes: wire.UsageInfo.DocSizeBytes}
		if wire.UsageInfo.PagesProcessed != 0 {
			measures["ocr_pages"] = wire.UsageInfo.PagesProcessed
		}
		if wire.UsageInfo.DocSizeBytes != 0 {
			measures["document_bytes"] = wire.UsageInfo.DocSizeBytes
		}
		if wire.UsageInfo.Credits != 0 {
			measures["ocr_credit_micros"] = int64(math.Ceil(wire.UsageInfo.Credits * 1_000_000))
		}
	}
	result.Usage.Measures = measures
	return result, nil
}

func validJSONObjectList(values []json.RawMessage) ([]json.RawMessage, error) {
	if values == nil {
		return nil, nil
	}
	result := make([]json.RawMessage, 0, len(values))
	for _, value := range values {
		if !contract.ValidJSONObject(value) {
			return nil, errors.New("value must be an object")
		}
		result = append(result, cloneJSONValue(value))
	}
	return result, nil
}

func cloneJSONValue(value json.RawMessage) json.RawMessage {
	trimmed := bytes.TrimSpace(value)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil
	}
	return append(json.RawMessage(nil), trimmed...)
}

func decodeRerankDocument(raw json.RawMessage) (contract.RerankDocument, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return contract.RerankDocument{}, errors.New("rerank document is empty")
	}
	if trimmed[0] == '"' {
		var text string
		if err := json.Unmarshal(trimmed, &text); err != nil || text == "" {
			return contract.RerankDocument{}, errors.New("rerank document text is invalid")
		}
		return contract.RerankDocument{Text: text}, nil
	}
	var object map[string]any
	if err := json.Unmarshal(trimmed, &object); err != nil || object == nil {
		return contract.RerankDocument{}, errors.New("rerank document object is invalid")
	}
	return contract.RerankDocument{Object: append([]byte(nil), trimmed...)}, nil
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
	reasoning := wire.OutputDetails.ReasoningTokens
	if reasoning == 0 {
		reasoning = wire.CompletionDetails.ReasoningTokens
	}
	return contract.Usage{InputTokens: input, OutputTokens: output, TotalTokens: total, CacheReadTokens: cacheRead, ReasoningTokens: reasoning}
}
