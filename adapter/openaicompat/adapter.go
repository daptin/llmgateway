// Package openaicompat implements a strict OpenAI-compatible upstream adapter.
package openaicompat

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/daptin/llmgateway/adapter"
	"github.com/daptin/llmgateway/catalog"
	"github.com/daptin/llmgateway/contract"
	"github.com/daptin/llmgateway/internal/jsonx"
	"github.com/daptin/llmgateway/internal/netpolicy"
)

const (
	defaultMaxResponseBytes = int64(32 << 20)
	defaultMaxEventBytes    = 1 << 20
)

var providerBaseURLs = map[string]string{
	"openai":     "https://api.openai.com/v1",
	"openrouter": "https://openrouter.ai/api/v1",
	"google":     "https://generativelanguage.googleapis.com/v1beta/openai",
	"lilac":      "https://api.getlilac.com/v1",
}

type Factory struct {
	Transport        http.RoundTripper
	MaxResponseBytes int64
	MaxEventBytes    int
	Now              func() time.Time
}

type providerParameters struct {
	Organization        string `json:"organization,omitempty"`
	Project             string `json:"project,omitempty"`
	ImageGenerationPath string `json:"image_generation_path,omitempty"`
}

type Adapter struct {
	baseURL          *url.URL
	apiKey           []byte
	parameters       providerParameters
	transport        http.RoundTripper
	maxResponseBytes int64
	maxEventBytes    int
	now              func() time.Time
	allowPrivate     bool
	clientsMu        sync.Mutex
	clients          map[time.Duration]*http.Client
	ownedTransports  []*http.Transport
}

func (f Factory) Build(_ context.Context, provider catalog.Provider, secret adapter.Secret) (adapter.Adapter, error) {
	baseURL := strings.TrimSpace(provider.BaseURL)
	if baseURL == "" {
		baseURL = providerBaseURLs[provider.Type]
	}
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("OpenAI-compatible provider requires a valid base URL")
	}
	if parsed.Scheme != "https" && !(parsed.Scheme == "http" && provider.AllowInsecure) {
		return nil, errors.New("OpenAI-compatible provider requires HTTPS unless insecure HTTP is explicit")
	}
	if !provider.AllowPrivateNetwork && netpolicy.PrivateHost(parsed.Hostname()) {
		return nil, errors.New("OpenAI-compatible provider private-network access requires explicit opt-in")
	}
	parameters := providerParameters{}
	if len(bytes.TrimSpace(provider.Parameters)) > 0 && !bytes.Equal(bytes.TrimSpace(provider.Parameters), []byte("{}")) {
		if err := decodeStrict(provider.Parameters, &parameters); err != nil {
			return nil, fmt.Errorf("invalid OpenAI-compatible provider parameters: %w", err)
		}
	}
	if parameters.ImageGenerationPath != "" && parameters.ImageGenerationPath != "/images" && parameters.ImageGenerationPath != "/images/generations" {
		return nil, errors.New("image_generation_path must be /images or /images/generations")
	}
	maximum := f.MaxResponseBytes
	if maximum == 0 {
		maximum = defaultMaxResponseBytes
	}
	frameMaximum := f.MaxEventBytes
	if frameMaximum == 0 {
		frameMaximum = defaultMaxEventBytes
	}
	if maximum < 1 || frameMaximum < 1 {
		return nil, errors.New("OpenAI-compatible response bounds must be positive")
	}
	if f.Now == nil {
		f.Now = time.Now
	}
	key := secret.Bytes()
	if len(key) == 0 {
		return nil, errors.New("OpenAI-compatible provider requires an API key")
	}
	transport := f.Transport
	if transport == nil {
		transport = http.DefaultTransport.(*http.Transport).Clone()
	}
	if _, enforceable := transport.(*http.Transport); !enforceable && !provider.AllowPrivateNetwork {
		return nil, errors.New("custom OpenAI-compatible transport requires explicit private-network opt-in")
	}
	return &Adapter{
		baseURL: parsed, apiKey: key, parameters: parameters, transport: transport,
		maxResponseBytes: maximum, maxEventBytes: frameMaximum, now: f.Now, allowPrivate: provider.AllowPrivateNetwork,
		clients: make(map[time.Duration]*http.Client),
	}, nil
}

func (a *Adapter) Capabilities() adapter.Capabilities {
	return adapter.Capabilities{Operations: map[contract.Operation]bool{
		contract.OperationChat: true, contract.OperationResponses: true,
		contract.OperationEmbeddings: true, contract.OperationImageGeneration: true,
	}, Features: map[string]bool{
		"audio": true, "dimensions": true, "json_schema": true, "logprobs": true,
		"parallel_tools": true, "penalties": true, "reasoning": true, "streaming": true,
		"token_ids": true, "tools": true, "vision": true,
	}}
}

