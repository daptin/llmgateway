package contract

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/url"
	"strings"
)

type OCRDocument struct {
	Type string
	URL  string
}

type OCRRequest struct {
	Document                    OCRDocument
	Pages                       []int
	IncludeImageBase64          *bool
	ImageLimit                  *int
	ImageMinSize                *int
	BBoxAnnotationFormat        json.RawMessage
	DocumentAnnotationFormat    json.RawMessage
	DocumentAnnotationPrompt    string
	ExtractHeader               *bool
	ExtractFooter               *bool
	TableFormat                 string
	ConfidenceScoresGranularity string
	IncludeBlocks               *bool
	ID                          string
}

type OCRResponse struct {
	Pages              []OCRPage
	Model              string
	DocumentAnnotation json.RawMessage
	UsageInfo          *OCRUsageInfo
	Content            string
	Tables             []json.RawMessage
	KeyValuePairs      []json.RawMessage
}

type OCRPage struct {
	Index      int
	Markdown   string
	Images     []OCRPageImage
	Dimensions *OCRPageDimensions
}

type OCRPageImage struct {
	ImageBase64 string
	BBox        json.RawMessage
}

type OCRPageDimensions struct {
	DPI    int
	Height int
	Width  int
}

type OCRUsageInfo struct {
	PagesProcessed int64
	Credits        float64
	DocSizeBytes   int64
}

func validOCRDocument(document OCRDocument) bool {
	if (document.Type != "document_url" && document.Type != "image_url") || document.URL == "" {
		return false
	}
	if strings.HasPrefix(document.URL, "data:") {
		header, payload, found := strings.Cut(document.URL, ",")
		return found && strings.HasSuffix(strings.ToLower(header), ";base64") && len(payload) != 0 && validBase64(payload)
	}
	parsed, err := url.ParseRequestURI(document.URL)
	return err == nil && (parsed.Scheme == "http" || parsed.Scheme == "https") && parsed.Host != "" && parsed.User == nil
}

func validBase64(value string) bool {
	decoder := base64.NewDecoder(base64.StdEncoding, strings.NewReader(value))
	buffer := make([]byte, 32*1024)
	for {
		_, err := decoder.Read(buffer)
		if err == nil {
			continue
		}
		return err == io.EOF
	}
}
