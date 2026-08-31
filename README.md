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

The current manifest is a target contract, not a verified parity claim. Named
services become certified only after their live-provider and conformance gates
pass; the presence of a compatible URL alone is not certification.

Model defaults are compiled once with the catalog and use operation-scoped JSON
so fields with similar names cannot collide:

```json
{
  "chat": {"temperature": 0, "max_completion_tokens": 512},
  "responses": {"max_output_tokens": 512},
  "embeddings": {"encoding_format": "float"},
  "image_generation": {"n": 1, "response_format": "b64_json"}
}
```

Unknown, invalid, or undeclared-operation defaults reject the whole snapshot.
Explicit request values always take precedence. Only the
`priority_weighted` routing strategy and `reject` unsupported-parameter policy
are currently accepted; other values fail catalog compilation instead of being
silently ignored.

## Development

```sh
go test ./...
go test -race ./...
```

The example in `examples/basic` proves that the catalog and lifecycle can be
embedded by a service that has no Daptin dependency.
