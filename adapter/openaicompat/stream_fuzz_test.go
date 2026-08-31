package openaicompat

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/daptin/llmgateway/contract"
)

func FuzzEventStreamBoundsAndParsing(f *testing.F) {
	f.Add("data: [DONE]\n\n", false)
	f.Add("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n", false)
	f.Add("event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n", true)
	f.Add(strings.Repeat("x", 257), false)
	f.Fuzz(func(t *testing.T, payload string, responses bool) {
		if len(payload) > 4096 {
			t.Skip()
		}
		operation := contract.OperationChat
		if responses {
			operation = contract.OperationResponses
		}
		stream := newEventStream(io.NopCloser(strings.NewReader(payload)), operation, 256)
		defer stream.Close()
		for index := 0; index < 8; index++ {
			event, err := stream.Next(context.Background())
			if event.Terminal || err != nil {
				return
			}
		}
		t.Fatal("bounded input produced an unbounded event sequence")
	})
}
