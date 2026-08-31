package guardrail

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"

	"github.com/daptin/llmgateway/catalog"
	"github.com/daptin/llmgateway/contract"
)

type HTTPFactory struct {
	Client           *http.Client
	MaxResponseBytes int64
}

type httpConfig struct {
	Endpoint      string `json:"endpoint"`
	AllowInsecure bool   `json:"allow_insecure,omitempty"`
}

type httpChecker struct {
	client   *http.Client
	endpoint string
	maximum  int64
}

func (f HTTPFactory) Build(configuration catalog.Guardrail) (Checker, error) {
	var config httpConfig
	if err := strictConfig(configuration.Config, &config); err != nil {
		return nil, err
	}
	endpoint, err := url.Parse(config.Endpoint)
	if err != nil || endpoint.Scheme == "" || endpoint.Host == "" || endpoint.User != nil {
		return nil, errors.New("HTTP guardrail requires a fixed absolute endpoint without user info")
	}
	if endpoint.Scheme != "https" && !(endpoint.Scheme == "http" && config.AllowInsecure) {
		return nil, errors.New("HTTP guardrail requires HTTPS unless insecure HTTP is explicit")
	}
	if f.Client == nil {
		f.Client = http.DefaultClient
	}
	if f.MaxResponseBytes == 0 {
		f.MaxResponseBytes = 64 << 10
	}
	if f.MaxResponseBytes < 1 {
		return nil, errors.New("HTTP guardrail response bound must be positive")
	}
	return httpChecker{client: f.Client, endpoint: endpoint.String(), maximum: f.MaxResponseBytes}, nil
}

func (c httpChecker) CheckInput(ctx context.Context, request contract.Request) (Decision, error) {
	return c.check(ctx, request, "input", requestText(request))
}

func (c httpChecker) CheckOutput(ctx context.Context, request contract.Request, response contract.Response) (Decision, error) {
	return c.check(ctx, request, "output", responseText(response))
}

func (c httpChecker) CheckStream(ctx context.Context, request contract.Request, event contract.StreamEvent) (Decision, error) {
	return c.check(ctx, request, "stream", eventText(event))
}

func (httpChecker) SupportsStreaming() bool { return false }
func (httpChecker) CacheStable() bool       { return false }

func (c httpChecker) check(ctx context.Context, request contract.Request, phase, text string) (Decision, error) {
	payload, err := json.Marshal(map[string]any{"request_id": request.ID, "operation": request.Operation, "phase": phase, "text": text})
	if err != nil {
		return Decision{}, err
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(payload))
	if err != nil {
		return Decision{}, err
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	response, err := c.client.Do(httpRequest)
	if err != nil {
		return Decision{}, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, c.maximum))
		return Decision{}, errors.New("HTTP guardrail returned a non-success status")
	}
	limited := io.LimitReader(response.Body, c.maximum+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return Decision{}, err
	}
	if int64(len(body)) > c.maximum {
		return Decision{}, errors.New("HTTP guardrail response is too large")
	}
	var decision Decision
	if err := strictConfig(body, &decision); err != nil {
		return Decision{}, err
	}
	return decision, nil
}
