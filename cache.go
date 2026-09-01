package llmgateway

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	exactcache "github.com/daptin/llmgateway/cache"
	"github.com/daptin/llmgateway/contract"
)

const cacheFillPollInterval = 25 * time.Millisecond

func (e *Engine) lookupCache(ctx context.Context, principal contract.Principal, prepared preparedRequest) (string, contract.Response, bool) {
	stable := true
	for _, bound := range prepared.runtime.guardrails[prepared.model.ID] {
		if !bound.cacheStable {
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
	response, found, status := e.readCache(ctx, key, prepared.request.Operation)
	e.recordCache(ctx, prepared.request, status)
	return key, response, found
}

func (e *Engine) readCache(ctx context.Context, key string, operation contract.Operation) (contract.Response, bool, string) {
	cacheCtx, cancel := context.WithTimeout(ctx, e.cacheTimeout)
	defer cancel()
	payload, found, err := e.cache.Get(cacheCtx, key)
	if err != nil {
		return contract.Response{}, false, "get_error"
	}
	if !found {
		return contract.Response{}, false, "miss"
	}
	if len(payload) > e.maxCacheEntryBytes {
		_ = e.cache.Delete(cacheCtx, key)
		return contract.Response{}, false, "invalid"
	}
	var response contract.Response
	if json.Unmarshal(payload, &response) != nil || !validCachedResponse(operation, response) {
		_ = e.cache.Delete(cacheCtx, key)
		return contract.Response{}, false, "invalid"
	}
	return response, true, "hit"
}

func (e *Engine) coordinateCacheFill(ctx context.Context, request contract.Request, key string) (contract.Response, bool, string) {
	leaseKey := "llmgateway:cache-fill:" + key
	lease, err := e.counters.Acquire(ctx, leaseKey, 1, e.requestTimeout)
	if err == nil {
		e.recordCache(ctx, request, "fill_owner")
		return contract.Response{}, false, lease
	}
	if !errors.Is(err, ErrCounterLimit) {
		e.recordCache(ctx, request, "fill_coordination_error")
		return contract.Response{}, false, ""
	}
	e.recordCache(ctx, request, "fill_wait")
	deadline := e.clock.Now().Add(e.cacheFillWait)
	for {
		remaining := deadline.Sub(e.clock.Now())
		if remaining <= 0 {
			e.recordCache(ctx, request, "fill_wait_timeout")
			return contract.Response{}, false, ""
		}
		delay := cacheFillPollInterval
		if remaining < delay {
			delay = remaining
		}
		timer := e.clock.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return contract.Response{}, false, ""
		case <-timer.C():
		}
		response, found, status := e.readCache(ctx, key, request.Operation)
		if found {
			e.recordCache(ctx, request, "coalesced_hit")
			return response, true, ""
		}
		if status == "get_error" {
			e.recordCache(ctx, request, "fill_coordination_error")
			return contract.Response{}, false, ""
		}
		lease, err = e.counters.Acquire(ctx, leaseKey, 1, e.requestTimeout)
		if err == nil {
			e.recordCache(ctx, request, "fill_owner")
			return contract.Response{}, false, lease
		}
		if !errors.Is(err, ErrCounterLimit) {
			e.recordCache(ctx, request, "fill_coordination_error")
			return contract.Response{}, false, ""
		}
	}
}

func (e *Engine) releaseCacheFill(ctx context.Context, lease string) {
	releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), e.finalizationTimeout)
	defer cancel()
	_ = e.counters.Release(releaseCtx, lease)
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
	payloads := []bool{response.Chat != nil, response.TextCompletion != nil, response.Responses != nil, response.Embeddings != nil,
		response.Images != nil, response.Moderation != nil, response.Rerank != nil, response.AudioSpeech != nil, response.Transcription != nil, response.Search != nil, response.OCR != nil}
	count := 0
	for _, present := range payloads {
		if present {
			count++
		}
	}
	if count != 1 {
		return false
	}
	switch operation {
	case contract.OperationChat:
		return response.Chat != nil
	case contract.OperationTextCompletion:
		return response.TextCompletion != nil
	case contract.OperationResponses:
		return response.Responses != nil
	case contract.OperationEmbeddings:
		return response.Embeddings != nil
	case contract.OperationImageGeneration, contract.OperationImageEdit, contract.OperationImageVariation:
		return response.Images != nil
	case contract.OperationModeration:
		return response.Moderation != nil
	case contract.OperationRerank:
		return response.Rerank != nil
	case contract.OperationAudioSpeech:
		return response.AudioSpeech != nil
	case contract.OperationTranscription, contract.OperationTranslation:
		return response.Transcription != nil
	case contract.OperationSearch:
		return response.Search != nil
	case contract.OperationOCR:
		return response.OCR != nil
	default:
		return false
	}
}

func (e *Engine) recordCache(ctx context.Context, request contract.Request, status string) {
	e.telemetry.Record(ctx, TelemetryEvent{Name: "cache", RequestID: request.ID, Attributes: map[string]string{"status": status}})
}
