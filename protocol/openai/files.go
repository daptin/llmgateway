package openai

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/daptin/llmgateway/contract"
)

// FileStore is the persistence boundary for the host-composed Files API. The
// protocol owns validation and wire compatibility; the host owns storage,
// permissions, and durability.
type FileStore interface {
	Create(context.Context, contract.Principal, contract.CreateFileRequest) (contract.File, error)
	List(context.Context, contract.Principal, contract.ListFilesRequest) (contract.FilePage, error)
	Get(context.Context, contract.Principal, contract.ID) (contract.File, error)
	Content(context.Context, contract.Principal, contract.ID) (contract.FileContent, error)
	Delete(context.Context, contract.Principal, contract.ID) error
}

func (h *Handler) createFile(response http.ResponseWriter, request *http.Request) {
	id, principal, ok := h.authenticatedRequest(response, request)
	if !ok {
		return
	}
	values, err := readMultipartForm(response, request, h.maxBody)
	if err != nil {
		writeError(response, err, id)
		return
	}
	for name, entries := range values {
		if (name != "file" && name != "purpose" && name != "expires_after[anchor]" && name != "expires_after[seconds]") || len(entries) != 1 {
			writeError(response, gatewayError(contract.ErrorInvalidRequest, "unsupported or duplicate multipart field", http.StatusBadRequest, false, nil), id)
			return
		}
	}
	file, fileOK := oneMultipartValue(values, "file")
	purpose := contract.FilePurpose(strings.TrimSpace(multipartText(values, "purpose")))
	if !fileOK || file.filename == "" || len(file.data) == 0 || !purpose.Valid() {
		writeError(response, gatewayError(contract.ErrorInvalidRequest, "file and valid purpose are required", http.StatusBadRequest, false, nil), id)
		return
	}
	if purpose == contract.FilePurposeBatch {
		if !strings.EqualFold(filepath.Ext(file.filename), ".jsonl") {
			writeError(response, gatewayError(contract.ErrorInvalidRequest, "batch file must use a .jsonl filename", http.StatusBadRequest, false, nil), id)
			return
		}
		if _, err := DecodeBatchInput(file.data, ""); err != nil {
			writeError(response, gatewayError(contract.ErrorInvalidRequest, err.Error(), http.StatusBadRequest, false, err), id)
			return
		}
	}
	var expiration *contract.FileExpiration
	anchor := strings.TrimSpace(multipartText(values, "expires_after[anchor]"))
	secondsText := strings.TrimSpace(multipartText(values, "expires_after[seconds]"))
	if anchor != "" || secondsText != "" {
		seconds, parseErr := strconv.ParseInt(secondsText, 10, 64)
		if anchor != "created_at" || parseErr != nil || seconds < 3600 || seconds > 2592000 {
			writeError(response, gatewayError(contract.ErrorInvalidRequest, "expires_after must use created_at and 3600..2592000 seconds", http.StatusBadRequest, false, parseErr), id)
			return
		}
		expiration = &contract.FileExpiration{Anchor: anchor, Seconds: seconds}
	}
	created, err := h.files.Create(request.Context(), principal, contract.CreateFileRequest{
		Filename: file.filename, ContentType: file.contentType, Purpose: purpose, Data: file.data, ExpiresAfter: expiration,
	})
	if err != nil {
		writeError(response, err, id)
		return
	}
	encoded, err := encodeFile(created)
	if err != nil {
		writeError(response, err, id)
		return
	}
	writeJSON(response, http.StatusOK, id, encoded)
}

