package openai

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/daptin/llmgateway/contract"
)

type multipartValue struct {
	data        []byte
	filename    string
	contentType string
}

func oneMultipartValue(values map[string][]multipartValue, name string) (multipartValue, bool) {
	entries := values[name]
	if len(entries) != 1 {
		return multipartValue{}, false
	}
	return entries[0], true
}

func multipartText(values map[string][]multipartValue, name string) string {
	value, ok := oneMultipartValue(values, name)
	if !ok {
		return ""
	}
	return string(value.data)
}

func readMultipartForm(response http.ResponseWriter, request *http.Request, maximum int64) (map[string][]multipartValue, error) {
	if encoding := strings.TrimSpace(request.Header.Get("Content-Encoding")); encoding != "" && !strings.EqualFold(encoding, "identity") {
		return nil, gatewayError(contract.ErrorInvalidRequest, "compressed request bodies are not supported", http.StatusUnsupportedMediaType, false, nil)
	}
	request.Body = http.MaxBytesReader(response, request.Body, maximum)
	reader, err := request.MultipartReader()
	if err != nil {
		return nil, gatewayError(contract.ErrorInvalidRequest, "content type must be multipart/form-data", http.StatusUnsupportedMediaType, false, err)
	}
	values := make(map[string][]multipartValue)
	parts := 0
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				return nil, gatewayError(contract.ErrorInvalidRequest, "request body is too large", http.StatusRequestEntityTooLarge, false, err)
			}
			return nil, gatewayError(contract.ErrorInvalidRequest, "invalid multipart request", http.StatusBadRequest, false, err)
		}
		parts++
		if parts > 32 {
			part.Close()
			return nil, gatewayError(contract.ErrorInvalidRequest, "multipart request has too many fields", http.StatusBadRequest, false, nil)
		}
		name := part.FormName()
		if name == "" {
			part.Close()
			return nil, gatewayError(contract.ErrorInvalidRequest, "multipart field name is required", http.StatusBadRequest, false, nil)
		}
		data, readErr := io.ReadAll(part)
		closeErr := part.Close()
		if readErr != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(readErr, &tooLarge) {
				return nil, gatewayError(contract.ErrorInvalidRequest, "request body is too large", http.StatusRequestEntityTooLarge, false, readErr)
			}
			return nil, gatewayError(contract.ErrorInvalidRequest, "failed to read multipart field", http.StatusBadRequest, false, readErr)
		}
		if closeErr != nil {
			return nil, gatewayError(contract.ErrorInvalidRequest, "failed to close multipart field", http.StatusBadRequest, false, closeErr)
		}
		filename := part.FileName()
		if filename == "" && !utf8.Valid(data) {
			return nil, gatewayError(contract.ErrorInvalidRequest, "multipart text field is not UTF-8", http.StatusBadRequest, false, nil)
		}
		values[name] = append(values[name], multipartValue{data: data, filename: filename, contentType: part.Header.Get("Content-Type")})
	}
	return values, nil
}
