package catalog

import (
	"testing"
	"time"

	"github.com/daptin/llmgateway/contract"
)

func validDocument() Document {
	return Document{
		Revision:    1,
		Providers:   []Provider{{ID: "provider-1", Name: "primary", Type: "openai", Enabled: true}},
		Models:      []Model{{ID: "model-1", Name: "public-model", Operations: []contract.Operation{contract.OperationChat}, RoutingStrategy: "priority_weighted", UnsupportedParameterPolicy: "reject", Enabled: true}},
		Deployments: []Deployment{{ID: "deployment-1", Name: "primary", ModelID: "model-1", ProviderID: "provider-1", UpstreamModel: "upstream-model", Operations: []contract.Operation{contract.OperationChat}, Weight: 1, MaxConcurrency: -1, RPM: -1, TPM: -1, Enabled: true}},
	}
}

func TestCompileNormalizesAndValidatesHealthChecks(t *testing.T) {
	document := validDocument()
	document.Deployments[0].HealthCheck = HealthCheck{Enabled: true}
	snapshot, err := Compile(document)
	if err != nil {
		t.Fatal(err)
	}
	check := snapshot.Deployments()[0].HealthCheck
	if !check.Enabled || check.Interval != 30*time.Second || check.Timeout != 5*time.Second || check.FailureThreshold != 3 {
		t.Fatalf("health check defaults = %+v", check)
	}
	document.Deployments[0].HealthCheck = HealthCheck{Enabled: true, Interval: time.Second, Timeout: 2 * time.Second, FailureThreshold: 1}
	if _, err := Compile(document); err == nil {
		t.Fatal("accepted a health timeout longer than its interval")
	}
}

func TestCompileAndLookupReturnsCopies(t *testing.T) {
	document := validDocument()
	document.Models[0].Capabilities = map[string]bool{"tools": true}
	snapshot, err := Compile(document)
	if err != nil {
		t.Fatal(err)
	}
	model, ok := snapshot.ModelByName("public-model")
	if !ok {
		t.Fatal("model not found")
	}
	model.Operations[0] = contract.OperationEmbeddings
	again, _ := snapshot.ModelByName("public-model")
	if again.Operations[0] != contract.OperationChat {
		t.Fatal("snapshot leaked mutable model state")
	}
	if !again.Capabilities["tools"] {
		t.Fatal("snapshot dropped model capabilities")
	}
	model.Capabilities["tools"] = false
	again, _ = snapshot.ModelByName("public-model")
	if !again.Capabilities["tools"] {
		t.Fatal("snapshot leaked mutable capability state")
	}
	deployments := snapshot.DeploymentsForModel("model-1")
	deployments[0].Operations[0] = contract.OperationEmbeddings
	againDeployments := snapshot.DeploymentsForModel("model-1")
	if againDeployments[0].Operations[0] != contract.OperationChat {
		t.Fatal("snapshot leaked mutable deployment state")
	}
}

func TestCompileRejectsFallbackCycle(t *testing.T) {
	document := validDocument()
	document.Models = append(document.Models,
		Model{ID: "model-2", Name: "fallback", Operations: []contract.Operation{contract.OperationChat}, RoutingStrategy: "priority_weighted", FallbackModelIDs: []contract.ID{"model-1"}, UnsupportedParameterPolicy: "reject"},
	)
	document.Models[0].FallbackModelIDs = []contract.ID{"model-2"}
	if _, err := Compile(document); err == nil {
		t.Fatal("expected fallback cycle rejection")
	}
}

func TestCompileRejectsDeploymentOperationOutsideModel(t *testing.T) {
	document := validDocument()
	document.Deployments[0].Operations = []contract.Operation{contract.OperationEmbeddings}
	if _, err := Compile(document); err == nil {
		t.Fatal("expected operation mismatch rejection")
	}
}

func TestCompileRejectsAcceptedLookingButUnimplementedModelConfiguration(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Model)
	}{
		{name: "routing strategy", mutate: func(model *Model) { model.RoutingStrategy = "least_busy" }},
		{name: "unknown parameter policy", mutate: func(model *Model) { model.UnsupportedParameterPolicy = "guess" }},
		{name: "unknown default", mutate: func(model *Model) { model.DefaultParameters = []byte(`{"chat":{"magic":true}}`) }},
		{name: "zero default", mutate: func(model *Model) { model.DefaultParameters = []byte(`{"chat":{"n":0}}`) }},
		{name: "undeclared operation default", mutate: func(model *Model) { model.DefaultParameters = []byte(`{"embeddings":{"encoding_format":"float"}}`) }},
		{name: "unknown capability", mutate: func(model *Model) { model.Capabilities = map[string]bool{"magic": true} }},
		{name: "public cache without exact cache", mutate: func(model *Model) { model.Capabilities = map[string]bool{"public_cache": true} }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			document := validDocument()
			test.mutate(&document.Models[0])
			if _, err := Compile(document); err == nil {
				t.Fatal("expected catalog rejection")
			}
		})
	}
}

func TestCompileAcceptsImplementedParameterPolicies(t *testing.T) {
	for _, policy := range []string{"reject", "drop", "passthrough"} {
		document := validDocument()
		document.Models[0].UnsupportedParameterPolicy = policy
		if _, err := Compile(document); err != nil {
			t.Fatalf("policy %q: %v", policy, err)
		}
	}
}

func TestCompiledDefaultsNormalizeWithoutMutatingCaller(t *testing.T) {
	document := validDocument()
	document.Models[0].DefaultParameters = []byte(`{"chat":{"n":2,"temperature":0,"max_completion_tokens":32,"stop":["done"]}}`)
	snapshot, err := Compile(document)
	if err != nil {
		t.Fatal(err)
	}
	request := contract.Request{Operation: contract.OperationChat, Chat: &contract.ChatRequest{}}
	normalized, err := snapshot.ApplyDefaults("model-1", request, 4096)
	if err != nil {
		t.Fatal(err)
	}
	if request.Chat.N != 0 || normalized.Chat.N != 2 || normalized.Chat.MaxCompletionTokens != 32 ||
		normalized.MaxOutputTokens != 32 || normalized.Chat.Temperature == nil || *normalized.Chat.Temperature != 0 || len(normalized.Chat.Stop) != 1 {
		t.Fatalf("request=%+v normalized=%+v", request, normalized)
	}
	explicit := contract.Request{Operation: contract.OperationChat, Chat: &contract.ChatRequest{N: 1, MaxCompletionTokens: 7}}
	normalized, err = snapshot.ApplyDefaults("model-1", explicit, 4096)
	if err != nil {
		t.Fatal(err)
	}
	if normalized.Chat.N != 1 || normalized.Chat.MaxCompletionTokens != 7 || normalized.MaxOutputTokens != 7 {
		t.Fatalf("explicit request was overwritten: %+v", normalized)
	}
}

func TestStoreRejectsStaleRevision(t *testing.T) {
	first, err := Compile(validDocument())
	if err != nil {
		t.Fatal(err)
	}
	var store Store
	if err := store.Swap(first); err != nil {
		t.Fatal(err)
	}
	if err := store.Swap(first); err != ErrStaleRevision {
		t.Fatalf("expected ErrStaleRevision, got %v", err)
	}
}
