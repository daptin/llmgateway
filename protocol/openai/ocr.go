package openai

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"mime"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/daptin/llmgateway/contract"
)

type ocrDocumentWire struct {
	Type        string `json:"type"`
	DocumentURL string `json:"document_url,omitempty"`
	ImageURL    string `json:"image_url,omitempty"`
}

type ocrRequest struct {
	Model                       string           `json:"model"`
	Document                    *ocrDocumentWire `json:"document"`
	Pages                       []int            `json:"pages,omitempty"`
	IncludeImageBase64          *bool            `json:"include_image_base64,omitempty"`
	ImageLimit                  *int             `json:"image_limit,omitempty"`
	ImageMinSize                *int             `json:"image_min_size,omitempty"`
	BBoxAnnotationFormat        json.RawMessage  `json:"bbox_annotation_format,omitempty"`
	DocumentAnnotationFormat    json.RawMessage  `json:"document_annotation_format,omitempty"`
	DocumentAnnotationPrompt    string           `json:"document_annotation_prompt,omitempty"`
	ExtractHeader               *bool            `json:"extract_header,omitempty"`
	ExtractFooter               *bool            `json:"extract_footer,omitempty"`
	TableFormat                 string           `json:"table_format,omitempty"`
	ConfidenceScoresGranularity string           `json:"confidence_scores_granularity,omitempty"`
	IncludeBlocks               *bool            `json:"include_blocks,omitempty"`
	ID                          string           `json:"id,omitempty"`
	RequestFormat               string           `json:"req_format,omitempty"`
}

func (h *Handler) ocr(response http.ResponseWriter, request *http.Request) {
	id, err := h.requestID(request)
	if err != nil {
		writeError(response, err, "")
		return
	}
	principal, err := h.authenticate(request)
	if err != nil {
		writeError(response, err, id)
		return
	}
	wire, measures, err := h.decodeOCRRequest(response, request)
	if err != nil {
		writeError(response, err, id)
		return
	}
	requestFormat := strings.ToLower(strings.TrimSpace(wire.RequestFormat))
	if requestFormat == "" {
		requestFormat = strings.ToLower(strings.TrimSpace(request.Header.Get("X-Req-Format")))
	}
	if requestFormat != "" && requestFormat != "litellm" {
		writeError(response, gatewayError(contract.ErrorInvalidRequest, "native OCR responses are not supported by the provider-neutral gateway", http.StatusBadRequest, false, nil), id)
		return
	}
	canonical := canonicalOCR(id, wire, measures)
	if err := canonical.Validate(); err != nil {
		writeError(response, gatewayError(contract.ErrorInvalidRequest, "invalid OCR request", http.StatusBadRequest, false, err), id)
		return
	}
	result, err := h.engine.Invoke(request.Context(), principal, canonical)
	if err != nil {
		writeError(response, err, id)
		return
	}
	encoded, err := encodeOCRResponse(result)
	if err != nil {
		writeError(response, err, id)
		return
	}
	writeJSON(response, http.StatusOK, id, encoded)
}

func (h *Handler) decodeOCRRequest(response http.ResponseWriter, request *http.Request) (ocrRequest, map[string]int64, error) {
	if !strings.HasPrefix(strings.ToLower(request.Header.Get("Content-Type")), "multipart/form-data;") {
		body, err := readJSONBody(response, request, h.maxBody)
		if err != nil {
			return ocrRequest{}, nil, err
		}
		var wire ocrRequest
		if err := decodeStrict(body, &wire); err != nil {
			return ocrRequest{}, nil, gatewayError(contract.ErrorInvalidRequest, "invalid OCR request", http.StatusBadRequest, false, err)
		}
		return wire, map[string]int64{"request_bytes": int64(len(body))}, nil
	}
	values, err := readMultipartForm(response, request, h.maxBody)
	if err != nil {
		return ocrRequest{}, nil, err
	}
	allowed := map[string]bool{"file": true, "model": true, "pages": true, "include_image_base64": true, "image_limit": true,
		"image_min_size": true, "bbox_annotation_format": true, "document_annotation_format": true, "document_annotation_prompt": true,
		"extract_header": true, "extract_footer": true, "table_format": true, "confidence_scores_granularity": true,
		"include_blocks": true, "id": true, "req_format": true}
	for name, entries := range values {
		if !allowed[name] || len(entries) != 1 {
			return ocrRequest{}, nil, gatewayError(contract.ErrorInvalidRequest, "unsupported or duplicate OCR multipart field", http.StatusBadRequest, false, nil)
		}
	}
	file, ok := oneMultipartValue(values, "file")
	if !ok || file.filename == "" || len(file.data) == 0 {
		return ocrRequest{}, nil, gatewayError(contract.ErrorInvalidRequest, "OCR multipart request requires a file", http.StatusBadRequest, false, nil)
	}
	document, err := uploadedOCRDocument(file)
	if err != nil {
		return ocrRequest{}, nil, gatewayError(contract.ErrorInvalidRequest, "invalid OCR file", http.StatusBadRequest, false, err)
	}
	object := map[string]any{"document": document}
	for name, entries := range values {
		if name == "file" {
			continue
		}
		text := string(entries[0].data)
		var value any
		if json.Unmarshal([]byte(text), &value) != nil {
			value = text
		}
		object[name] = value
	}
	payload, err := json.Marshal(object)
	if err != nil {
		return ocrRequest{}, nil, gatewayError(contract.ErrorInvalidRequest, "invalid OCR multipart fields", http.StatusBadRequest, false, err)
	}
	var wire ocrRequest
	if err := decodeStrict(payload, &wire); err != nil {
		return ocrRequest{}, nil, gatewayError(contract.ErrorInvalidRequest, "invalid OCR multipart fields", http.StatusBadRequest, false, err)
	}
	return wire, map[string]int64{"document_bytes": int64(len(file.data))}, nil
}

