package openai

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestDecodeBatchInputPreservesPhysicalLinesAndValidatesEndpoint(t *testing.T) {
	data := []byte("\n" +
		`{"custom_id":"one","method":"post","url":"/v1/chat/completions","body":{"model":"m"}}` + "\n\n" +
		`{"custom_id":"two","method":"POST","url":"/v1/chat/completions","body":{"model":"m"}}` + "\n")
	lines, err := DecodeBatchInput(data, "/v1/chat/completions")
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 2 || lines[0].Line != 2 || lines[1].Line != 4 {
		t.Fatalf("decoded lines = %#v", lines)
	}
	if _, err := DecodeBatchInput(data, "/v1/embeddings"); err == nil {
		t.Fatal("accepted input for a different batch endpoint")
	}
}

func TestDecodeBatchInputRejectsAmbiguousRecords(t *testing.T) {
	for _, data := range []string{
		`{"custom_id":"one","custom_id":"two","method":"POST","url":"/v1/chat/completions","body":{}}`,
		`{"custom_id":"one","method":"POST","url":"/v1/chat/completions","body":{"model":"a","model":"b"}}`,
		`{"custom_id":"one","method":"POST","url":"/v1/unknown","body":{}}`,
		`{"custom_id":"one","method":"POST","url":"/v1/chat/completions","body":[]}`,
	} {
		if _, err := DecodeBatchInput([]byte(data), ""); err == nil {
			t.Fatalf("accepted invalid batch record: %s", data)
		}
	}
	duplicate := strings.Join([]string{
		`{"custom_id":"same","method":"POST","url":"/v1/chat/completions","body":{}}`,
		`{"custom_id":"same","method":"POST","url":"/v1/chat/completions","body":{}}`,
	}, "\n")
	if _, err := DecodeBatchInput([]byte(duplicate), ""); err == nil {
		t.Fatal("accepted duplicate custom_id")
	}
}

func TestEncodeBatchOutputUsesOpenAIJSONLShape(t *testing.T) {
	encoded, err := EncodeBatchOutput([]BatchOutputLine{{ID: "request-1", CustomID: "one", StatusCode: 200,
		RequestID: "upstream-1", Body: json.RawMessage(`{"id":"response-1"}`)}})
	if err != nil {
		t.Fatal(err)
	}
	var line map[string]any
	if err := json.Unmarshal(encoded, &line); err != nil {
		t.Fatal(err)
	}
	response, ok := line["response"].(map[string]any)
	if !ok || response["status_code"] != float64(200) || line["custom_id"] != "one" || line["error"] != nil {
		t.Fatalf("output = %#v", line)
	}
}
