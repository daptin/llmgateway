package contract

import "testing"

func TestOCRRequestValidatesDocumentLocationAndControls(t *testing.T) {
	base := Request{ID: "request", PublicModel: "ocr", Operation: OperationOCR, OCR: &OCRRequest{
		Document: OCRDocument{Type: "document_url", URL: "https://example.test/document.pdf"}, Pages: []int{0, 2}, TableFormat: "markdown",
	}}
	if err := base.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*Request){
		"provider file id": func(request *Request) { request.OCR.Document.URL = "reducto://foreign" },
		"filesystem":       func(request *Request) { request.OCR.Document = OCRDocument{Type: "file", URL: "/etc/passwd"} },
		"duplicate page":   func(request *Request) { request.OCR.Pages = []int{1, 1} },
		"invalid schema":   func(request *Request) { request.OCR.BBoxAnnotationFormat = []byte(`[]`) },
	} {
		t.Run(name, func(t *testing.T) {
			request := base
			copy := *base.OCR
			request.OCR = &copy
			mutate(&request)
			if err := request.Validate(); err == nil {
				t.Fatalf("invalid OCR request accepted: %#v", request.OCR)
			}
		})
	}
}

func TestOCRRequestAcceptsInlineBase64Document(t *testing.T) {
	request := Request{ID: "request", PublicModel: "ocr", Operation: OperationOCR, OCR: &OCRRequest{
		Document: OCRDocument{Type: "document_url", URL: "data:application/pdf;base64,cGRm"},
	}}
	if err := request.Validate(); err != nil {
		t.Fatal(err)
	}
}