func (a *Adapter) ValidateDeployment(deployment catalog.Deployment) error {
	if _, err := parseDeploymentParameters(deployment.Parameters); err != nil {
		return err
	}
	if deployment.HealthCheck.Path != "" {
		parsed, err := url.Parse(deployment.HealthCheck.Path)
		if err != nil || !strings.HasPrefix(parsed.Path, "/") || parsed.IsAbs() || parsed.Host != "" || parsed.RawQuery != "" || parsed.Fragment != "" || strings.Contains(parsed.Path, "..") {
			return errors.New("health check path must be an absolute URL path without traversal, query, or fragment")
		}
	}
	if strings.Contains(deployment.HealthCheck.Path, "{model}") && strings.TrimSpace(deployment.HealthCheck.Model) == "" {
		return errors.New("health check path uses {model} without a health check model")
	}
	return nil
}

func (a *Adapter) Invoke(ctx context.Context, deployment catalog.Deployment, request contract.Request) (contract.Response, error) {
	if request.Stream {
		return contract.Response{}, invalidRequest("streaming requests must use Stream", nil)
	}
	body, path, err := encodeRequest(deployment, request)
	if err != nil {
		return contract.Response{}, err
	}
	response, err := a.do(ctx, deployment, path, body)
	if err != nil {
		return contract.Response{}, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return contract.Response{}, a.providerError(response)
	}
	payload, err := readBounded(response.Body, a.maxResponseBytes)
	if err != nil {
		return contract.Response{}, providerFailure("upstream response exceeded the configured bound", err)
	}
	decoded, err := decodeResponse(request.Operation, payload)
	if err != nil {
		return contract.Response{}, err
	}
	switch request.Operation {
	case contract.OperationChat:
		if request.Chat.N > 0 && len(decoded.Chat.Choices) != request.Chat.N {
			return contract.Response{}, providerFailure("upstream returned the wrong number of chat choices", nil)
		}
	case contract.OperationEmbeddings:
		expected := len(request.Embeddings.Input.Texts) + len(request.Embeddings.Input.Tokens)
		if len(decoded.Embeddings.Data) != expected {
			return contract.Response{}, providerFailure("upstream returned the wrong number of embeddings", nil)
		}
	case contract.OperationImageGeneration:
		if request.ImageGeneration.N > 0 && len(decoded.ImageGeneration.Data) != request.ImageGeneration.N {
			return contract.Response{}, providerFailure("upstream returned the wrong number of images", nil)
		}
	}
	return decoded, nil
}

func (a *Adapter) Stream(ctx context.Context, deployment catalog.Deployment, request contract.Request) (adapter.Stream, error) {
	if !request.Stream || (request.Operation != contract.OperationChat && request.Operation != contract.OperationResponses) {
		return nil, invalidRequest("operation does not support streaming", nil)
	}
	body, path, err := encodeRequest(deployment, request)
	if err != nil {
		return nil, err
	}
	response, err := a.do(ctx, deployment, path, body)
	if err != nil {
		return nil, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		defer response.Body.Close()
		return nil, a.providerError(response)
	}
	if !strings.HasPrefix(strings.ToLower(response.Header.Get("Content-Type")), "text/event-stream") {
		response.Body.Close()
		return nil, providerFailure("upstream returned a non-stream response", nil)
	}
	return newEventStream(response.Body, request.Operation, a.maxEventBytes), nil
}

func (a *Adapter) HealthCheck(ctx context.Context, deployment catalog.Deployment) error {
	endpoint := *a.baseURL
	basePath := strings.TrimRight(endpoint.Path, "/")
	baseRawPath := strings.TrimRight(endpoint.EscapedPath(), "/")
	path, rawPath := deployment.HealthCheck.Path, ""
	if path == "" {
		path, rawPath = "/models", "/models"
		if deployment.HealthCheck.Model != "" {
			path += "/" + deployment.HealthCheck.Model
			rawPath += "/" + url.PathEscape(deployment.HealthCheck.Model)
		}
	} else {
		parsed, _ := url.Parse(path) // ValidateDeployment has already accepted this path.
		path, rawPath = parsed.Path, parsed.EscapedPath()
		if deployment.HealthCheck.Model != "" {
			path = strings.ReplaceAll(path, "{model}", deployment.HealthCheck.Model)
			rawPath = strings.ReplaceAll(rawPath, "%7Bmodel%7D", url.PathEscape(deployment.HealthCheck.Model))
		}
	}
	endpoint.Path = basePath + path
	endpoint.RawPath = baseRawPath + rawPath
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return providerFailure("failed to construct upstream health probe", err)
	}
	a.setHeaders(request, "application/json")
	response, err := a.clientFor(deployment.ConnectTimeout).Do(request)
	if err != nil {
		return providerFailure("upstream health probe failed", err)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return providerFailure("upstream health probe returned an unsuccessful status", nil)
	}
	return nil
}

