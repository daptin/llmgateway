package guardrail

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
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
	checker, err := (HTTPFactory{}).Build(catalog.Guardrail{Config: json.RawMessage(`{"endpoint":"` + server.URL + `/check","allow_insecure":true,"allow_private_network":true}`)})
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
		`{"endpoint":"https://example.test/check?tenant=hidden"}`,
		`{"endpoint":"http://127.0.0.1/check","allow_insecure":true}`,
		`{"endpoint":"https://example.test/check","unknown":true}`,
	} {
		if _, err := (HTTPFactory{}).Build(catalog.Guardrail{Config: json.RawMessage(config)}); err == nil {
			t.Fatalf("accepted unsafe config %s", config)
		}
	}
}

func TestHTTPGuardrailDoesNotFollowRedirects(t *testing.T) {
	var followed atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { followed.Add(1) }))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		http.Redirect(response, request, target.URL, http.StatusTemporaryRedirect)
	}))
	defer source.Close()
	checker, err := (HTTPFactory{}).Build(catalog.Guardrail{Config: json.RawMessage(`{"endpoint":"` + source.URL + `","allow_insecure":true,"allow_private_network":true}`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := checker.CheckInput(context.Background(), contract.Request{ID: "redirect", Operation: contract.OperationChat}); err == nil {
		t.Fatal("guardrail redirect was accepted")
	}
	if followed.Load() != 0 {
		t.Fatal("guardrail followed a redirect")
	}
}
