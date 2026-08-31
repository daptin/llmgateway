// Package openaicompat implements a strict OpenAI-compatible upstream adapter.
package openaicompat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/daptin/llmgateway/adapter"
	"github.com/daptin/llmgateway/catalog"
	"github.com/daptin/llmgateway/contract"
)

const (
	defaultMaxResponseBytes = int64(32 << 20)
	defaultMaxEventBytes    = 1 << 20
)

type Factory struct {
	Client           *http.Client
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
	client           *http.Client
	transport        http.RoundTripper
	maxResponseBytes int64
	maxEventBytes    int
	now              func() time.Time
	allowPrivate     bool
	clientsMu        sync.Mutex
	clients          map[time.Duration]*http.Client
}

func (f Factory) Build(_ context.Context, provider catalog.Provider, secret adapter.Secret) (adapter.Adapter, error) {
	parsed, err := url.Parse(provider.BaseURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, errors.New("OpenAI-compatible provider requires a valid base URL")
	}
	if parsed.Scheme != "https" && !(parsed.Scheme == "http" && provider.AllowInsecure) {
		return nil, errors.New("OpenAI-compatible provider requires HTTPS unless insecure HTTP is explicit")
	}
	if !provider.AllowPrivateNetwork && privateHost(parsed.Hostname()) {
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
	client := f.Client
	if client != nil {
		copy := *client
		copy.CheckRedirect = rejectRedirect
		client = &copy
	}
	return &Adapter{
		baseURL: parsed, apiKey: key, parameters: parameters, client: client, transport: transport,
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
		"streaming": true, "token_ids": true, "tools": true, "vision": true,
	}}
}

func (a *Adapter) ValidateDeployment(deployment catalog.Deployment) error {
	if len(bytes.TrimSpace(deployment.Parameters)) > 0 && !bytes.Equal(bytes.TrimSpace(deployment.Parameters), []byte("{}")) {
		return errors.New("OpenAI-compatible deployment parameters are not supported")
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
	return decodeResponse(request.Operation, payload)
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
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + "/models"
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
	cancel context.CancelFunc
	once   sync.Once
}

func (c *cancelReadCloser) Close() error {
	err := c.ReadCloser.Close()
	c.once.Do(c.cancel)
	return err
}

func (a *Adapter) clientFor(connectTimeout time.Duration) *http.Client {
	if a.client != nil {
		return a.client
	}
	a.clientsMu.Lock()
	defer a.clientsMu.Unlock()
	if client := a.clients[connectTimeout]; client != nil {
		return client
	}
	transport := a.transport
	if base, ok := a.transport.(*http.Transport); ok {
		clone := base.Clone()
		clone.DialContext = safeDialContext(connectTimeout, a.allowPrivate)
		if connectTimeout > 0 {
			clone.TLSHandshakeTimeout = connectTimeout
			clone.ResponseHeaderTimeout = connectTimeout
		}
		transport = clone
	}
	client := &http.Client{Transport: transport, CheckRedirect: rejectRedirect}
	a.clients[connectTimeout] = client
	return client
}

func rejectRedirect(_ *http.Request, _ []*http.Request) error {
	return http.ErrUseLastResponse
}

func safeDialContext(timeout time.Duration, allowPrivate bool) func(context.Context, string, string) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second}
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		if allowPrivate {
			return dialer.DialContext(ctx, network, address)
		}
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, errors.New("upstream address is invalid")
		}
		addresses, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, err
		}
		for _, candidate := range addresses {
			if privateIP(candidate.IP) {
				continue
			}
			connection, dialErr := dialer.DialContext(ctx, network, net.JoinHostPort(candidate.IP.String(), port))
			if dialErr == nil {
				return connection, nil
			}
			err = dialErr
		}
		if err != nil {
			return nil, err
		}
		return nil, errors.New("upstream resolved only to private-network addresses")
	}
}

func privateHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return privateIP(ip)
	}
	return false
}

func privateIP(ip net.IP) bool {
	carrierGradeNAT := len(ip) > 0 && ip.To4() != nil && ip.To4()[0] == 100 && ip.To4()[1]&0xc0 == 0x40
	return !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || carrierGradeNAT
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
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("expected one JSON document")
	}
	return nil
}
