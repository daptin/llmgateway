package compatibility

import (
	_ "embed"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/daptin/llmgateway/internal/jsonx"
)

type Support string

const (
	Native     Support = "native"
	Translated Support = "translated"
	Ignored    Support = "ignored"
	Rejected   Support = "rejected"
)

type Manifest struct {
	Version         string                `json:"version"`
	Kind            string                `json:"kind"`
	Comparison      Comparison            `json:"comparison"`
	Endpoints       []Endpoint            `json:"endpoints"`
	Unsupported     []UnsupportedEndpoint `json:"unsupported_endpoints"`
	Providers       []Provider            `json:"providers"`
	ErrorCodes      []string              `json:"error_codes"`
	StreamingEvents map[string]Support    `json:"streaming_events"`
}

type Comparison struct {
	LiteLLM string            `json:"litellm"`
	OpenAI  map[string]string `json:"openai"`
}

type Endpoint struct {
	Method         string             `json:"method"`
	Path           string             `json:"path"`
	RequestFields  map[string]Support `json:"request_fields"`
	ResponseFields map[string]Support `json:"response_fields"`
}

type UnsupportedEndpoint struct {
	Method string `json:"method"`
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

type Provider struct {
	Name       string             `json:"name"`
	Operations map[string]Support `json:"operations"`
}

//go:embed manifest.json
var defaultBytes []byte

func Default() (Manifest, error) {
	return Decode(strings.NewReader(string(defaultBytes)))
}

func Decode(reader io.Reader) (Manifest, error) {
	var manifest Manifest
	if err := jsonx.DecodeOne(reader, &manifest); err != nil {
		return Manifest{}, err
	}
	if err := manifest.Validate(); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

func (m Manifest) Validate() error {
	if strings.TrimSpace(m.Version) == "" || strings.TrimSpace(m.Comparison.LiteLLM) == "" {
		return errors.New("manifest version and pinned LiteLLM version are required")
	}
	if m.Kind != "target" && m.Kind != "verified" {
		return fmt.Errorf("manifest kind must be target or verified, got %q", m.Kind)
	}
	if len(m.Comparison.OpenAI) == 0 {
		return errors.New("at least one pinned OpenAI SDK is required")
	}
	endpoints := make(map[string]struct{}, len(m.Endpoints))
	for _, endpoint := range m.Endpoints {
		key := strings.ToUpper(endpoint.Method) + " " + endpoint.Path
		if endpoint.Method == "" || !strings.HasPrefix(endpoint.Path, "/") {
			return fmt.Errorf("invalid endpoint %q", key)
		}
		if _, exists := endpoints[key]; exists {
			return fmt.Errorf("duplicate endpoint %q", key)
		}
		endpoints[key] = struct{}{}
		if endpoint.RequestFields == nil || endpoint.ResponseFields == nil {
			return fmt.Errorf("endpoint %q must declare request_fields and response_fields", key)
		}
		for direction, fields := range map[string]map[string]Support{"request": endpoint.RequestFields, "response": endpoint.ResponseFields} {
			for field, support := range fields {
				if strings.TrimSpace(field) == "" || !validSupport(support) {
					return fmt.Errorf("endpoint %q has invalid %s field support for %q", key, direction, field)
				}
			}
		}
	}
	for _, endpoint := range m.Unsupported {
		key := strings.ToUpper(endpoint.Method) + " " + endpoint.Path
		if endpoint.Method == "" || !strings.HasPrefix(endpoint.Path, "/") || strings.TrimSpace(endpoint.Reason) == "" {
			return fmt.Errorf("invalid unsupported endpoint %q", key)
		}
		if _, exists := endpoints[key]; exists {
			return fmt.Errorf("duplicate endpoint %q", key)
		}
		endpoints[key] = struct{}{}
	}
	providers := make(map[string]struct{}, len(m.Providers))
	for _, provider := range m.Providers {
		if strings.TrimSpace(provider.Name) == "" {
			return errors.New("provider name is required")
		}
		if _, exists := providers[provider.Name]; exists {
			return fmt.Errorf("duplicate provider %q", provider.Name)
		}
		providers[provider.Name] = struct{}{}
		for operation, support := range provider.Operations {
			if strings.TrimSpace(operation) == "" || !validSupport(support) {
				return fmt.Errorf("provider %q has invalid operation support for %q", provider.Name, operation)
			}
		}
	}
	for event, support := range m.StreamingEvents {
		if strings.TrimSpace(event) == "" || !validSupport(support) {
			return fmt.Errorf("invalid streaming event support for %q", event)
		}
	}
	return nil
}

func validSupport(value Support) bool {
	switch value {
	case Native, Translated, Ignored, Rejected:
		return true
	default:
		return false
	}
}
