package openai

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/daptin/llmgateway/contract"
)

type rerankRequest struct {
	Model           string            `json:"model"`
	Query           string            `json:"query"`
	Documents       []json.RawMessage `json:"documents"`
	TopN            *int              `json:"top_n,omitempty"`
	RankFields      []string          `json:"rank_fields,omitempty"`
	ReturnDocuments *bool             `json:"return_documents,omitempty"`
	MaxChunksPerDoc *int              `json:"max_chunks_per_doc,omitempty"`
	MaxTokensPerDoc *int              `json:"max_tokens_per_doc,omitempty"`
	Instruction     string            `json:"instruction,omitempty"`
}

func (h *Handler) rerank(response http.ResponseWriter, request *http.Request) {
	id, err := h.requestID(request)
	if err != nil {
		writeError(response, err, "")
		return
	}
	principal, err := h.authenticate(request)
	if err != nil {
		writeError(response, err, id)
		return
	}
	body, err := readJSONBody(response, request, h.maxBody)
	if err != nil {
		writeError(response, err, id)
		return
	}
	var wire rerankRequest
	if err := decodeStrict(body, &wire); err != nil {
		writeError(response, gatewayError(contract.ErrorInvalidRequest, "invalid rerank request", http.StatusBadRequest, false, err), id)
		return
	}
	canonical, err := canonicalRerank(id, wire, int64(len(body)))
	if err != nil {
		writeError(response, err, id)
		return
	}
	result, err := h.engine.Invoke(request.Context(), principal, canonical)
	if err != nil {
		writeError(response, err, id)
		return
	}
	encoded, err := encodeRerankResponse(result)
	if err != nil {
		writeError(response, err, id)
		return
	}
	writeJSON(response, http.StatusOK, id, encoded)
}

func canonicalRerank(id contract.ID, wire rerankRequest, requestBytes int64) (contract.Request, error) {
	if strings.TrimSpace(wire.Model) == "" || wire.Query == "" || len(wire.Documents) == 0 {
		return contract.Request{}, gatewayError(contract.ErrorInvalidRequest, "model, query, and documents are required", http.StatusBadRequest, false, nil)
	}
	documents := make([]contract.RerankDocument, 0, len(wire.Documents))
	for _, raw := range wire.Documents {
		value := bytes.TrimSpace(raw)
		if len(value) == 0 {
			return contract.Request{}, gatewayError(contract.ErrorInvalidRequest, "rerank document is empty", http.StatusBadRequest, false, nil)
		}
		if value[0] == '"' {
			var text string
			if err := json.Unmarshal(value, &text); err != nil || text == "" {
				return contract.Request{}, gatewayError(contract.ErrorInvalidRequest, "rerank document text is invalid", http.StatusBadRequest, false, err)
			}
			documents = append(documents, contract.RerankDocument{Text: text})
			continue
		}
		var object map[string]any
		if err := json.Unmarshal(value, &object); err != nil || object == nil {
			return contract.Request{}, gatewayError(contract.ErrorInvalidRequest, "rerank document object is invalid", http.StatusBadRequest, false, err)
		}
		documents = append(documents, contract.RerankDocument{Object: append([]byte(nil), value...)})
	}
	topN := len(documents)
	if wire.TopN != nil {
		topN = *wire.TopN
	}
	maxChunks, maxTokens := 0, 0
	if wire.MaxChunksPerDoc != nil {
		maxChunks = *wire.MaxChunksPerDoc
	}
	if wire.MaxTokensPerDoc != nil {
		maxTokens = *wire.MaxTokensPerDoc
	}
	if topN < 1 || topN > len(documents) || maxChunks < 0 || maxTokens < 0 {
		return contract.Request{}, gatewayError(contract.ErrorInvalidRequest, "invalid rerank bounds", http.StatusBadRequest, false, nil)
	}
	for _, field := range wire.RankFields {
		if field == "" {
			return contract.Request{}, gatewayError(contract.ErrorInvalidRequest, "rank_fields cannot contain empty values", http.StatusBadRequest, false, nil)
		}
	}
	return contract.Request{ID: id, Operation: contract.OperationRerank, PublicModel: wire.Model,
		EstimatedUsage: contract.Usage{InputTokens: requestBytes, TotalTokens: requestBytes, Estimated: true},
		Rerank: &contract.RerankRequest{Query: wire.Query, Documents: documents, TopN: topN, RankFields: wire.RankFields,
			ReturnDocuments: wire.ReturnDocuments, MaxChunksPerDoc: maxChunks, MaxTokensPerDoc: maxTokens, Instruction: wire.Instruction}}, nil
}

func encodeRerankResponse(response contract.Response) (map[string]any, error) {
	if response.Rerank == nil || len(response.Rerank.Results) == 0 {
		return nil, gatewayError(contract.ErrorProvider, "provider returned no rerank result", http.StatusBadGateway, false, nil)
	}
	results := make([]map[string]any, 0, len(response.Rerank.Results))
	for _, result := range response.Rerank.Results {
		encoded := map[string]any{"index": result.Index, "relevance_score": result.RelevanceScore}
		if result.Document != nil {
			document, err := encodeRerankDocument(*result.Document)
			if err != nil {
				return nil, gatewayError(contract.ErrorProvider, "provider returned an invalid rerank document", http.StatusBadGateway, false, err)
			}
			encoded["document"] = document
		}
		results = append(results, encoded)
	}
	meta := map[string]any{}
	if response.Rerank.Meta.SearchUnits != 0 {
		meta["billed_units"] = map[string]any{"search_units": response.Rerank.Meta.SearchUnits}
	}
	if response.Rerank.Meta.InputTokens != 0 || response.Rerank.Meta.OutputTokens != 0 {
		meta["tokens"] = map[string]any{"input_tokens": response.Rerank.Meta.InputTokens, "output_tokens": response.Rerank.Meta.OutputTokens}
	}
	return map[string]any{"id": response.Rerank.ID, "results": results, "meta": meta}, nil
}

func encodeRerankDocument(document contract.RerankDocument) (any, error) {
	if document.Text != "" {
		return map[string]any{"text": document.Text}, nil
	}
	if len(document.Object) == 0 {
		return nil, errors.New("rerank document is empty")
	}
	var object map[string]any
	if err := json.Unmarshal(document.Object, &object); err != nil || object == nil {
		return nil, errors.New("rerank document object is invalid")
	}
	return object, nil
}
