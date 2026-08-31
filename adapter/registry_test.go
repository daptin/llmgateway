package adapter

import (
	"context"
	"testing"

	"github.com/daptin/llmgateway/catalog"
	"github.com/daptin/llmgateway/contract"
)

type testAdapter struct{}

func (testAdapter) Capabilities() Capabilities { return Capabilities{} }
func (testAdapter) Invoke(context.Context, catalog.Deployment, contract.Request) (contract.Response, error) {
	return contract.Response{}, nil
}
func (testAdapter) Stream(context.Context, catalog.Deployment, contract.Request) (Stream, error) {
	return nil, nil
}

func TestRegistryRejectsDuplicatesAndMutationAfterFreeze(t *testing.T) {
	registry := NewRegistry()
	factory := FactoryFunc(func(context.Context, catalog.Provider, Secret) (Adapter, error) { return testAdapter{}, nil })
	if err := registry.Register("test", factory); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register("test", factory); err == nil {
		t.Fatal("expected duplicate registration rejection")
	}
	registry.Freeze()
	if err := registry.Register("other", factory); err == nil {
		t.Fatal("expected frozen registry rejection")
	}
	if _, ok := registry.Factory("test"); !ok {
		t.Fatal("registered factory not found")
	}
}

func TestSecretDoesNotRenderValue(t *testing.T) {
	secret := NewSecret([]byte("sensitive"))
	if secret.String() != "[REDACTED]" {
		t.Fatalf("unexpected secret rendering: %s", secret.String())
	}
	bytes := secret.Bytes()
	bytes[0] = 'x'
	if string(secret.Bytes()) != "sensitive" {
		t.Fatal("secret returned mutable backing data")
	}
}
