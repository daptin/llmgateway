package openai

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/daptin/llmgateway/contract"
)

func TestOCRProtocolPreservesTypedControlsAndNormalizesResponse(t *testing.T) {
	engine := &fakeEngine{snapshot: testSnapshot(t), invokeResult: contract.Response{OCR: &contract.OCRResponse{
		Model: "actual-ocr", Pages: []contract.OCRPage{{Index: 0, Markdown: "Invoice", Images: []contract.OCRPageImage{{ImageBase64: "aW1hZ2U=", BBox: []byte(`{"x":1}`)}},
			Dimensions: &contract.OCRPageDimensions{DPI: 300, Height: 100, Width: 80}}}, DocumentAnnotation: []byte(`{"kind":"invoice"}`),
		UsageInfo: &contract.OCRUsageInfo{PagesProcessed: 1, Credits: 0.25, DocSizeBytes: 12}, Content: "Invoice",
		Tables: []json.RawMessage{[]byte(`{"rows":1}`)}, KeyValuePairs: []json.RawMessage{[]byte(`{"key":"total"}`)},
	}}}
	body := `{"model":"allowed","document":{"type":"document_url","document_url":"https://example.test/invoice.pdf"},"pages":[0],"include_image_base64":true,"image_limit":2,"image_min_size":64,"bbox_annotation_format":{"type":"json_schema"},"document_annotation_format":{"type":"json_schema"},"document_annotation_prompt":"extract invoice fields","extract_header":false,"extract_footer":true,"table_format":"markdown","confidence_scores_granularity":"word","include_blocks":true,"id":"ocr_1","req_format":"litellm"}`
	response := perform(testHandler(t, engine, fakeAuthenticator{}), http.MethodPost, "/v1/ocr", body, "key")
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	request := engine.invokeRequest
	if request.Operation != contract.OperationOCR || request.OCR == nil || request.OCR.Document.URL != "https://example.test/invoice.pdf" ||
		request.OCR.IncludeImageBase64 == nil || !*request.OCR.IncludeImageBase64 || request.OCR.ExtractHeader == nil || *request.OCR.ExtractHeader ||
		request.OCR.IncludeBlocks == nil || !*request.OCR.IncludeBlocks || request.OCR.DocumentAnnotationPrompt != "extract invoice fields" {
		t.Fatalf("canonical OCR request=%#v", request)
	}
	if request.EstimatedUsage.TotalTokens != 0 || request.EstimatedUsage.Measures["request_bytes"] == 0 {
		t.Fatalf("OCR usage estimate=%#v", request.EstimatedUsage)
	}
	assertJSONEqual(t, response.Body.String(), `{"object":"ocr","pages":[{"index":0,"markdown":"Invoice","images":[{"image_base64":"aW1hZ2U=","bbox":{"x":1}}],"dimensions":{"dpi":300,"height":100,"width":80}}],"model":"actual-ocr","document_annotation":{"kind":"invoice"},"usage_info":{"pages_processed":1,"credits":0.25,"doc_size_bytes":12},"content":"Invoice","tables":[{"rows":1}],"keyValuePairs":[{"key":"total"}]}`)
}

func TestOCRProtocolConvertsBoundedMultipartUpload(t *testing.T) {
	engine := &fakeEngine{snapshot: testSnapshot(t), invokeResult: contract.Response{OCR: &contract.OCRResponse{Model: "ocr", Pages: []contract.OCRPage{}}}}
	request := multipartFilesRequest(t, "/ocr", map[string][]string{"model": {"allowed"}, "pages": {"[0,1]"}, "include_image_base64": {"true"}},
		map[string][]contract.MediaFile{"file": {{Name: "brief.pdf", Data: []byte("pdf")}}})
	response := httptest.NewRecorder()
	testHandler(t, engine, fakeAuthenticator{}).ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	ocr := engine.invokeRequest.OCR
	if ocr == nil || ocr.Document.Type != "document_url" || !strings.HasPrefix(ocr.Document.URL, "data:application/pdf;base64,") || len(ocr.Pages) != 2 || ocr.IncludeImageBase64 == nil || !*ocr.IncludeImageBase64 {
		t.Fatalf("multipart canonical OCR request=%#v", engine.invokeRequest)
	}
	if engine.invokeRequest.EstimatedUsage.Measures["document_bytes"] != 3 || engine.invokeRequest.EstimatedUsage.TotalTokens != 0 {
		t.Fatalf("multipart OCR usage=%#v", engine.invokeRequest.EstimatedUsage)
	}
}

func TestOCRProtocolRejectsUnsafeAndAmbiguousInputs(t *testing.T) {
	handler := testHandler(t, &fakeEngine{snapshot: testSnapshot(t)}, fakeAuthenticator{})
	for _, body := range []string{
		`{"model":"allowed","document":{"type":"file","document_url":"/etc/passwd"}}`,
		`{"model":"allowed","document":{"type":"document_url","document_url":"reducto://foreign"}}`,
		`{"model":"allowed","document":{"type":"document_url","document_url":"https://example.test/a","image_url":"https://example.test/b"}}`,
		`{"model":"allowed","document":{"type":"document_url","document_url":"data:application/pdf;base64,%%%"}}`,
		`{"model":"allowed","document":{"type":"document_url","document_url":"https://example.test/a"},"req_format":"native"}`,
	} {
		response := perform(handler, http.MethodPost, "/v1/ocr", body, "key")
		if response.Code != http.StatusBadRequest {
			t.Fatalf("invalid OCR input accepted: status=%d body=%s request=%s", response.Code, response.Body.String(), body)
		}
	}
}