func (a *Adapter) do(ctx context.Context, deployment catalog.Deployment, path string, body []byte) (*http.Response, error) {
	if err := a.ValidateDeployment(deployment); err != nil {
		return nil, invalidRequest(err.Error(), err)
	}
	if path == "/images/generations" && a.parameters.ImageGenerationPath != "" {
		path = a.parameters.ImageGenerationPath
	}
	endpoint := *a.baseURL
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + path
	requestContext := ctx
	cancel := func() {}
	if deployment.RequestTimeout > 0 {
		requestContext, cancel = context.WithTimeout(ctx, deployment.RequestTimeout)
	}
	request, err := http.NewRequestWithContext(requestContext, http.MethodPost, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		cancel()
		return nil, invalidRequest("failed to construct upstream request", err)
	}
	a.setHeaders(request, "application/json")
	request.Header.Set("Content-Type", "application/json")
	response, err := a.clientFor(deployment.ConnectTimeout).Do(request)
	if err != nil {
		cancel()
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(requestContext.Err(), context.DeadlineExceeded) {
			return nil, &contract.Error{Code: contract.ErrorTimeout, Message: "upstream provider timed out", HTTPStatus: http.StatusGatewayTimeout, Retryable: true, Cause: err}
		}
		if errors.Is(err, context.Canceled) {
			return nil, &contract.Error{Code: contract.ErrorProvider, Message: "upstream request cancelled", HTTPStatus: 499, Retryable: false, Cause: err}
		}
		return nil, providerFailure("upstream provider failed", err)
	}
	response.Body = &cancelReadCloser{ReadCloser: response.Body, cancel: cancel}
	return response, nil
}

func (a *Adapter) setHeaders(request *http.Request, accept string) {
	request.Header.Set("Authorization", "Bearer "+string(a.apiKey))
	request.Header.Set("Accept", accept)
	if a.parameters.Organization != "" {
		request.Header.Set("OpenAI-Organization", a.parameters.Organization)
	}
	if a.parameters.Project != "" {
		request.Header.Set("OpenAI-Project", a.parameters.Project)
	}
}

type cancelReadCloser struct {
	io.ReadCloser
	cancel    context.CancelFunc
	closeOnce sync.Once
	closeErr  error
}

func (c *cancelReadCloser) Close() error {
	c.closeOnce.Do(func() {
		c.closeErr = c.ReadCloser.Close()
		c.cancel()
	})
	return c.closeErr
}

func (a *Adapter) clientFor(connectTimeout time.Duration) *http.Client {
	a.clientsMu.Lock()
	defer a.clientsMu.Unlock()
	if client := a.clients[connectTimeout]; client != nil {
		return client
	}
	transport := a.transport
	if base, ok := a.transport.(*http.Transport); ok {
		clone := netpolicy.CloneTransport(base, connectTimeout, a.allowPrivate)
		if connectTimeout > 0 {
			clone.TLSHandshakeTimeout = connectTimeout
			clone.ResponseHeaderTimeout = connectTimeout
		}
		transport = clone
		a.ownedTransports = append(a.ownedTransports, clone)
	}
	client := &http.Client{Transport: transport, CheckRedirect: netpolicy.RejectRedirect}
	a.clients[connectTimeout] = client
	return client
}

func (a *Adapter) CloseIdleConnections() {
	a.clientsMu.Lock()
	transports := a.ownedTransports
	a.ownedTransports = nil
	a.clients = make(map[time.Duration]*http.Client)
	a.clientsMu.Unlock()
	for _, transport := range transports {
		transport.CloseIdleConnections()
	}
}

func readBounded(reader io.Reader, maximum int64) ([]byte, error) {
	limited := io.LimitReader(reader, maximum+1)
	payload, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	if int64(len(payload)) > maximum {
		return nil, errors.New("response body is too large")
	}
	return payload, nil
}

func decodeStrict(payload []byte, destination any) error {
	return jsonx.DecodeOne(bytes.NewReader(payload), destination)
}
