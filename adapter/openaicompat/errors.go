package openaicompat

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/daptin/llmgateway/contract"
)

type errorEnvelope struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    any    `json:"code"`
	} `json:"error"`
}

func (a *Adapter) providerError(response *http.Response) error {
	payload, readErr := readBounded(response.Body, min(a.maxResponseBytes, 64<<10))
	var envelope errorEnvelope
	_ = json.Unmarshal(payload, &envelope)
	code := contract.ErrorProvider
	message := "upstream provider failed"
	retryable := response.StatusCode == http.StatusRequestTimeout || response.StatusCode == http.StatusConflict || response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= 500
	switch response.StatusCode {
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		code = contract.ErrorInvalidRequest
		message = "upstream provider rejected the request"
	case http.StatusUnauthorized:
		code = contract.ErrorAuthentication
		message = "upstream provider authentication failed"
	case http.StatusForbidden:
		code = contract.ErrorPermission
		message = "upstream provider denied the request"
	case http.StatusPaymentRequired:
		code = contract.ErrorInsufficientQuota
		message = "upstream provider quota is exhausted"
	case http.StatusNotFound:
		code = contract.ErrorModelNotFound
		message = "upstream model not found"
	case http.StatusTooManyRequests:
		code = contract.ErrorRateLimit
		message = "upstream provider rate limited the request"
	case http.StatusRequestTimeout, http.StatusGatewayTimeout:
		code = contract.ErrorTimeout
		message = "upstream provider timed out"
	}
	cause := readErr
	if envelope.Error.Message != "" {
		cause = errors.New(envelope.Error.Message)
	}
	return &contract.Error{Code: code, Message: message, HTTPStatus: response.StatusCode, Retryable: retryable, RetryAfter: retryAfter(response.Header.Get("Retry-After"), a.now()), Cause: cause}
}

func retryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil {
		if seconds <= 0 {
			return 0
		}
		if seconds > int64((1<<63-1)/time.Second) {
			return time.Duration(1<<63 - 1)
		}
		return time.Duration(seconds) * time.Second
	}
	when, err := http.ParseTime(value)
	if err != nil || !when.After(now) {
		return 0
	}
	return when.Sub(now)
}

func invalidRequest(message string, cause error) *contract.Error {
	return &contract.Error{Code: contract.ErrorInvalidRequest, Message: message, HTTPStatus: http.StatusBadRequest, Cause: cause}
}

func providerFailure(message string, cause error) *contract.Error {
	if errors.Is(cause, io.EOF) {
		cause = errors.New("upstream response ended unexpectedly")
	}
	return &contract.Error{Code: contract.ErrorProvider, Message: message, HTTPStatus: http.StatusBadGateway, Retryable: true, Cause: cause}
}
