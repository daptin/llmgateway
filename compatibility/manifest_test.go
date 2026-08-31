package compatibility

import (
	"strings"
	"testing"
)

func TestDefaultManifestIsValid(t *testing.T) {
	manifest, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Endpoints) != 6 {
		t.Fatalf("expected 6 endpoints, got %d", len(manifest.Endpoints))
	}
	if manifest.Kind != "verified" || len(manifest.Providers) != 1 || manifest.Providers[0].Name != "openai-compatible" {
		t.Fatalf("manifest overstates implemented provider coverage: %+v", manifest.Providers)
	}
}

func TestDecodeRejectsUnknownFields(t *testing.T) {
	_, err := Decode(strings.NewReader(`{"version":"1","kind":"target","comparison":{"litellm":"1","openai":{"go":"1"}},"unknown":true}`))
	if err == nil {
		t.Fatal("expected unknown field rejection")
	}
}

func TestValidateRejectsDuplicateEndpoint(t *testing.T) {
	manifest, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	manifest.Endpoints = append(manifest.Endpoints, manifest.Endpoints[0])
	if err := manifest.Validate(); err == nil {
		t.Fatal("expected duplicate endpoint rejection")
	}
}
