# llmgateway

`llmgateway` is an embeddable Go engine for authenticated, host-metered,
multi-provider LLM inference. It owns protocol normalization, routing, retries,
stream commit semantics, guardrails, caching decisions, and provider-neutral
usage facts. Hosts provide metering policy and persistence, identity, secrets,
distributed counters, and telemetry through explicit interfaces.

The module intentionally has no dependency on Daptin, Gin, api2go, SQL,
database drivers, or distributed-cache clients. Daptin consumes tagged releases
and supplies those host integrations from its own repository.

The compatibility surface is declared in `compatibility/manifest.json`. An
endpoint or provider is supported only when its manifest entry is backed by its
conformance suite.

The current manifest is the target module contract. Declared wire fields and
operations require conformance coverage, and named services additionally
require their live-provider matrix before the manifest can be marked verified.

Model defaults are compiled once with the catalog and use operation-scoped JSON
so fields with similar names cannot collide:

```json
{
  "chat": {"temperature": 0, "max_completion_tokens": 512},
  "text_completion": {"temperature": 0, "max_tokens": 512},
  "responses": {"max_output_tokens": 512},
  "embeddings": {"encoding_format": "float"},
  "image_generation": {"n": 1, "response_format": "b64_json"}
}
```

Unknown, invalid, or undeclared-operation defaults reject the whole snapshot.
Explicit request values always take precedence. The routing strategy is
`priority_weighted`. Unsupported-parameter policy is explicit: `reject` fails,
`drop` removes only non-semantic optional fields, and `passthrough` preserves
typed manifest fields only when an eligible adapter declares support. Unknown
wire fields are never accepted as arbitrary passthrough data.

## Development

```sh
go test ./...
go test -race ./...
```

The example in `examples/basic` proves that the catalog and lifecycle can be
embedded by a service that has no Daptin dependency.