func uploadedOCRDocument(file multipartValue) (ocrDocumentWire, error) {
	mediaType := strings.TrimSpace(file.contentType)
	if mediaType != "" {
		parsed, _, err := mime.ParseMediaType(mediaType)
		if err != nil {
			return ocrDocumentWire{}, err
		}
		mediaType = parsed
	}
	if mediaType == "" || mediaType == "application/octet-stream" {
		mediaType = mime.TypeByExtension(strings.ToLower(filepath.Ext(file.filename)))
	}
	if mediaType == "" {
		mediaType = http.DetectContentType(file.data)
	}
	parsed, _, err := mime.ParseMediaType(mediaType)
	if err != nil {
		return ocrDocumentWire{}, err
	}
	mediaType = parsed
	if strings.ContainsAny(mediaType, "\r\n,;") {
		return ocrDocumentWire{}, errors.New("file media type is invalid")
	}
	encoded := "data:" + mediaType + ";base64," + base64.StdEncoding.EncodeToString(file.data)
	if strings.HasPrefix(mediaType, "image/") {
		return ocrDocumentWire{Type: "image_url", ImageURL: encoded}, nil
	}
	return ocrDocumentWire{Type: "document_url", DocumentURL: encoded}, nil
}

func canonicalOCR(id contract.ID, wire ocrRequest, measures map[string]int64) contract.Request {
	document := contract.OCRDocument{}
	if wire.Document != nil {
		document.Type = wire.Document.Type
		switch wire.Document.Type {
		case "document_url":
			if wire.Document.ImageURL == "" {
				document.URL = wire.Document.DocumentURL
			}
		case "image_url":
			if wire.Document.DocumentURL == "" {
				document.URL = wire.Document.ImageURL
			}
		}
	}
	return contract.Request{ID: id, Operation: contract.OperationOCR, PublicModel: strings.TrimSpace(wire.Model),
		EstimatedUsage: contract.Usage{Estimated: true, Measures: measures},
		OCR: &contract.OCRRequest{Document: document, Pages: append([]int(nil), wire.Pages...), IncludeImageBase64: wire.IncludeImageBase64,
			ImageLimit: wire.ImageLimit, ImageMinSize: wire.ImageMinSize, BBoxAnnotationFormat: append([]byte(nil), wire.BBoxAnnotationFormat...),
			DocumentAnnotationFormat: append([]byte(nil), wire.DocumentAnnotationFormat...), DocumentAnnotationPrompt: wire.DocumentAnnotationPrompt,
			ExtractHeader: wire.ExtractHeader, ExtractFooter: wire.ExtractFooter, TableFormat: wire.TableFormat,
			ConfidenceScoresGranularity: wire.ConfidenceScoresGranularity, IncludeBlocks: wire.IncludeBlocks, ID: wire.ID}}
}

func encodeOCRResponse(response contract.Response) (map[string]any, error) {
	if response.OCR == nil || response.OCR.Pages == nil || response.OCR.Model == "" {
		return nil, gatewayError(contract.ErrorProvider, "provider returned an invalid OCR response", http.StatusBadGateway, false, nil)
	}
	pages := make([]map[string]any, 0, len(response.OCR.Pages))
	for _, page := range response.OCR.Pages {
		value := map[string]any{"index": page.Index, "markdown": page.Markdown}
		if page.Images != nil {
			images := make([]map[string]any, 0, len(page.Images))
			for _, image := range page.Images {
				item := map[string]any{}
				if image.ImageBase64 != "" {
					item["image_base64"] = image.ImageBase64
				}
				if len(image.BBox) != 0 {
					item["bbox"] = json.RawMessage(image.BBox)
				}
				images = append(images, item)
			}
			value["images"] = images
		}
		if page.Dimensions != nil {
			value["dimensions"] = map[string]any{"dpi": page.Dimensions.DPI, "height": page.Dimensions.Height, "width": page.Dimensions.Width}
		}
		pages = append(pages, value)
	}
	result := map[string]any{"object": "ocr", "pages": pages, "model": response.OCR.Model}
	if len(response.OCR.DocumentAnnotation) != 0 {
		result["document_annotation"] = json.RawMessage(response.OCR.DocumentAnnotation)
	}
	if response.OCR.UsageInfo != nil {
		result["usage_info"] = map[string]any{"pages_processed": response.OCR.UsageInfo.PagesProcessed, "credits": response.OCR.UsageInfo.Credits, "doc_size_bytes": response.OCR.UsageInfo.DocSizeBytes}
	}
	if response.OCR.Content != "" {
		result["content"] = response.OCR.Content
	}
	if response.OCR.Tables != nil {
		result["tables"] = response.OCR.Tables
	}
	if response.OCR.KeyValuePairs != nil {
		result["keyValuePairs"] = response.OCR.KeyValuePairs
	}
	return result, nil
}
