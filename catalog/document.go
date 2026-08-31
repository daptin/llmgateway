package catalog

import (
	"encoding/json"
	"time"

	"github.com/daptin/llmgateway/contract"
)

type Document struct {
	Revision    uint64
	GeneratedAt time.Time
	Providers   []Provider
	Models      []Model
	Deployments []Deployment
	Policies    []Policy
	Guardrails  []Guardrail
}

type Provider struct {
	ID            contract.ID
	Name          string
	Type          string
	BaseURL       string
	AllowInsecure bool
	SecretRef     string
	Parameters    json.RawMessage
	Enabled       bool
}

type Model struct {
	ID                         contract.ID
	Name                       string
	Operations                 []contract.Operation
	Capabilities               map[string]bool
	FallbackModelIDs           []contract.ID
	DefaultParameters          json.RawMessage
	UnsupportedParameterPolicy string
	PolicyID                   contract.ID
	Enabled                    bool
}

type Deployment struct {
	ID             contract.ID
	Name           string
	ModelID        contract.ID
	ProviderID     contract.ID
	UpstreamModel  string
	Operations     []contract.Operation
	Priority       int
	Weight         int
	RequestTimeout time.Duration
	ConnectTimeout time.Duration
	MaxConcurrency int64
	RPM            int64
	TPM            int64
	Pricing        Pricing
	Parameters     json.RawMessage
	Enabled        bool
}

type Pricing struct {
	InputMicrosPerMillion      int64
	OutputMicrosPerMillion     int64
	CacheReadMicrosPerMillion  int64
	CacheWriteMicrosPerMillion int64
	ReasoningMicrosPerMillion  int64
}

type Policy struct {
	ID     contract.ID
	Name   string
	Limits []Limit
}

type Limit struct {
	Metric  string
	Window  string
	Maximum int64
	Mode    string
}

type Guardrail struct {
	ID        contract.ID
	Name      string
	Kind      string
	Phase     string
	Priority  int
	Config    json.RawMessage
	Timeout   time.Duration
	FailMode  string
	AuditOnly bool
	Enabled   bool
}
