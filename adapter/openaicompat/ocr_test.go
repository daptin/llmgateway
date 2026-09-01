package openaicompat

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/daptin/llmgateway/contract"
)

func TestInvokeOCRTranslatesCanonicalRequestResponseAndUsage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/ocr" {
			t.Fatalf("path=%s", request.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		document := body["document"].(map[string]any)
		if body["model"] != "upstream-model" || document["type"] != "document_url" || document["document_url"] != "https://example.test/a.pdf" ||
			body["include_image_base64"] != true || body["table_format"] != "html" || body["vendor_flag"] != "enabled" {
			t.Fatalf("OCR body=%#v", body)
		}
		response.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(response, `{"pages":[{"index":0,"markdown":"Page","images":[],"dimensions":{"dpi":300,"height":100,"width":80}}],"model":"ocr-model","document_annotation":{"type":"invoice"},"usage_info":{"pages_processed":1,"credits":0.25,"doc_size_bytes":100},"tables":[{"rows":1}],"keyValuePairs":[{"key":"total"}]}`)
	}))
	defer server.Close()
	include := true
	adapter := buildAdapter(t, server, `{}`, Factory{})
	result, err := adapter.Invoke(context.Background(), deploymentWithParameters(`{"ocr":{"vendor_flag":"enabled"}}`), contract.Request{Operation: contract.OperationOCR,
		OCR: &contract.OCRRequest{Document: contract.OCRDocument{Type: "document_url", URL: "https://example.test/a.pdf"}, IncludeImageBase64: &include, TableFormat: "html"}})
	if err != nil || result.OCR == nil || len(result.OCR.Pages) != 1 || result.OCR.Pages[0].Markdown != "Page" ||
		result.Usage.Measures["ocr_pages"] != 1 || result.Usage.Measures["document_bytes"] != 100 || result.Usage.Measures["ocr_credit_micros"] != 250000 {
		t.Fatalf("OCR result=%#v err=%v", result, err)
	}
}

func TestInvokeOCRRejectsMalformedProviderResponse(t *testing.T) {
	for _, payload := range []string{
		`{"model":"ocr","pages":[{"index":-1,"markdown":"bad"}]}`,
		`{"model":"ocr","pages":[],"tables":["not-an-object"]}`,
		`{"model":"ocr","pages":[],"usage_info":{"pages_processed":-1}}`,
	} {
		server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(response, payload) }))
		adapter := buildAdapter(t, server, `{}`, Factory{})
		_, err := adapter.Invoke(context.Background(), deployment(), contract.Request{Operation: contract.OperationOCR,
			OCR: &contract.OCRRequest{Document: contract.OCRDocument{Type: "document_url", URL: "https://example.test/a.pdf"}}})
		server.Close()
		if err == nil {
			t.Fatalf("malformed OCR response accepted: %s", payload)
		}
	}
}
