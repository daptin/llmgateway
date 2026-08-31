# llmgateway

`llmgateway` is an embeddable Go engine for authenticated, policy-controlled,
multi-provider LLM inference. It owns protocol normalization, routing, retries,
stream commit semantics, guardrails, caching decisions, and accounting state
transitions. Hosts provide persistence, identity, secrets, distributed counters,
and telemetry through explicit interfaces.

The module intentionally has no dependency on Daptin, Gin, api2go, SQL,
database drivers, or distributed-cache clients. Daptin consumes tagged releases
and supplies those host integrations from its own repository.

The compatibility surface is declared in `compatibility/manifest.json`. An
endpoint or provider is supported only when its manifest entry is backed by its
conformance suite.

The verified provider surface is currently the strict OpenAI-compatible
adapter. Named services are certified separately against the live-provider
matrix before they are added to the manifest; the presence of a compatible
URL alone is not treated as certification.

## Development

```sh
go test ./...
go test -race ./...
```

The example in `examples/basic` proves that the catalog and lifecycle can be
embedded by a service that has no Daptin dependency.
