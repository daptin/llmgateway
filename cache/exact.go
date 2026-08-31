package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/daptin/llmgateway/catalog"
	"github.com/daptin/llmgateway/contract"
)

type keyMaterial struct {
	Revision  uint64
	ModelID   contract.ID
	Principal *principalMaterial
	Request   requestMaterial
}

type principalMaterial struct {
	KeyID   contract.ID
	OwnerID contract.ID
	TeamID  contract.ID
}

type requestMaterial struct {
	Operation       contract.Operation
	PublicModel     string
	MaxOutputTokens int64
	Chat            *contract.ChatRequest
	Responses       *contract.ResponsesRequest
	Embeddings      *contract.EmbeddingsRequest
}

func Eligible(model catalog.Model, request contract.Request, guardrailsStable bool) bool {
	if request.Stream || !model.Capabilities["exact_cache"] || !guardrailsStable {
		return false
	}
	switch request.Operation {
	case contract.OperationEmbeddings:
		return true
	case contract.OperationChat:
		return request.Chat != nil && request.Chat.N == 1 && request.Chat.Temperature != nil && *request.Chat.Temperature == 0 && len(request.Chat.Tools) == 0
	case contract.OperationResponses:
		return request.Responses != nil && len(request.Responses.Tools) == 0
	default:
		return false
	}
}

func Key(revision uint64, model catalog.Model, principal contract.Principal, request contract.Request) (string, error) {
	material := keyMaterial{
		Revision: revision, ModelID: model.ID,
		Request: requestMaterial{Operation: request.Operation, PublicModel: request.PublicModel, MaxOutputTokens: request.MaxOutputTokens, Chat: request.Chat, Responses: request.Responses, Embeddings: request.Embeddings},
	}
	if !model.Capabilities["public_cache"] {
		material.Principal = &principalMaterial{KeyID: principal.KeyID, OwnerID: principal.OwnerID, TeamID: principal.TeamID}
	}
	payload, err := json.Marshal(material)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(payload)
	return "llmgateway:exact:" + hex.EncodeToString(digest[:]), nil
}
