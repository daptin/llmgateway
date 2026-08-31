package testkit

import (
	"context"
	"sync"

	"github.com/daptin/llmgateway/catalog"
)

// CatalogSource is a deterministic, concurrency-safe source for host contract
// and engine tests. Set replaces the complete document.
type CatalogSource struct {
	mu       sync.RWMutex
	document catalog.Document
}

func NewCatalogSource(document catalog.Document) *CatalogSource {
	return &CatalogSource{document: document}
}

func (s *CatalogSource) Load(ctx context.Context, _ uint64) (catalog.Document, error) {
	if err := ctx.Err(); err != nil {
		return catalog.Document{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.document, nil
}

func (s *CatalogSource) Set(document catalog.Document) {
	s.mu.Lock()
	s.document = document
	s.mu.Unlock()
}
