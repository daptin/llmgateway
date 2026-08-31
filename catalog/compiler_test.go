package catalog

import (
	"testing"

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
		{name: "parameter drop", mutate: func(model *Model) { model.UnsupportedParameterPolicy = "drop" }},
		{name: "parameter passthrough", mutate: func(model *Model) { model.UnsupportedParameterPolicy = "passthrough" }},
		{name: "defaults", mutate: func(model *Model) { model.DefaultParameters = []byte(`{"temperature":0}`) }},
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