func (h *Handler) listFiles(response http.ResponseWriter, request *http.Request) {
	id, principal, ok := h.authenticatedRequest(response, request)
	if !ok {
		return
	}
	query := request.URL.Query()
	if err := rejectUnknownQuery(query, "purpose", "after", "limit", "order"); err != nil {
		writeError(response, err, id)
		return
	}
	purpose := contract.FilePurpose(strings.TrimSpace(query.Get("purpose")))
	if purpose != "" && !purpose.Valid() {
		writeError(response, gatewayError(contract.ErrorInvalidRequest, "invalid file purpose", http.StatusBadRequest, false, nil), id)
		return
	}
	limit := 10000
	if text := strings.TrimSpace(query.Get("limit")); text != "" {
		parsed, err := strconv.Atoi(text)
		if err != nil || parsed < 1 || parsed > 10000 {
			writeError(response, gatewayError(contract.ErrorInvalidRequest, "limit must be between 1 and 10000", http.StatusBadRequest, false, err), id)
			return
		}
		limit = parsed
	}
	order := strings.TrimSpace(query.Get("order"))
	if order == "" {
		order = "desc"
	}
	if order != "asc" && order != "desc" {
		writeError(response, gatewayError(contract.ErrorInvalidRequest, "order must be asc or desc", http.StatusBadRequest, false, nil), id)
		return
	}
	page, err := h.files.List(request.Context(), principal, contract.ListFilesRequest{
		Purpose: purpose, After: contract.ID(strings.TrimSpace(query.Get("after"))), Limit: limit, Order: order,
	})
	if err != nil {
		writeError(response, err, id)
		return
	}
	data := make([]map[string]any, 0, len(page.Data))
	for _, file := range page.Data {
		encoded, encodeErr := encodeFile(file)
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

func (h *Handler) getFile(response http.ResponseWriter, request *http.Request) {
	id, principal, fileID, ok := h.identifiedRequest(response, request)
	if !ok {
		return
	}
	file, err := h.files.Get(request.Context(), principal, fileID)
	if err != nil {
		writeError(response, err, id)
		return
	}
	encoded, err := encodeFile(file)
	if err != nil {
		writeError(response, err, id)
		return
	}
	writeJSON(response, http.StatusOK, id, encoded)
}

func (h *Handler) getFileContent(response http.ResponseWriter, request *http.Request) {
	id, principal, fileID, ok := h.identifiedRequest(response, request)
	if !ok {
		return
	}
	content, err := h.files.Content(request.Context(), principal, fileID)
	if err != nil {
		writeError(response, err, id)
		return
	}
	if len(content.Data) == 0 {
		writeError(response, gatewayError(contract.ErrorInternal, "stored file is empty", http.StatusInternalServerError, false, nil), id)
		return
	}
	contentType := strings.TrimSpace(content.ContentType)
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	response.Header().Set("Content-Type", contentType)
	response.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", safeDownloadName(content.Filename)))
	response.Header().Set("X-Request-ID", string(id))
	response.WriteHeader(http.StatusOK)
	_, _ = response.Write(content.Data)
}

func (h *Handler) deleteFile(response http.ResponseWriter, request *http.Request) {
	id, principal, fileID, ok := h.identifiedRequest(response, request)
	if !ok {
		return
	}
	if err := h.files.Delete(request.Context(), principal, fileID); err != nil {
		writeError(response, err, id)
		return
	}
	writeJSON(response, http.StatusOK, id, map[string]any{"id": fileID, "object": "file", "deleted": true})
}

func encodeFile(file contract.File) (map[string]any, error) {
	if file.ID == "" || file.Bytes < 0 || file.CreatedAt.IsZero() || file.Filename == "" || !file.Purpose.Valid() {
		return nil, gatewayError(contract.ErrorInternal, "file store returned invalid metadata", http.StatusInternalServerError, false, nil)
	}
	encoded := map[string]any{"id": file.ID, "object": "file", "bytes": file.Bytes, "created_at": file.CreatedAt.Unix(),
		"filename": file.Filename, "purpose": file.Purpose}
	if file.Status != "" {
		encoded["status"] = file.Status
	}
	if file.StatusDetails != "" {
		encoded["status_details"] = file.StatusDetails
	}
	if file.ExpiresAt != nil {
		encoded["expires_at"] = file.ExpiresAt.Unix()
	}
	return encoded, nil
}

func safeDownloadName(name string) string {
	name = strings.ReplaceAll(name, "\\", "_")
	name = strings.ReplaceAll(name, "/", "_")
	name = strings.ReplaceAll(name, "\r", "_")
	name = strings.ReplaceAll(name, "\n", "_")
	if strings.TrimSpace(name) == "" {
		return "file"
	}
	return name
}
