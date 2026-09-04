package openai

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/daptin/llmgateway/catalog"
	"github.com/daptin/llmgateway/contract"
	"github.com/daptin/llmgateway/internal/jsonx"
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
	MaxBodyBytes        int64
	TotalRequestTimeout time.Duration
	StreamKeepalive     time.Duration
	NewRequestID        func() (contract.ID, error)
	Files               FileStore
	Batches             BatchStore
}

type Handler struct {
	engine    Engine
	auth      Authenticator
	maxBody   int64
	timeout   time.Duration
	keepalive time.Duration
	newID     func() (contract.ID, error)
	files     FileStore
	batches   BatchStore
	mux       *http.ServeMux
}

func NewHandler(engine Engine, authenticator Authenticator, options Options) (*Handler, error) {
	if engine == nil || authenticator == nil {
		return nil, errors.New("engine and authenticator are required")
	}
	if options.MaxBodyBytes == 0 {
		options.MaxBodyBytes = 16 << 20
	}
	if options.TotalRequestTimeout == 0 {
		options.TotalRequestTimeout = 2 * time.Minute
	}
	if options.StreamKeepalive == 0 {
		options.StreamKeepalive = 15 * time.Second
	}
	if options.MaxBodyBytes < 1 || options.TotalRequestTimeout < 0 || options.StreamKeepalive < 0 {
		return nil, errors.New("handler bounds must be positive")
	}
	if options.NewRequestID == nil {
		options.NewRequestID = randomRequestID
	}
	handler := &Handler{engine: engine, auth: authenticator, maxBody: options.MaxBodyBytes, timeout: options.TotalRequestTimeout,
		keepalive: options.StreamKeepalive, newID: options.NewRequestID, files: options.Files, batches: options.Batches, mux: http.NewServeMux()}
	handler.mux.HandleFunc("POST /v1/chat/completions", handler.chatCompletions)
	handler.mux.HandleFunc("POST /v1/messages", handler.messages)
	handler.mux.HandleFunc("POST /v1/completions", handler.completions)
	handler.mux.HandleFunc("POST /v1/responses", handler.responses)
	handler.mux.HandleFunc("POST /v1/responses/compact", handler.compactResponses)
	handler.mux.HandleFunc("POST /v1/embeddings", handler.embeddings)
	handler.mux.HandleFunc("POST /v1/images/generations", handler.imageGenerations)
	handler.mux.HandleFunc("POST /v1/images/edits", handler.imageEdits)
	handler.mux.HandleFunc("POST /v1/images/variations", handler.imageVariations)
	handler.mux.HandleFunc("POST /v1/moderations", handler.moderations)
	handler.mux.HandleFunc("POST /v1/rerank", handler.rerank)
	handler.mux.HandleFunc("POST /v2/rerank", handler.rerank)
	handler.mux.HandleFunc("POST /rerank", handler.rerank)
	handler.mux.HandleFunc("POST /v1/audio/speech", handler.audioSpeech)
	handler.mux.HandleFunc("POST /v1/audio/transcriptions", handler.audioTranscription)
	handler.mux.HandleFunc("POST /v1/audio/translations", handler.audioTranslation)
	handler.mux.HandleFunc("POST /v1/search", handler.search)
	handler.mux.HandleFunc("POST /v1/search/{tool}", handler.search)
	handler.mux.HandleFunc("POST /v1/ocr", handler.ocr)
	handler.mux.HandleFunc("POST /ocr", handler.ocr)
	handler.mux.HandleFunc("GET /v1/models", handler.models)
	handler.mux.HandleFunc("GET /v1/models/{id}", handler.model)
	if handler.files != nil {
		handler.mux.HandleFunc("POST /v1/files", handler.createFile)
		handler.mux.HandleFunc("GET /v1/files", handler.listFiles)
		handler.mux.HandleFunc("GET /v1/files/{id}", handler.getFile)
		handler.mux.HandleFunc("GET /v1/files/{id}/content", handler.getFileContent)
		handler.mux.HandleFunc("DELETE /v1/files/{id}", handler.deleteFile)
	}
	if handler.batches != nil {
		handler.mux.HandleFunc("POST /v1/batches", handler.createBatch)
		handler.mux.HandleFunc("GET /v1/batches", handler.listBatches)
		handler.mux.HandleFunc("GET /v1/batches/{id}", handler.getBatch)
		handler.mux.HandleFunc("POST /v1/batches/{id}/cancel", handler.cancelBatch)
	}
	handler.mux.HandleFunc("/", handler.notFound)
	return handler, nil
}

