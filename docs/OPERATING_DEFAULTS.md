# Initial operating profile

These are conservative defaults and qualification targets, not benchmark
claims. They must be revised from measured results on the release reference
machine before v1.0.0.

## Bounds

- Compressed request body: 8 MiB
- Decompressed request body: 16 MiB
- Upstream stream event: 1 MiB
- Buffered first semantic event: 1 MiB
- Maximum upstream attempts including the first: 3
- Maximum fallback depth: 4 models
- Maximum tool definitions: 128
- Maximum bounded attempt records per logical request: 12

## Timeouts

- Dial: 5 seconds
- TLS handshake: 5 seconds
- Response headers / first byte: 30 seconds
- Non-streaming request: 120 seconds
- Stream idle: 60 seconds
- Accounting terminalization: 5 seconds
- Graceful drain: 30 seconds
- Maximum cumulative retry backoff: 5 seconds and at most 20% of the request deadline

## Qualification target

Reference topology: one gateway process and one local PostgreSQL 16 instance on
an 8-core, 16-GiB Linux machine, with a deterministic upstream fault server on
the same network.

- 1,000 concurrent streaming connections
- 250 non-streaming requests/second
- Gateway-only p95 overhead below 15 ms and p99 below 40 ms
- Steady-state memory below 96 KiB per idle/slow stream, excluding payload data
- No more than 20 accounting connections from the process
- Accounting terminalization p99 below 100 ms under the target mix
- Catalog convergence within 5 seconds after SQL commit
- API-key revocation convergence within 5 seconds after SQL commit
- Zero durable hard-budget overspend under concurrent multi-node admission
- Reservation recovery begins within 15 seconds after a dead lease expires

Olric loss may reduce cache hit rate and may introduce at most one fixed-window
counter interval of documented RPM/TPM skew. It must not widen durable token or
monetary budgets.

Provider URLs are HTTPS-only by default. Insecure HTTP and access to loopback,
link-local, or private networks require separate explicit provider opt-ins.
