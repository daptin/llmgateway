package routing_test

import (
	"testing"

	"github.com/daptin/llmgateway/adapter"
	"github.com/daptin/llmgateway/catalog"
	"github.com/daptin/llmgateway/contract"
	"github.com/daptin/llmgateway/internal/routing"
	"github.com/daptin/llmgateway/testkit"
)

type capabilityMap map[contract.ID]adapter.Capabilities

func (c capabilityMap) Capabilities(id contract.ID) (adapter.Capabilities, bool) {
	value, ok := c[id]
	return value, ok
}

func routingSnapshot(t *testing.T) *catalog.Snapshot {
	t.Helper()
	document := catalog.Document{
		Revision: 1,
		Providers: []catalog.Provider{
			{ID: "p1", Name: "one", Type: "test", Enabled: true},
			{ID: "p2", Name: "two", Type: "test", Enabled: true},
			{ID: "p3", Name: "three", Type: "test", Enabled: true},
		},
		Models: []catalog.Model{
			{ID: "m1", Name: "public", Operations: []contract.Operation{contract.OperationChat}, RoutingStrategy: "priority_weighted", FallbackModelIDs: []contract.ID{"m2"}, UnsupportedParameterPolicy: "reject", Enabled: true},
			{ID: "m2", Name: "fallback", Operations: []contract.Operation{contract.OperationChat}, RoutingStrategy: "priority_weighted", UnsupportedParameterPolicy: "reject", Enabled: true},
		},
		Deployments: []catalog.Deployment{
			{ID: "d1", Name: "one", ModelID: "m1", ProviderID: "p1", UpstreamModel: "a", Operations: []contract.Operation{contract.OperationChat}, Priority: 0, Weight: 1, MaxConcurrency: -1, RPM: -1, TPM: -1, Enabled: true},
			{ID: "d2", Name: "two", ModelID: "m1", ProviderID: "p2", UpstreamModel: "b", Operations: []contract.Operation{contract.OperationChat}, Priority: 0, Weight: 3, MaxConcurrency: -1, RPM: -1, TPM: -1, Enabled: true},
			{ID: "d3", Name: "three", ModelID: "m2", ProviderID: "p3", UpstreamModel: "c", Operations: []contract.Operation{contract.OperationChat}, Priority: 0, Weight: 1, MaxConcurrency: -1, RPM: -1, TPM: -1, Enabled: true},
		},
	}
	snapshot, err := catalog.Compile(document)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func TestBuildUsesWeightWithoutReplacementThenFallback(t *testing.T) {
	capabilities := capabilityMap{
		"p1": {Operations: map[contract.Operation]bool{contract.OperationChat: true}},
		"p2": {Operations: map[contract.Operation]bool{contract.OperationChat: true}},
		"p3": {Operations: map[contract.Operation]bool{contract.OperationChat: true}},
	}
	plan, err := routing.Build(routingSnapshot(t), "public", contract.OperationChat, capabilities, testkit.NewSelector(2, 0, 0))
	if err != nil {
		t.Fatal(err)
	}
	want := []contract.ID{"d2", "d1", "d3"}
	if len(plan.Attempts) != len(want) {
		t.Fatalf("got %d attempts, want %d", len(plan.Attempts), len(want))
	}
	for index, attempt := range plan.Attempts {
		if attempt.Deployment.ID != want[index] {
			t.Fatalf("attempt %d deployment=%s want=%s", index, attempt.Deployment.ID, want[index])
		}
	}
}

func TestBuildFiltersProviderWithoutCapability(t *testing.T) {
	capabilities := capabilityMap{
		"p1": {Operations: map[contract.Operation]bool{contract.OperationChat: true}},
		"p3": {Operations: map[contract.Operation]bool{contract.OperationChat: true}},
	}
	plan, err := routing.Build(routingSnapshot(t), "public", contract.OperationChat, capabilities, testkit.NewSelector(0))
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Attempts) != 2 || plan.Attempts[0].Deployment.ID != "d1" || plan.Attempts[1].Deployment.ID != "d3" {
		t.Fatalf("unexpected attempts: %#v", plan.Attempts)
	}
}

func TestBuildRejectsUnknownModel(t *testing.T) {
	_, err := routing.Build(routingSnapshot(t), "missing", contract.OperationChat, capabilityMap{}, testkit.NewSelector(0))
	if err == nil {
		t.Fatal("expected missing model error")
	}
}
