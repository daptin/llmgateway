package openai

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/daptin/llmgateway/catalog"
	"github.com/daptin/llmgateway/contract"
)

type Engine interface {
	Invoke(context.Context, contract.Principal, contract.Request) (contract.Response, error)
	Stream(context.Context, contract.Principal, contract.Request) (contract.EventStream, error)
	Snapshot() (*catalog.Snapshot, error)
	Authorize(context.Context, contract.Principal, string) error
}

type Authenticator interface {
	Authenticate(context.Context, string) (contract.Principal, error)
}

type Options struct {
	MaxBodyBytes           int64
	DefaultMaxOutputTokens int64
	TotalRequestTimeout    time.Duration
	NewRequestID           func() (contract.ID, error)
}

type Handler struct {
	engine    Engine
	auth      Authenticator
	maxBody   int64
	maxOutput int64
	timeout   time.Duration
	newID     func() (contract.ID, error)
	mux       *http.ServeMux
}

func NewHandler(engine Engine, authenticator Authenticator, options Options) (*Handler, error) {
	if engine == nil || authenticator == nil {
		return nil, errors.New("engine and authenticator are required")
	}
	if options.MaxBodyBytes == 0 {
		options.MaxBodyBytes = 16 << 20
	}
	if options.DefaultMaxOutputTokens == 0 {
		options.DefaultMaxOutputTokens = 4096
	}
	if options.TotalRequestTimeout == 0 {
		options.TotalRequestTimeout = 2 * time.Minute
	}
	if options.MaxBodyBytes < 1 || options.DefaultMaxOutputTokens < 1 || options.TotalRequestTimeout < 0 {
		return nil, errors.New("handler bounds must be positive")
	}
	if options.NewRequestID == nil {
		options.NewRequestID = randomRequestID
	}
	handler := &Handler{engine: engine, auth: authenticator, maxBody: options.MaxBodyBytes, maxOutput: options.DefaultMaxOutputTokens, timeout: options.TotalRequestTimeout, newID: options.NewRequestID, mux: http.NewServeMux()}
	handler.mux.HandleFunc("POST /v1/chat/completions", handler.chatCompletions)
	handler.mux.HandleFunc("POST /v1/responses", handler.responses)
	handler.mux.HandleFunc("POST /v1/embeddings", handler.embeddings)
	handler.mux.HandleFunc("POST /v1/images/generations", handler.imageGenerations)
	handler.mux.HandleFunc("GET /v1/models", handler.models)
	handler.mux.HandleFunc("GET /v1/models/{id}", handler.model)
	handler.mux.HandleFunc("/", handler.notFound)
	return handler, nil
}

func (h *Handler) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	if h.timeout > 0 {
		ctx, cancel := context.WithTimeout(request.Context(), h.timeout)
		defer cancel()
		request = request.WithContext(ctx)
	}
	h.mux.ServeHTTP(response, request)
}

func (h *Handler) authenticate(request *http.Request) (contract.Principal, error) {
	header := request.Header.Get("Authorization")
	parts := strings.SplitN(header, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || strings.TrimSpace(parts[1]) == "" {
		return contract.Principal{}, gatewayError(contract.ErrorAuthentication, "missing or invalid bearer token", http.StatusUnauthorized, false, nil)
	}
	principal, err := h.auth.Authenticate(request.Context(), strings.TrimSpace(parts[1]))
	if err != nil {
		var public *contract.Error
		if errors.As(err, &public) {
			return contract.Principal{}, public.Safe()
		}
		return contract.Principal{}, gatewayError(contract.ErrorAuthentication, "invalid API key", http.StatusUnauthorized, false, err)
	}
	return principal, nil
}

func readJSONBody(response http.ResponseWriter, request *http.Request, maximum int64) ([]byte, error) {
	if encoding := request.Header.Get("Content-Encoding"); encoding != "" && encoding != "identity" {
		return nil, gatewayError(contract.ErrorInvalidRequest, "compressed request bodies are not supported", http.StatusUnsupportedMediaType, false, nil)
	}
	contentType := request.Header.Get("Content-Type")
	if contentType != "" && !strings.HasPrefix(strings.ToLower(contentType), "application/json") {
		return nil, gatewayError(contract.ErrorInvalidRequest, "content type must be application/json", http.StatusUnsupportedMediaType, false, nil)
	}
	request.Body = http.MaxBytesReader(response, request.Body, maximum)
	body, err := io.ReadAll(request.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return nil, gatewayError(contract.ErrorInvalidRequest, "request body is too large", http.StatusRequestEntityTooLarge, false, err)
		}
		return nil, gatewayError(contract.ErrorInvalidRequest, "failed to read request body", http.StatusBadRequest, false, err)
	}
	if len(body) == 0 {
		return nil, gatewayError(contract.ErrorInvalidRequest, "request body is required", http.StatusBadRequest, false, nil)
	}
	if err := rejectDuplicateJSONKeys(body); err != nil {
		return nil, gatewayError(contract.ErrorInvalidRequest, "request contains invalid or duplicate JSON fields", http.StatusBadRequest, false, err)
	}
	return body, nil
}

func decodeStrict(data []byte, destination any) error {
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("request must contain one JSON document")
	}
	return nil
}

func (h *Handler) requestID(request *http.Request) (contract.ID, error) {
	provided := strings.TrimSpace(request.Header.Get("X-Request-ID"))
	if provided != "" {
		if !validRequestID(provided) {
			return "", gatewayError(contract.ErrorInvalidRequest, "invalid X-Request-ID", http.StatusBadRequest, false, nil)
		}
		return contract.ID(provided), nil
	}
	return h.newID()
}

func randomRequestID() (contract.ID, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("generate request ID: %w", err)
	}
	return contract.ID("req_" + hex.EncodeToString(value[:])), nil
}

func validRequestID(value string) bool {
	if len(value) < 1 || len(value) > 128 {
		return false
	}
	for _, character := range value {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') || character == '-' || character == '_' || character == '.' {
			continue
		}
		return false
	}
	return true
}

func gatewayError(code contract.ErrorCode, message string, status int, retryable bool, cause error) *contract.Error {
	return &contract.Error{Code: code, Message: message, HTTPStatus: status, Retryable: retryable, Cause: cause}
}

func writeError(response http.ResponseWriter, err error, id contract.ID) {
	public := gatewayError(contract.ErrorInternal, "internal server error", http.StatusInternalServerError, false, err)
	var typed *contract.Error
	if errors.As(err, &typed) {
		public = typed.Safe()
	}
	if public.RetryAfter > 0 {
		seconds := int64((public.RetryAfter + time.Second - 1) / time.Second)
		response.Header().Set("Retry-After", strconv.FormatInt(seconds, 10))
	}
	response.Header().Set("Content-Type", "application/json")
	response.Header().Set("X-Request-ID", string(id))
	response.WriteHeader(public.HTTPStatus)
	_ = json.NewEncoder(response).Encode(map[string]any{"error": map[string]any{
		"message": public.Message, "type": string(public.Code), "code": string(public.Code), "request_id": id,
	}})
}

func writeJSON(response http.ResponseWriter, status int, id contract.ID, value any) {
	response.Header().Set("Content-Type", "application/json")
	response.Header().Set("X-Request-ID", string(id))
	response.WriteHeader(status)
	_ = json.NewEncoder(response).Encode(value)
}

func (h *Handler) notFound(response http.ResponseWriter, request *http.Request) {
	id, err := h.requestID(request)
	if err != nil {
		writeError(response, err, "")
		return
	}
	writeError(response, gatewayError(contract.ErrorInvalidRequest, "route not found", http.StatusNotFound, false, nil), id)
}
