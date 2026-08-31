package openai

import (
	"testing"

	"github.com/daptin/llmgateway/contract"
)

func TestDecodeChatRequestUsesTheHTTPCanonicalContract(t *testing.T) {
	request, err := DecodeChatRequest("action-1", []byte(`{"model":"public","messages":[{"role":"user","content":"hello"}],"max_tokens":12}`))
	if err != nil {
		t.Fatal(err)
	}
	if request.ID != "action-1" || request.Operation != contract.OperationChat || request.PublicModel != "public" ||
		request.Chat == nil || request.Chat.MaxCompletionTokens != 12 || request.EstimatedUsage.OutputTokens != 12 {
		t.Fatalf("unexpected canonical chat request: %#v", request)
	}
	for _, body := range []string{
		`{"model":"public","model":"shadow","messages":[{"role":"user","content":"hello"}]}`,
		`{"model":"public","messages":[{"role":"user","content":"hello"}],"unknown":true}`,
	} {
		if _, err := DecodeChatRequest("action-1", []byte(body)); err == nil {
			t.Fatalf("accepted invalid chat action body: %s", body)
		}
	}
}

func TestDecodeEmbeddingsRequestUsesTheHTTPCanonicalContract(t *testing.T) {
	request, err := DecodeEmbeddingsRequest("action-2", []byte(`{"model":"embedding","input":["one","two"],"dimensions":8}`))
	if err != nil {
		t.Fatal(err)
	}
	if request.ID != "action-2" || request.Operation != contract.OperationEmbeddings || request.Embeddings == nil ||
		len(request.Embeddings.Input.Texts) != 2 || request.Embeddings.Dimensions != 8 || !request.EstimatedUsage.Estimated {
		t.Fatalf("unexpected canonical embeddings request: %#v", request)
	}
	if _, err := DecodeEmbeddingsRequest("action-2", []byte(`{"model":"embedding","input":"one","unknown":true}`)); err == nil {
		t.Fatal("accepted an unknown embeddings field")
	}
}
