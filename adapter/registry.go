package adapter

import (
	"errors"
	"fmt"
	"strings"
	"sync"
)

type Registry struct {
	mu        sync.RWMutex
	factories map[string]Factory
	frozen    bool
}

func NewRegistry() *Registry {
	return &Registry{factories: make(map[string]Factory)}
}

func (r *Registry) Register(providerType string, factory Factory) error {
	providerType = strings.TrimSpace(providerType)
	if providerType == "" || factory == nil {
		return errors.New("provider type and factory are required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.frozen {
		return errors.New("adapter registry is frozen")
	}
	if _, exists := r.factories[providerType]; exists {
		return fmt.Errorf("adapter factory %q is already registered", providerType)
	}
	r.factories[providerType] = factory
	return nil
}

func (r *Registry) Freeze() {
	r.mu.Lock()
	r.frozen = true
	r.mu.Unlock()
}

func (r *Registry) Factory(providerType string) (Factory, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	factory, ok := r.factories[providerType]
	return factory, ok
}
