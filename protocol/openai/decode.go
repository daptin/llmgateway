package openai

import (
	"net/http"

	"github.com/daptin/llmgateway/contract"
)

func DecodeChatRequest(id contract.ID, body []byte) (contract.Request, error) {
	if err := rejectDuplicateJSONKeys(body); err != nil {
		return contract.Request{}, gatewayError(contract.ErrorInvalidRequest, "request contains invalid or duplicate JSON fields", http.StatusBadRequest, false, err)
	}
	var wire chatRequest
	if err := decodeStrict(body, &wire); err != nil {
		return contract.Request{}, gatewayError(contract.ErrorInvalidRequest, "invalid chat completion request", http.StatusBadRequest, false, err)
	}
	request, _, err := canonicalChat(id, wire, int64(len(body)))
	return request, err
}

func DecodeEmbeddingsRequest(id contract.ID, body []byte) (contract.Request, error) {
	if err := rejectDuplicateJSONKeys(body); err != nil {
		return contract.Request{}, gatewayError(contract.ErrorInvalidRequest, "request contains invalid or duplicate JSON fields", http.StatusBadRequest, false, err)
	}
	var wire embeddingsRequest
	if err := decodeStrict(body, &wire); err != nil {
		return contract.Request{}, gatewayError(contract.ErrorInvalidRequest, "invalid embeddings request", http.StatusBadRequest, false, err)
	}
	return canonicalEmbeddings(id, wire, int64(len(body)))
}
