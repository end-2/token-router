# Proxy performance optimization history

This document is a historical record of a proxy overhead review. Current behavior is described in [Proxy server architecture](../architecture/proxy-server.md), and repeatable measurement procedures are in [Proxy performance measurement](../development/performance.md).

- Recorded: 2026-10-11
- Baseline commit: [`14b723d8531717dfcd9430780d3722e0bf6ef9ff`](https://github.com/end-2/token-router/commit/14b723d8531717dfcd9430780d3722e0bf6ef9ff)
- Scope: request forwarding, routing, response usage observation, account identification, local rate limiting, and observability

## Response copying and flushing

The baseline allocated a response copy buffer for each request. [`proxy.New`](../../src/internal/proxy/proxy.go) now supplies a shared `httputil.BufferPool` backed by `sync.Pool`. Each buffer has a fixed 32 KiB capacity, and pointer storage avoids allocating a slice header on return to the pool. Concurrent responses borrow separate buffers. Buffer retention is managed by the Go runtime and is not a permanent reservation.

The baseline also set `FlushInterval` to a negative value for every response. The proxy now uses the standard flush policy, allowing known-length ordinary responses to use HTTP write buffering. SSE and unknown-length responses still flush immediately. This reduces forced writes for ordinary responses without delaying SSE events. `EnableFullDuplex` still allows response writes while the upstream transport is reading the incoming body.

The behavior follows Go 1.27.2 [`net/http/httputil/reverseproxy.go`](https://github.com/golang/go/blob/go1.27.2/src/net/http/httputil/reverseproxy.go), specifically `ReverseProxy.flushInterval`, `ReverseProxy.copyResponse`, and `ReverseProxy.copyBuffer`.

## Routing and header rewriting

[`routing.Router.SelectTarget`](../../src/internal/proxy/routing/routing.go) bypasses Host parsing when only the default pool is configured. Pools containing one destination bypass the round-robin atomic increment; pools with multiple destinations retain atomic round robin. Authorities without a colon avoid `net.SplitHostPort` and its error allocation.

[`proxy.New`](../../src/internal/proxy/proxy.go) constructs a configured authorization value once per proxy generation. Forwarding header removal compares prefixes without allocating lowercase copies for each request header. Header removal remains case insensitive.

Model routing still reads bounded request bodies, validates the JSON object, and selects the exact `model` key. The optimization does not replace this validation with a partial JSON parser.

## Response usage observation

[`usageEvents.feed`](../../src/internal/proxy/usage/usage.go) appends line segments in batches instead of processing and appending each byte individually. CR, LF, CRLF, fragmented reads, event size bounds, and event dispatch remain part of the parser's contract.

Content events without a possible non-null `usage` value bypass JSON decoding. The precheck recognizes case folding, continues scanning after null values for duplicate or nested usage, and falls back to the JSON decoder when strings contain escapes. Missing and null usage objects return before token field decoding. Integer token counts use `strconv.ParseInt` after the containing JSON has been validated, retaining signed 64-bit range checks and rejection of negative values, fractional values, strings, and overflow.

Ineligible requests no longer receive an unnecessary usage context value. Eligible generation responses retain usage accounting on completion and after client cancellation, including protection against duplicate usage records.

## Account identification

[`accountIdentifier`](../../src/internal/app/account.go) reads `Authorization` values once, splits fields with `strings.FieldsSeq`, and separates JWT segments with `strings.Cut`. These operations avoid temporary field and segment slices. Duplicate headers, extra fields, invalid JWT segments, claim validation, and identifier validation retain their existing rejection behavior. Authentication remains the responsibility of the trusted authentication layer.

## Rate limiting

The baseline local memory limiter serialized all accounts behind one mutex. [`memory.Limiter`](../../src/internal/bucket/memory/memory.go) partitions account states into sixteen shards selected from the already hashed account key. Each account still checks request and token balances under one mutex, while accounts in different shards can proceed concurrently. The global bucket retains a single shared balance.

`Update` and `Close` lock every shard in a fixed order so changes remain atomic across accounts. Periodic reclamation scans shards individually and removes only fully replenished states. Admission still uses reservations and cancellation to preserve retry rounding and avoid deducting request capacity for rejected calls.

[`distributed.Limiter.submit`](../../src/internal/bucket/distributed/distributed.go) returns before constructing a timeout context when usage accounting is a no-op. Redis atomic scripts, mutation retry settings, Raft command ordering, and majority commit requirements retain their existing behavior. Network and consensus latency remain costs of the shared backends.

## Observability

[`Telemetry.Middleware`](../../src/internal/observability/telemetry/http.go) bypasses timing and attribute construction when tracing, OTLP metrics, and access logging are inactive. Disabled OTLP metrics no longer construct attributes or invoke no-op instruments. The fast path still invokes Echo's error handler when the next handler returns an error.

Access logs use typed `slog.Attr` values and are constructed only when the logger accepts the `info` level. Enabled OTLP metric observations use attribute sets, while fixed rate limiter outcomes reuse their metric options. Prometheus rate limiter counters are resolved once per outcome, retaining lazy series creation.

Upstream tracing copies the request and headers needed for context propagation without cloning unrelated URL and request data. Unsampled spans still propagate trace context but do not wrap response bodies solely to end a non-recording span. Sampled spans retain response-body lifetime tracking.

## Validation scope

The changes include benchmarks for the application request pipeline and individual proxy, rate limiter, account, and observability paths. Correctness checks cover forwarding, routing, full duplex reads, streaming and cancellation, usage extraction and bounds, policy updates, metrics, and trace propagation. The commands for unit tests, race detection, lint, and Docker-based E2E scenarios are maintained in [Proxy server development and execution](../development/proxy-server.md#validation).

Performance comparisons must use equivalent inputs and build conditions.
