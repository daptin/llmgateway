package openai

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/daptin/llmgateway/contract"
)

type searchRequest struct {
	SearchToolName   string          `json:"search_tool_name,omitempty"`
	Query            json.RawMessage `json:"query"`
	MaxResults       *int            `json:"max_results,omitempty"`
	DomainFilter     []string        `json:"search_domain_filter,omitempty"`
	MaxTokensPerPage *int            `json:"max_tokens_per_page,omitempty"`
	Country          string          `json:"country,omitempty"`
}

func (h *Handler) search(response http.ResponseWriter, request *http.Request) {
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
	var wire searchRequest
	if err := decodeStrict(body, &wire); err != nil {
		writeError(response, gatewayError(contract.ErrorInvalidRequest, "invalid search request", http.StatusBadRequest, false, err), id)
		return
	}
	tool := strings.TrimSpace(request.PathValue("tool"))
	if tool == "" {
		tool = strings.TrimSpace(wire.SearchToolName)
	} else if wire.SearchToolName != "" && strings.TrimSpace(wire.SearchToolName) != tool {
		writeError(response, gatewayError(contract.ErrorInvalidRequest, "search tool path and body disagree", http.StatusBadRequest, false, nil), id)
		return
	}
	queries, err := searchQueries(wire.Query)
	if err != nil || tool == "" {
		writeError(response, gatewayError(contract.ErrorInvalidRequest, "search tool and query are required", http.StatusBadRequest, false, err), id)
		return
	}
	maximum, pageTokens := 10, 1024
	if wire.MaxResults != nil {
		maximum = *wire.MaxResults
	}
	if wire.MaxTokensPerPage != nil {
		pageTokens = *wire.MaxTokensPerPage
	}
	canonical := contract.Request{ID: id, Operation: contract.OperationSearch, PublicModel: tool,
		EstimatedUsage: contract.Usage{Estimated: true, Measures: map[string]int64{"request_bytes": int64(len(body)), "search_queries": int64(len(queries)), "search_results": int64(maximum)}},
		Search:         &contract.SearchRequest{Queries: queries, MaxResults: maximum, DomainFilter: append([]string(nil), wire.DomainFilter...), MaxTokensPerPage: pageTokens, Country: wire.Country}}
	if err := canonical.Validate(); err != nil {
		writeError(response, gatewayError(contract.ErrorInvalidRequest, "invalid search request", http.StatusBadRequest, false, err), id)
		return
	}
	result, err := h.engine.Invoke(request.Context(), principal, canonical)
	if err != nil {
		writeError(response, err, id)
		return
	}
	if result.Search == nil || result.Search.Results == nil {
		writeError(response, gatewayError(contract.ErrorProvider, "provider returned no search response", http.StatusBadGateway, false, nil), id)
		return
	}
	results := make([]map[string]any, 0, len(result.Search.Results))
	for _, item := range result.Search.Results {
		value := map[string]any{"title": item.Title, "url": item.URL, "snippet": item.Snippet}
		if item.Date != "" {
			value["date"] = item.Date
		}
		if item.LastUpdated != "" {
			value["last_updated"] = item.LastUpdated
		}
		results = append(results, value)
	}
	writeJSON(response, http.StatusOK, id, map[string]any{"object": "search", "results": results})
}

func searchQueries(raw json.RawMessage) ([]string, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, errors.New("query is required")
	}
	if trimmed[0] == '"' {
		var query string
		if err := json.Unmarshal(trimmed, &query); err != nil || query == "" {
			return nil, errors.New("query is invalid")
		}
		return []string{query}, nil
	}
	var queries []string
	if err := json.Unmarshal(trimmed, &queries); err != nil || len(queries) == 0 {
		return nil, errors.New("query must be a string or string array")
	}
	return queries, nil
}
