package guardrail

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/daptin/llmgateway/catalog"
	"github.com/daptin/llmgateway/contract"
)

type Decision struct {
	Allowed bool   `json:"allowed"`
	Reason  string `json:"reason,omitempty"`
}

type Checker interface {
	CheckInput(context.Context, contract.Request) (Decision, error)
	CheckOutput(context.Context, contract.Request, contract.Response) (Decision, error)
	CheckStream(context.Context, contract.Request, contract.StreamEvent) (Decision, error)
	SupportsStreaming() bool
	CacheStable() bool
}

type Factory interface {
	Build(catalog.Guardrail) (Checker, error)
}

type FactoryFunc func(catalog.Guardrail) (Checker, error)

func (f FactoryFunc) Build(configuration catalog.Guardrail) (Checker, error) { return f(configuration) }

type Registry struct {
	mu        sync.RWMutex
	factories map[string]Factory
	frozen    bool
}

func NewRegistry() *Registry { return &Registry{factories: make(map[string]Factory)} }

func (r *Registry) Register(kind string, factory Factory) error {
	kind = strings.TrimSpace(kind)
	if kind == "" || factory == nil {
		return errors.New("guardrail kind and factory are required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.frozen {
		return errors.New("guardrail registry is frozen")
	}
	if _, exists := r.factories[kind]; exists {
		return fmt.Errorf("guardrail factory %q is already registered", kind)
	}
	r.factories[kind] = factory
	return nil
}

func (r *Registry) Freeze() {
	r.mu.Lock()
	r.frozen = true
	r.mu.Unlock()
}

func (r *Registry) Factory(kind string) (Factory, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	factory, ok := r.factories[kind]
	return factory, ok
}
