package contract

import "testing"

func TestResponseInputFileRequiresOneValidCanonicalSource(t *testing.T) {
	request := func(file *InputFile) Request {
		return Request{ID: "request", PublicModel: "model", Operation: OperationResponses, MaxOutputTokens: 1, Responses: &ResponsesRequest{
			Input: []ResponseInputItem{
				{Type: "message", Role: "user", Content: []ContentPart{{Type: "input_file", File: file}}},
			},
		}}
	}
	for _, file := range []*InputFile{
		{Data: "data:application/pdf;base64,cGRm"},
		{URL: "https://example.test/brief.pdf"},
	} {
		if err := request(file).Validate(); err != nil {
			t.Fatalf("valid file source rejected: %#v: %v", file, err)
		}
	}
	for _, file := range []*InputFile{
		nil,
		{},
		{Data: "data:application/pdf;base64,cGRm", URL: "https://example.test/brief.pdf"},
		{URL: "file:///tmp/brief.pdf"},
		{URL: "https://user@example.test/brief.pdf"},
	} {
		if err := request(file).Validate(); err == nil {
			t.Fatalf("invalid file source accepted: %#v", file)
		}
	}
}
