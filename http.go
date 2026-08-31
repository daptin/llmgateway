package llmgateway

import (
	"encoding/json"
	"net/http"

	"github.com/daptin/llmgateway/protocol/openai"
)

// HTTPOptions configures the strict OpenAI-compatible HTTP surface. The
// authenticator is supplied here so hosts that invoke the engine directly do
// not need to configure an unused HTTP authentication path.
type HTTPOptions struct {
	Authenticator Authenticator
	Protocol      openai.Options
}

// Handler exposes the engine's supported OpenAI-compatible HTTP routes.
func (e *Engine) Handler(options HTTPOptions) (http.Handler, error) {
	protocolHandler, err := openai.NewHandler(e, options.Authenticator, options.Protocol)
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.Handle("/v1/", protocolHandler)
	mux.HandleFunc("GET /healthz", func(response http.ResponseWriter, _ *http.Request) {
		writeStatus(response, http.StatusOK, e.Status())
	})
	mux.HandleFunc("GET /readyz", func(response http.ResponseWriter, _ *http.Request) {
		status := e.Status()
		code := http.StatusOK
		if !status.Ready || status.Degraded {
			code = http.StatusServiceUnavailable
		}
		writeStatus(response, code, status)
	})
	return mux, nil
}

func writeStatus(response http.ResponseWriter, code int, status Status) {
	response.Header().Set("Content-Type", "application/json")
	response.Header().Set("Cache-Control", "no-store")
	response.WriteHeader(code)
	_ = json.NewEncoder(response).Encode(status)
}
