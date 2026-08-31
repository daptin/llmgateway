package llmgateway

import (
	"context"
	"encoding/json"

	exactcache "github.com/daptin/llmgateway/cache"
	"github.com/daptin/llmgateway/contract"
)

func (e *Engine) lookupCache(ctx context.Context, principal contract.Principal, prepared preparedRequest) (string, contract.Response, bool) {
	stable := true
	for _, bound := range prepared.runtime.guardrails[prepared.model.ID] {
		if !bound.checker.CacheStable() {
			stable = false
			break
		}
	}
	if !exactcache.Eligible(prepared.model, prepared.request, stable) {
		return "", contract.Response{}, false
	}
	key, err := exactcache.Key(prepared.runtime.catalog.Revision(), prepared.model, principal, prepared.request)
	if err != nil {
		e.recordCache(ctx, prepared.request, "key_error")
		return "", contract.Response{}, false
	}
	cacheCtx, cancel := context.WithTimeout(ctx, e.cacheTimeout)
	defer cancel()
	payload, found, err := e.cache.Get(cacheCtx, key)
	if err != nil {
		e.recordCache(ctx, prepared.request, "get_error")
		return key, contract.Response{}, false
	}
	if !found {
		e.recordCache(ctx, prepared.request, "miss")
		return key, contract.Response{}, false
	}
	if len(payload) > e.maxCacheEntryBytes {
		_ = e.cache.Delete(cacheCtx, key)
		e.recordCache(ctx, prepared.request, "invalid")
		return key, contract.Response{}, false
	}
	var response contract.Response
	if json.Unmarshal(payload, &response) != nil || !validCachedResponse(prepared.request.Operation, response) {
		_ = e.cache.Delete(cacheCtx, key)
		e.recordCache(ctx, prepared.request, "invalid")
		return key, contract.Response{}, false
	}
	e.recordCache(ctx, prepared.request, "hit")
	return key, response, true
}

func (e *Engine) storeCache(ctx context.Context, key string, response contract.Response) {
	if key == "" {
		return
	}
	payload, err := json.Marshal(response)
	if err != nil || len(payload) > e.maxCacheEntryBytes {
		e.recordCache(ctx, contract.Request{ID: response.RequestID}, "store_skipped")
		return
	}
	cacheCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), e.cacheTimeout)
	defer cancel()
	if err := e.cache.Set(cacheCtx, key, payload, e.cacheTTL); err != nil {
		e.recordCache(ctx, contract.Request{ID: response.RequestID}, "set_error")
	}
}

func validCachedResponse(operation contract.Operation, response contract.Response) bool {
	if !response.Usage.Valid() {
		return false
	}
	switch operation {
	case contract.OperationChat:
		return response.Chat != nil && response.Responses == nil && response.Embeddings == nil && response.ImageGeneration == nil
	case contract.OperationResponses:
		return response.Chat == nil && response.Responses != nil && response.Embeddings == nil && response.ImageGeneration == nil
	case contract.OperationEmbeddings:
		return response.Chat == nil && response.Responses == nil && response.Embeddings != nil && response.ImageGeneration == nil
	default:
		return false
	}
}

func (e *Engine) recordCache(ctx context.Context, request contract.Request, status string) {
	e.telemetry.Record(ctx, TelemetryEvent{Name: "cache", RequestID: request.ID, Attributes: map[string]string{"status": status}})
}
