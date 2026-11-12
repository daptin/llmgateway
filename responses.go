package llmgateway

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/daptin/llmgateway/contract"
)

// MaxResponseHistoryBytes bounds replayed context independently of its transport.
const MaxResponseHistoryBytes = 16 << 20

// StoredResponse is durable conversation state, independent of provider storage.
// Input contains the full input used for this turn, including resolved history.
type StoredResponse struct {
	ID                 contract.ID
	Input              []contract.ResponseInputItem
	Response           contract.Response
	PreviousResponseID string
}

// ResponseStore uses the host's normal ownership and operation permissions.
// Begin receives the admitted usage token and exposes a retained response ID before
// provider I/O. Hosts can retain that same capture instead of duplicating payloads.
type ResponseStore interface {
	Authorize(context.Context, contract.Principal, string) error
	Begin(context.Context, contract.Principal, contract.ReservationToken, string) (contract.ID, error)
	Save(context.Context, contract.Principal, StoredResponse) error
	Load(context.Context, contract.Principal, contract.ID) (StoredResponse, error)
	Delete(context.Context, contract.Principal, contract.ID) error
}

func (e *Engine) resolvePreviousResponse(ctx context.Context, principal contract.Principal, request *contract.Request) error {
	if request.Responses == nil {
		return nil
	}
	if request.Responses.Store || request.Responses.PreviousResponseID != "" {
		if e.responses == nil {
			return publicError(contract.ErrorUnavailable, "response storage is not configured", 503, false, nil)
		}
	}
	if request.Responses.PreviousResponseID == "" {
		if request.Responses.Store {
			if err := e.responses.Authorize(ctx, principal, ""); err != nil {
				return normalizeError(err, contract.ErrorUnavailable, 503, false)
			}
			return nil
		}
		return nil
	}
	previous, err := e.responses.Load(ctx, principal, contract.ID(request.Responses.PreviousResponseID))
	if err != nil {
		return normalizeError(err, contract.ErrorUnavailable, 503, false)
	}
	if previous.Response.Responses == nil || (previous.Response.Responses.Status != "completed" && previous.Response.Responses.Status != "incomplete") {
		return publicError(contract.ErrorInvalidRequest, "previous response has not completed", 400, false, nil)
	}
	if request.Responses.Store {
		if err := e.responses.Authorize(ctx, principal, request.Responses.PreviousResponseID); err != nil {
			return normalizeError(err, contract.ErrorUnavailable, 503, false)
		}
	}
	responseRequest := *request.Responses
	request.Responses = &responseRequest
	input := append([]contract.ResponseInputItem(nil), previous.Input...)
	for _, output := range previous.Response.Responses.Output {
		input = append(input, contract.ResponseInputItem{ID: output.ID, Type: output.Type, Role: output.Role,
			Content: output.Content, Summary: output.Summary, Status: output.Status, Action: output.Action,
			CallID: output.CallID, Name: output.Name, Arguments: output.Arguments, EncryptedContent: output.EncryptedContent})
	}
	input = append(input, request.Responses.Input...)
	encoded, err := json.Marshal(input)
	if err != nil {
		return publicError(contract.ErrorInternal, "failed to encode response history", 500, false, err)
	}
	if len(encoded) > MaxResponseHistoryBytes {
		return publicError(contract.ErrorInvalidRequest, "response history exceeds request size limit", 413, false, nil)
	}
	request.Responses.Input = input
	if int64(len(encoded)) > request.EstimatedUsage.InputTokens {
		request.EstimatedUsage.InputTokens = int64(len(encoded))
	}
	return nil
}

func (e *Engine) saveResponse(parent context.Context, prepared preparedRequest, response *contract.Response) error {
	if prepared.request.Responses == nil {
		return nil
	}
	if response.Responses == nil {
		if prepared.responseID != "" {
			return publicError(contract.ErrorProvider, "provider returned no responses response", 502, false, nil)
		}
		return nil
	}
	response.Responses.PreviousResponseID = prepared.request.Responses.PreviousResponseID
	response.Responses.Store = prepared.request.Responses.Store
	if prepared.responseID == "" {
		return nil
	}
	response.Responses.ID = string(prepared.responseID)
	stored := StoredResponse{ID: prepared.responseID, Input: prepared.request.Responses.Input, Response: *response,
		PreviousResponseID: prepared.request.Responses.PreviousResponseID}
	encoded, err := json.Marshal(stored)
	if err != nil || len(encoded) > MaxResponseHistoryBytes {
		return publicError(contract.ErrorInvalidRequest, "stored response exceeds history size limit", 413, false, err)
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), e.finalizationTimeout)
	defer cancel()
	if err := e.responses.Save(ctx, prepared.principal, stored); err != nil {
		return normalizeError(err, contract.ErrorUnavailable, 503, false)
	}
	return nil
}

func (e *Engine) failStoredResponse(ctx context.Context, prepared preparedRequest, status string) error {
	if prepared.responseID == "" {
		return nil
	}
	responseRequest := *prepared.request.Responses
	responseRequest.Input = nil
	prepared.request.Responses = &responseRequest
	response := contract.Response{Model: prepared.request.PublicModel, RequestID: prepared.request.ID,
		Responses: &contract.ResponsesResponse{Status: status}}
	return e.saveResponse(ctx, prepared, &response)
}

func (e *Engine) GetResponse(ctx context.Context, principal contract.Principal, id contract.ID) (contract.Response, error) {
	if e.responses == nil {
		return contract.Response{}, publicError(contract.ErrorUnavailable, "response storage is not configured", 503, false, nil)
	}
	stored, err := e.responses.Load(ctx, principal, id)
	if err != nil {
		return contract.Response{}, normalizeError(err, contract.ErrorUnavailable, 503, false)
	}
	return stored.Response, nil
}

func (e *Engine) DeleteResponse(ctx context.Context, principal contract.Principal, id contract.ID) error {
	if e.responses == nil {
		return publicError(contract.ErrorUnavailable, "response storage is not configured", http.StatusServiceUnavailable, false, nil)
	}
	return e.responses.Delete(ctx, principal, id)
}

// Stored-response controls belong to the host. Providers receive complete input
// and must not retain or resolve gateway-owned conversation references.
func statelessProviderRequest(request contract.Request) contract.Request {
	if request.Operation == contract.OperationResponses && request.Responses != nil {
		responses := *request.Responses
		responses.Store = false
		responses.PreviousResponseID = ""
		request.Responses = &responses
	}
	return request
}
