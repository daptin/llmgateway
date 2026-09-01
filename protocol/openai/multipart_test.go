package openai

import (
	"bytes"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/daptin/llmgateway/contract"
)

func multipartRequest(t *testing.T, path string, fields map[string][]string, filename string, file []byte) *http.Request {
	return multipartFilesRequest(t, path, fields, map[string][]contract.MediaFile{"file": {{Name: filename, Data: file}}})
}

func multipartFilesRequest(t *testing.T, path string, fields map[string][]string, files map[string][]contract.MediaFile) *http.Request {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	var err error
	for name, entries := range files {
		for _, entry := range entries {
			var part io.Writer
			if err == nil {
				part, err = writer.CreateFormFile(name, entry.Name)
			}
			if err == nil {
				_, err = part.Write(entry.Data)
			}
		}
	}
	for name, values := range fields {
		for _, value := range values {
			if err == nil {
				err = writer.WriteField(name, value)
			}
		}
	}
	if err == nil {
		err = writer.Close()
	}
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, path, &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	request.Header.Set("Authorization", "Bearer key")
	request.Header.Set("X-Request-ID", "req_test")
	return request
}