type asyncResult[T any] struct {
	value T
	err   error
}

func awaitWithKeepalive[T any](ctx context.Context, interval time.Duration, operation func() (T, error), keepalive func() error, abandon func(T)) (T, error) {
	result := make(chan asyncResult[T])
	go func() {
		value, err := operation()
		select {
		case result <- asyncResult[T]{value: value, err: err}:
		case <-ctx.Done():
			if err == nil && abandon != nil {
				abandon(value)
			}
		}
	}()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case completed := <-result:
			return completed.value, completed.err
		case <-ticker.C:
			if err := keepalive(); err != nil {
				var zero T
				return zero, err
			}
		case <-ctx.Done():
			var zero T
			return zero, ctx.Err()
		}
	}
}

func streamEventResults(ctx context.Context, stream contract.EventStream) <-chan asyncResult[contract.StreamEvent] {
	results := make(chan asyncResult[contract.StreamEvent])
	go func() {
		defer close(results)
		for {
			event, err := stream.Next(ctx)
			select {
			case results <- asyncResult[contract.StreamEvent]{value: event, err: err}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()
	return results
}

func receiveWithKeepalive[T any](ctx context.Context, results <-chan asyncResult[T], interval time.Duration, keepalive func() error) (T, error) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case completed, ok := <-results:
			if !ok {
				var zero T
				return zero, io.EOF
			}
			return completed.value, completed.err
		case <-ticker.C:
			if err := keepalive(); err != nil {
				var zero T
				return zero, err
			}
		case <-ctx.Done():
			var zero T
			return zero, ctx.Err()
		}
	}
}

func beginSSE(response http.ResponseWriter, id contract.ID) (http.Flusher, error) {
	flusher, ok := response.(http.Flusher)
	if !ok {
		return nil, gatewayError(contract.ErrorInternal, "streaming is unavailable", http.StatusInternalServerError, false, nil)
	}
	response.Header().Set("Content-Type", "text/event-stream")
	response.Header().Set("Cache-Control", "no-cache")
	response.Header().Set("X-Accel-Buffering", "no")
	response.Header().Set("X-Request-ID", string(id))
	response.WriteHeader(http.StatusOK)
	flusher.Flush()
	return flusher, nil
}

func writeSSEKeepalive(response io.Writer, flusher http.Flusher) error {
	if _, err := io.WriteString(response, ": keep-alive\n\n"); err != nil {
		return err
	}
	flusher.Flush()
	return nil
}

func closeAbandonedStream(stream contract.EventStream) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = stream.Close(ctx)
}

type sseSession struct {
	ctx       context.Context
	cancel    context.CancelFunc
	stream    contract.EventStream
	events    <-chan asyncResult[contract.StreamEvent]
	response  http.ResponseWriter
	flusher   http.Flusher
	keepalive time.Duration
}

func (h *Handler) startSSE(response http.ResponseWriter, request *http.Request, principal contract.Principal, canonical contract.Request) (*sseSession, error) {
	flusher, err := beginSSE(response, canonical.ID)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(request.Context())
	session := &sseSession{ctx: ctx, cancel: cancel, response: response, flusher: flusher, keepalive: h.keepalive}
	session.stream, err = awaitWithKeepalive(ctx, h.keepalive,
		func() (contract.EventStream, error) { return h.engine.Stream(ctx, principal, canonical) }, session.writeKeepalive, closeAbandonedStream)
	if err != nil {
		return session, err
	}
	session.events = streamEventResults(ctx, session.stream)
	return session, nil
}

func (s *sseSession) writeKeepalive() error {
	return writeSSEKeepalive(s.response, s.flusher)
}

func (s *sseSession) Next() (contract.StreamEvent, error) {
	return receiveWithKeepalive(s.ctx, s.events, s.keepalive, s.writeKeepalive)
}

func (s *sseSession) Close() {
	s.cancel()
	if s.stream != nil {
		closeAbandonedStream(s.stream)
	}
}

