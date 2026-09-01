package openai

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/daptin/llmgateway/contract"
)

type BatchStore interface {
	Create(context.Context, contract.Principal, contract.CreateBatchRequest) (contract.Batch, error)
	List(context.Context, contract.Principal, contract.ListBatchesRequest) (contract.BatchPage, error)
	Get(context.Context, contract.Principal, contract.ID) (contract.Batch, error)
	Cancel(context.Context, contract.Principal, contract.ID) (contract.Batch, error)
}

type batchRequest struct {
	InputFileID        string                 `json:"input_file_id"`
	Endpoint           string                 `json:"endpoint"`
	CompletionWindow   string                 `json:"completion_window"`
	Metadata           map[string]string      `json:"metadata,omitempty"`
	OutputExpiresAfter *fileExpirationRequest `json:"output_expires_after,omitempty"`
}

type fileExpirationRequest struct {
	Anchor  string `json:"anchor"`
	Seconds int64  `json:"seconds"`
}

func (h *Handler) createBatch(response http.ResponseWriter, request *http.Request) {
	id, principal, ok := h.authenticatedRequest(response, request)
	if !ok {
		return
	}
	body, err := readJSONBody(response, request, h.maxBody)
	if err != nil {
		writeError(response, err, id)
		return
	}
	var wire batchRequest
	if err := decodeStrict(body, &wire); err != nil {
		writeError(response, gatewayError(contract.ErrorInvalidRequest, "invalid batch request", http.StatusBadRequest, false, err), id)
		return
	}
	canonical := contract.CreateBatchRequest{InputFileID: contract.ID(strings.TrimSpace(wire.InputFileID)),
		Endpoint: strings.TrimSpace(wire.Endpoint), CompletionWindow: strings.TrimSpace(wire.CompletionWindow), Metadata: wire.Metadata}
	if wire.OutputExpiresAfter != nil {
		canonical.OutputExpiresAfter = &contract.FileExpiration{Anchor: wire.OutputExpiresAfter.Anchor, Seconds: wire.OutputExpiresAfter.Seconds}
	}
	if err := validateCreateBatch(canonical); err != nil {
		writeError(response, err, id)
		return
	}
	batch, err := h.batches.Create(request.Context(), principal, canonical)
	if err != nil {
		writeError(response, err, id)
		return
	}
	encoded, err := encodeBatch(batch)
	if err != nil {
		writeError(response, err, id)
		return
	}
	writeJSON(response, http.StatusOK, id, encoded)
}

func (h *Handler) listBatches(response http.ResponseWriter, request *http.Request) {
	id, principal, ok := h.authenticatedRequest(response, request)
	if !ok {
		return
	}
	query := request.URL.Query()
	if err := rejectUnknownQuery(query, "after", "limit"); err != nil {
		writeError(response, err, id)
		return
	}
	limit := 20
	if value := strings.TrimSpace(query.Get("limit")); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 1 || parsed > 100 {
			writeError(response, gatewayError(contract.ErrorInvalidRequest, "limit must be between 1 and 100", http.StatusBadRequest, false, err), id)
			return
		}
		limit = parsed
	}
	page, err := h.batches.List(request.Context(), principal, contract.ListBatchesRequest{
		After: contract.ID(strings.TrimSpace(query.Get("after"))), Limit: limit,
	})
	if err != nil {
		writeError(response, err, id)
		return
	}
	data := make([]map[string]any, 0, len(page.Data))
	for _, batch := range page.Data {
		encoded, encodeErr := encodeBatch(batch)
		if encodeErr != nil {
			writeError(response, encodeErr, id)
			return
		}
		data = append(data, encoded)
	}
	result := map[string]any{"object": "list", "data": data, "has_more": page.HasMore}
	if len(data) > 0 {
		result["first_id"] = data[0]["id"]
		result["last_id"] = data[len(data)-1]["id"]
	}
	writeJSON(response, http.StatusOK, id, result)
}

func (h *Handler) getBatch(response http.ResponseWriter, request *http.Request) {
	id, principal, batchID, ok := h.identifiedRequest(response, request)
	if !ok {
		return
	}
	batch, err := h.batches.Get(request.Context(), principal, batchID)
	if err != nil {
		writeError(response, err, id)
		return
	}
	encoded, err := encodeBatch(batch)
	if err != nil {
		writeError(response, err, id)
		return
	}
	writeJSON(response, http.StatusOK, id, encoded)
}

