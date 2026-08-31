package main

import (
	"context"
	"fmt"
	"log"

	"github.com/daptin/llmgateway"
	"github.com/daptin/llmgateway/adapter"
	"github.com/daptin/llmgateway/catalog"
	"github.com/daptin/llmgateway/contract"
	"github.com/daptin/llmgateway/guardrail"
	"github.com/daptin/llmgateway/testkit"
)

func main() {
	document := catalog.Document{
		Revision:  1,
		Providers: []catalog.Provider{{ID: "provider", Name: "example", Type: "openai", Enabled: true}},
		Models: []catalog.Model{{
			ID: "model", Name: "example-model", Operations: []contract.Operation{contract.OperationChat},
			UnsupportedParameterPolicy: "reject", Enabled: true,
		}},
		Deployments: []catalog.Deployment{{
			ID: "deployment", Name: "example", ModelID: "model", ProviderID: "provider",
			UpstreamModel: "upstream-model", Operations: []contract.Operation{contract.OperationChat},
			Weight: 1, MaxConcurrency: -1, RPM: -1, TPM: -1, Enabled: true,
		}},
	}
	registry := adapter.NewRegistry()
	if err := registry.Register("openai", adapter.FactoryFunc(func(context.Context, catalog.Provider, adapter.Secret) (adapter.Adapter, error) {
		return testkit.NewFaultAdapter(adapter.Capabilities{Operations: map[contract.Operation]bool{contract.OperationChat: true}}), nil
	})); err != nil {
		log.Fatal(err)
	}
	clock := llmgateway.SystemClock{}
	engine, err := llmgateway.New(llmgateway.Dependencies{
		Catalog: testkit.NewCatalogSource(document), Adapters: registry,
		Authorizer: testkit.AllowAuthorizer{}, Accounting: testkit.NewAccountingStore(), Counters: testkit.NewCounterStore(clock.Now), Cache: llmgateway.DisabledResponseCache{},
		Guardrails: guardrail.NewRegistry(), Telemetry: llmgateway.DiscardTelemetrySink{}, Selector: testkit.NewSelector(0), Clock: clock,
	}, llmgateway.Options{})
	if err != nil {
		log.Fatal(err)
	}
	if err := engine.Reload(context.Background()); err != nil {
		log.Fatal(err)
	}
	snapshot, err := engine.Snapshot()
	if err != nil {
		log.Fatal(err)
	}
	model, ok := snapshot.ModelByName("example-model")
	if !ok {
		log.Fatal("compiled model is missing")
	}
	fmt.Printf("catalog revision %d loaded model %s\n", snapshot.Revision(), model.Name)
	if err := engine.Drain(context.Background()); err != nil {
		log.Fatal(err)
	}
}
