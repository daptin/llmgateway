package openai

import (
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/daptin/llmgateway/compatibility"
)

func TestCompatibilityManifestDeclaresEveryAcceptedTopLevelRequestField(t *testing.T) {
	manifest, err := compatibility.Default()
	if err != nil {
		t.Fatal(err)
	}
	wires := map[string]reflect.Type{
		"POST /v1/chat/completions":   reflect.TypeOf(chatRequest{}),
		"POST /v1/responses":          reflect.TypeOf(responsesRequest{}),
		"POST /v1/embeddings":         reflect.TypeOf(embeddingsRequest{}),
		"POST /v1/images/generations": reflect.TypeOf(imageRequest{}),
	}
	for endpoint, wire := range wires {
		fields := make(map[string]compatibility.Support)
		for _, declared := range manifest.Endpoints {
			if strings.ToUpper(declared.Method)+" "+declared.Path == endpoint {
				fields = declared.RequestFields
				break
			}
		}
		if fields == nil {
			t.Fatalf("manifest is missing %s", endpoint)
		}
		accepted := requestFieldNames(wire)
		declared := make([]string, 0, len(fields))
		for field := range fields {
			declared = append(declared, field)
		}
		sort.Strings(declared)
		if !reflect.DeepEqual(accepted, declared) {
			t.Fatalf("%s fields drifted\naccepted: %v\ndeclared: %v", endpoint, accepted, declared)
		}
	}
}

func requestFieldNames(wire reflect.Type) []string {
	fields := make([]string, 0, wire.NumField())
	for index := 0; index < wire.NumField(); index++ {
		name := strings.Split(wire.Field(index).Tag.Get("json"), ",")[0]
		switch name {
		case "stream_options":
			name = "stream_options.include_usage"
		case "text":
			name = "text.format"
		}
		fields = append(fields, name)
	}
	sort.Strings(fields)
	return fields
}