func (h *Handler) cancelBatch(response http.ResponseWriter, request *http.Request) {
	id, principal, batchID, ok := h.identifiedRequest(response, request)
	if !ok {
		return
	}
	batch, err := h.batches.Cancel(request.Context(), principal, batchID)
	if err != nil {
		writeError(response, err, id)
		return
	}
	encoded, err := encodeBatch(batch)
	if err != nil {
		writeError(response, err, id)
		return
	}
	writeJSON(response, http.StatusOK, id, encoded)
}

func validateCreateBatch(request contract.CreateBatchRequest) error {
	if request.InputFileID == "" {
		return gatewayError(contract.ErrorInvalidRequest, "input_file_id is required", http.StatusBadRequest, false, nil)
	}
	if !validBatchEndpoint(request.Endpoint) {
		return gatewayError(contract.ErrorInvalidRequest, "unsupported batch endpoint", http.StatusBadRequest, false, nil)
	}
	if request.CompletionWindow != "24h" {
		return gatewayError(contract.ErrorInvalidRequest, "completion_window must be 24h", http.StatusBadRequest, false, nil)
	}
	if len(request.Metadata) > 16 {
		return gatewayError(contract.ErrorInvalidRequest, "batch metadata has too many entries", http.StatusBadRequest, false, nil)
	}
	for key, value := range request.Metadata {
		if strings.TrimSpace(key) == "" || len(key) > 64 || len(value) > 512 {
			return gatewayError(contract.ErrorInvalidRequest, "invalid batch metadata", http.StatusBadRequest, false, nil)
		}
	}
	if expiration := request.OutputExpiresAfter; expiration != nil &&
		(expiration.Anchor != "created_at" || expiration.Seconds < 3600 || expiration.Seconds > 2592000) {
		return gatewayError(contract.ErrorInvalidRequest, "invalid output_expires_after", http.StatusBadRequest, false, nil)
	}
	return nil
}

func validBatchEndpoint(endpoint string) bool {
	switch endpoint {
	case "/v1/chat/completions", "/v1/embeddings", "/v1/completions", "/v1/responses":
		return true
	default:
		return false
	}
}

func encodeBatch(batch contract.Batch) (map[string]any, error) {
	if batch.ID == "" || batch.InputFileID == "" || batch.Endpoint == "" || batch.CompletionWindow == "" ||
		!batch.Status.Valid() || batch.CreatedAt.IsZero() || batch.RequestCounts.Total < 0 || batch.RequestCounts.Completed < 0 ||
		batch.RequestCounts.Failed < 0 || batch.RequestCounts.Completed+batch.RequestCounts.Failed > batch.RequestCounts.Total {
		return nil, gatewayError(contract.ErrorInternal, "batch store returned invalid state", http.StatusInternalServerError, false, nil)
	}
	encoded := map[string]any{"id": batch.ID, "object": "batch", "endpoint": batch.Endpoint, "input_file_id": batch.InputFileID,
		"completion_window": batch.CompletionWindow, "status": batch.Status, "created_at": batch.CreatedAt.Unix(), "metadata": batch.Metadata,
		"request_counts": map[string]int64{"total": batch.RequestCounts.Total, "completed": batch.RequestCounts.Completed, "failed": batch.RequestCounts.Failed}}
	if batch.OutputFileID != "" {
		encoded["output_file_id"] = batch.OutputFileID
	}
	if batch.ErrorFileID != "" {
		encoded["error_file_id"] = batch.ErrorFileID
	}
	if len(batch.Errors) > 0 {
		data := make([]map[string]any, 0, len(batch.Errors))
		for _, item := range batch.Errors {
			value := map[string]any{"code": item.Code, "message": item.Message, "param": item.Param}
			if item.Line != nil {
				value["line"] = *item.Line
			}
			data = append(data, value)
		}
		encoded["errors"] = map[string]any{"object": "list", "data": data}
	}
	for name, value := range map[string]*time.Time{"in_progress_at": batch.InProgressAt, "expires_at": batch.ExpiresAt,
		"finalizing_at": batch.FinalizingAt, "completed_at": batch.CompletedAt, "failed_at": batch.FailedAt,
		"expired_at": batch.ExpiredAt, "cancelling_at": batch.CancellingAt, "cancelled_at": batch.CancelledAt} {
		if value != nil {
			encoded[name] = value.Unix()
		}
	}
	return encoded, nil
}