func (s *sseSession) Active() bool { return s.ctx.Err() == nil }

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
	apiKey := strings.TrimSpace(request.Header.Get("X-API-Key"))
	parts := strings.SplitN(header, " ", 2)
	bearerValid := len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") && strings.TrimSpace(parts[1]) != ""
	if header != "" && !bearerValid {
		return contract.Principal{}, gatewayError(contract.ErrorAuthentication, "invalid authorization header", http.StatusUnauthorized, false, nil)
	}
	if apiKey != "" && bearerValid {
		return contract.Principal{}, gatewayError(contract.ErrorAuthentication, "provide exactly one API credential", http.StatusUnauthorized, false, nil)
	}
	token := apiKey
	if bearerValid {
		token = strings.TrimSpace(parts[1])
	}
	if token == "" {
		return contract.Principal{}, gatewayError(contract.ErrorAuthentication, "missing or invalid API credential", http.StatusUnauthorized, false, nil)
	}
	principal, err := h.auth.Authenticate(request.Context(), token)
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
	if encoding := strings.TrimSpace(request.Header.Get("Content-Encoding")); encoding != "" && !strings.EqualFold(encoding, "identity") {
		return nil, gatewayError(contract.ErrorInvalidRequest, "compressed request bodies are not supported", http.StatusUnsupportedMediaType, false, nil)
	}
	contentType := request.Header.Get("Content-Type")
	if contentType != "" {
		mediaType, _, err := mime.ParseMediaType(contentType)
		if err != nil || !strings.EqualFold(mediaType, "application/json") {
			return nil, gatewayError(contract.ErrorInvalidRequest, "content type must be application/json", http.StatusUnsupportedMediaType, false, err)
		}
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
	return jsonx.DecodeOne(bytes.NewReader(data), destination)
}

func strictJSONErrorMessage(message string, err error) string {
	const unknownFieldPrefix = "json: unknown field \""
	if err != nil && strings.HasPrefix(err.Error(), unknownFieldPrefix) && strings.HasSuffix(err.Error(), "\"") {
		field := strings.TrimSuffix(strings.TrimPrefix(err.Error(), unknownFieldPrefix), "\"")
		return fmt.Sprintf("%s: unsupported field %q", message, field)
	}
	var typeError *json.UnmarshalTypeError
	if errors.As(err, &typeError) && typeError.Field != "" {
		return fmt.Sprintf("%s: field %q has an invalid type", message, typeError.Field)
	}
	return message
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

func (h *Handler) authenticatedRequest(response http.ResponseWriter, request *http.Request) (contract.ID, contract.Principal, bool) {
	id, err := h.requestID(request)
	if err != nil {
		writeError(response, err, "")
		return "", contract.Principal{}, false
	}
	principal, err := h.authenticate(request)
	if err != nil {
		writeError(response, err, id)
		return id, contract.Principal{}, false
	}
	return id, principal, true
}

func (h *Handler) identifiedRequest(response http.ResponseWriter, request *http.Request) (contract.ID, contract.Principal, contract.ID, bool) {
	id, principal, ok := h.authenticatedRequest(response, request)
	resourceID := contract.ID(strings.TrimSpace(request.PathValue("id")))
	if ok && (resourceID == "" || len(resourceID) > 200) {
		writeError(response, gatewayError(contract.ErrorInvalidRequest, "invalid resource ID", http.StatusBadRequest, false, nil), id)
		ok = false
	}
	return id, principal, resourceID, ok
}

func rejectUnknownQuery(values url.Values, allowed ...string) error {
	set := make(map[string]bool, len(allowed))
	for _, name := range allowed {
		set[name] = true
	}
	for name, entries := range values {
		if !set[name] || len(entries) != 1 {
			return gatewayError(contract.ErrorInvalidRequest, "unsupported or duplicate query parameter", http.StatusBadRequest, false, nil)
		}
	}
	return nil
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
	public := publicError(err)
	if public.Retryable && public.RetryAfter > 0 {
		seconds := int64(public.RetryAfter / time.Second)
		if public.RetryAfter%time.Second != 0 {
			seconds++
		}
		response.Header().Set("Retry-After", strconv.FormatInt(seconds, 10))
	}
	response.Header().Set("Content-Type", "application/json")
	response.Header().Set("X-Request-ID", string(id))
	response.WriteHeader(public.HTTPStatus)
	_ = json.NewEncoder(response).Encode(map[string]any{"error": map[string]any{
		"message": public.Message, "type": string(public.Code), "code": string(public.Code), "request_id": id,
	}})
}

func publicError(err error) *contract.Error {
	public := gatewayError(contract.ErrorInternal, "internal server error", http.StatusInternalServerError, false, err)
	var typed *contract.Error
	if errors.As(err, &typed) {
		public = typed.Safe()
	}
	if public == nil || !public.Code.Valid() || public.Message == "" || public.HTTPStatus < 400 || public.HTTPStatus > 599 {
		public = gatewayError(contract.ErrorInternal, "internal server error", http.StatusInternalServerError, false, nil)
	}
	return public
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
