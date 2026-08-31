package openai

import (
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/daptin/llmgateway/catalog"
	"github.com/daptin/llmgateway/contract"
)

func (h *Handler) models(response http.ResponseWriter, request *http.Request) {
	id, idErr := h.requestID(request)
	if idErr != nil {
		writeError(response, idErr, "")
		return
	}
	principal, err := h.authenticate(request)
	if err != nil {
		writeError(response, err, id)
		return
	}
	snapshot, err := h.engine.Snapshot()
	if err != nil {
		writeError(response, err, id)
		return
	}
	models := make([]map[string]any, 0)
	for _, model := range snapshot.Models() {
		if !model.Enabled || h.engine.Authorize(request.Context(), principal, model.Name) != nil {
			continue
		}
		models = append(models, encodeModel(model))
	}
	sort.Slice(models, func(i, j int) bool { return models[i]["id"].(string) < models[j]["id"].(string) })
	writeJSON(response, http.StatusOK, id, map[string]any{"object": "list", "data": models})
}

func (h *Handler) model(response http.ResponseWriter, request *http.Request) {
	id, idErr := h.requestID(request)
	if idErr != nil {
		writeError(response, idErr, "")
		return
	}
	principal, err := h.authenticate(request)
	if err != nil {
		writeError(response, err, id)
		return
	}
	snapshot, err := h.engine.Snapshot()
	if err != nil {
		writeError(response, err, id)
		return
	}
	name := strings.TrimSpace(request.PathValue("id"))
	model, ok := snapshot.ModelByName(name)
	if !ok || !model.Enabled || h.engine.Authorize(request.Context(), principal, model.Name) != nil {
		writeError(response, gatewayError(contract.ErrorModelNotFound, "model not found", http.StatusNotFound, false, nil), id)
		return
	}
	writeJSON(response, http.StatusOK, id, encodeModel(model))
}

func encodeModel(model catalog.Model) map[string]any {
	operations := make([]string, 0, len(model.Operations))
	for _, operation := range model.Operations {
		operations = append(operations, string(operation))
	}
	sort.Strings(operations)
	return map[string]any{
		"id": model.Name, "object": "model", "created": time.Unix(0, 0).Unix(), "owned_by": "daptin",
		"daptin": map[string]any{"operations": operations, "capabilities": model.Capabilities},
	}
}
