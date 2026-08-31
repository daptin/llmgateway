package guardrail

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/daptin/llmgateway/catalog"
	"github.com/daptin/llmgateway/contract"
)

func TestHTTPGuardrailUsesFixedBoundedEndpoint(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/check" || request.Method != http.MethodPost {
			t.Fatalf("unexpected request %s %s", request.Method, request.URL.Path)
		}
		body, _ := io.ReadAll(request.Body)
		if !strings.Contains(string(body), `"text":"hello"`) || strings.Contains(string(body), "endpoint") {
			t.Fatalf("unexpected body: %s", body)
		}
		_, _ = io.WriteString(response, `{"allowed":false,"reason":"policy"}`)
	}))
	defer server.Close()
	checker, err := (HTTPFactory{}).Build(catalog.Guardrail{Config: json.RawMessage(`{"endpoint":"` + server.URL + `/check","allow_insecure":true}`)})
	if err != nil {
		t.Fatal(err)
	}
	decision, err := checker.CheckInput(context.Background(), contract.Request{ID: "request", Operation: contract.OperationChat, Chat: &contract.ChatRequest{Messages: []contract.Message{{Content: []contract.ContentPart{{Text: "hello"}}}}}})
	if err != nil || decision.Allowed || decision.Reason != "policy" {
		t.Fatalf("decision=%#v err=%v", decision, err)
	}
}

func TestHTTPGuardrailRejectsUnsafeOrUnknownConfiguration(t *testing.T) {
	for _, config := range []string{
		`{"endpoint":"http://example.test/check"}`,
		`{"endpoint":"https://user:password@example.test/check"}`,
		`{"endpoint":"https://example.test/check","unknown":true}`,
	} {
		if _, err := (HTTPFactory{}).Build(catalog.Guardrail{Config: json.RawMessage(config)}); err == nil {
			t.Fatalf("accepted unsafe config %s", config)
		}
	}
}
