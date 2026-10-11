# Proxy server architecture

## Package boundaries

The Go module is at the repository root, and application code lives under `src/`.

| Path | Responsibility |
| --- | --- |
| [`src/cmd/token-router`](../../src/cmd/token-router/main.go) | Shutdown signals and exit codes |
| [`src/internal/cli`](../../src/internal/cli/root.go) | Cobra commands and explicit argument collection |
| [`src/internal/config`](../../src/internal/config/config.go) | Configuration layer merging, type conversion, and validation |
| [`src/internal/app`](../../src/internal/app/app.go) | Dependency construction, Echo routing, and listener lifecycles |
| [`src/internal/proxy`](../../src/internal/proxy/proxy.go) | Host and model routing, standard `httputil.ReverseProxy`, upstream connection pools, and OpenAI response usage observation |
| [`src/internal/bucket`](../../src/internal/bucket/bucket.go) | The `Limiter` interface and local memory, distributed memory, and Redis implementations |
| [`src/internal/observability`](../../src/internal/observability/metrics.go) | Per-instance Prometheus registries, OpenTelemetry providers, and JSON logs |

`main` converts process signals into a context and runs the CLI. The CLI loads configuration and calls `app.Run`. `app` assembles the implementations; request processing code does not read configuration files or environment variables directly.

## HTTP proxy

Echo routes `/v1` and `/v1/*` to the proxy. It forwards the method, body, query, and upstream status code. SSE responses are flushed without buffering the entire response, and client disconnections propagate to the upstream context. When token limiting is enabled, successful `POST` JSON and SSE responses on supported generation paths are inspected for OpenAI-format `usage`, which is passed to the usage recording function. Retrieval and metadata updates are not counted. Forwarded request and response bodies remain unchanged.

[`proxy.Handler.ServeHTTP`](../../src/internal/proxy/proxy.go) uses `http.ResponseController.EnableFullDuplex` on HTTP/1.x to allow request body reads and response writes concurrently. This prevents response flushing from closing the incoming body while the upstream transport is still reading it.

[`proxy.router`](../../src/internal/proxy/routing.go) selects a pool from configured destinations using the request `Host` and the JSON body's `model`. Rules with more conditions take precedence, and address arrays with identical conditions are merged into one pool. Each pool's atomic counter distributes concurrent requests round robin. The selected destination is stored in the request context and applied in `ReverseProxy.Rewrite`; all destinations share one transport. Unmatched requests use the default pool, or return `404` if none exists. See [Multiple upstream routing](../reference/configuration.md#multiple-upstream-routing) for configuration and precedence.

When model conditions apply to the request host, the JSON request body is read within `upstream.max_routing_body_bytes` and forwarded unchanged. Requests using only Host conditions and non-JSON requests are not read in advance. If the context is canceled during model inspection, a read deadline stops body reception. SSE forwarding and usage accounting use the same path regardless of the selected destination.

`Forwarded` and `X-Forwarded-*` are removed, and process environment variables such as `HTTP_PROXY` do not alter destination routing. No application-level retries or redirect following are added. The standard `http.Transport` may retry replayable requests after a reused connection fails. WebSocket and HTTP protocol upgrades are not supported.

Connection errors before a response starts become `502`, and timeouts become `504`. Upstream API errors and status codes are forwarded unchanged. Transmission errors after a response starts close the connection. Proxy-generated errors and logs do not include upstream URLs, API keys, or request bodies.

## Backend interface

`bucket.Limiter` provides these operations:

```go
type Limiter interface {
    Allow(context.Context) (Decision, error)
    RecordUsage(context.Context, int64) error
    Close() error
}
```

`Decision` contains the admission result and retry delay. `bucket.New` selects an implementation from configuration. Local memory uses `golang.org/x/time/rate` for request counts and separately maintains token usage balances, checking both limits under the same mutex. Redis uses `go-redis` and atomic Lua scripts. Automatic Redis mutation retries are disabled to avoid duplicate deductions when a request's outcome is unknown.

`Allow` checks both the request balance and any active token usage balance, deducting only one request unit on admission. `RecordUsage` separately deducts model tokens observed in responses and allows negative balances. Usage is neither estimated nor reserved before requests, so requests in progress can exceed the limit. See [Request count and token usage limits](../reference/configuration.md#request-count-and-token-usage-limits) for extraction rules and constraints.

To add a bucket backend, implement this interface and add configuration validation and a factory branch. With account limiting disabled, all API requests share a global request bucket and an optional token usage bucket. With account limiting enabled, `app.accountIdentifier` extracts an identifier from the incoming `Authorization`, and `bucket.WithAccount` places its hash in the context. `Allow` and `RecordUsage` use the account key from the same context.

Distributed memory replicates per-account balances with `hashicorp/raft`. Followers send internal HTTP requests to the leader, which returns results after recording commands on a majority. `bucket.AdminBackend` registers internal routes on the admin listener, and the Raft `StreamLayer` uses HTTP CONNECT on the same port. See [Distributed memory rate limiting](../operations/distributed-rate-limiting.md) for snapshots, persistent logs, and failure behavior.

`bucket.New` returns `Reconfigurable`, which adds `Update(config.LimitValues)` to `Limiter`. This method accepts validated limit values and account settings and updates them while preserving the backend and existing balances. New bucket implementations must also provide this method.

## Policy configuration

[`app/policy.go`](../../src/internal/app/policy.go) handles file content watching, the admin API, and configuration replacement. `updateMu` serializes file reads, API merges, and persistence, while `policyMu` provides consistent configuration for admission and upstream selection. Limits and the proxy are applied together after proxy construction and file persistence succeed. Streams do not hold the configuration lock and continue using their selected proxy. Replaced proxies close idle connections immediately and clean up again when their last active request finishes.

The admin API serves configuration only on an optional separate listener. Both files and the API allow all `upstream` settings, rate limit values, and account settings; the backend, listeners, distributed memory, Redis, and telemetry remain fixed. See [Policy configuration and admin API](../operations/policy-configuration.md) for file merging and API persistence rules.

## Metrics and health checks

| Path or metric | Meaning |
| --- | --- |
| `/healthz` | Liveness of the process HTTP handler |
| `/readyz` | HTTP listener startup and shutdown status; no continuous health checks for upstreams, Redis, or the Raft majority |
| `token_router_http_requests_total` | Completed requests, labeled by `method`, `route`, and `status` |
| `token_router_http_request_duration_seconds` | Request duration, including the entire SSE lifetime |
| `token_router_http_requests_in_flight` | Requests currently being processed |
| `token_router_rate_limit_decisions_total` | Bucket calls by `allowed`, `denied`, and `error` results |
| `go_*`, `process_*` | Default Prometheus Go runtime and process information |

`route` is a registered route such as `/v1/*`; unmatched paths are recorded as `unmatched`. Unknown HTTP methods are recorded as `OTHER` to prevent arbitrary input from increasing label cardinality. API keys, user identifiers, and raw URLs are excluded from labels. Interrupted streams may retain the HTTP status already sent, so a status code alone does not establish stream completion.

The metrics listener is separate from the API listener, so its own scrape requests are excluded from API metrics. With `telemetry.metrics.enabled=false` or `telemetry.metrics.prometheus.enabled=false`, no Prometheus registry or listener is created.

## OpenTelemetry and JSON logs

The application creates per-instance trace, metric, and log providers according to configuration. OTLP HTTP exporters send the three signals to the Collector. Prometheus scraping and OTLP metric export can be enabled independently.

Echo middleware creates server spans, while an `http.RoundTripper` wrapping the upstream transport handles client spans and W3C trace context propagation. A client span ends when the streaming response body finishes or closes. Request completion logs use `slog` with JSON output by default. A valid span in the context adds `trace_id` and `span_id`.

Logs and spans exclude request bodies, authentication headers, queries, and raw URLs. HTTP routes and methods use the same bounded values as metrics. See [OpenTelemetry observability](../operations/observability.md) for collection paths and queries.

## Startup and shutdown

1. Validate configuration and create telemetry providers, the logger, buckets, upstream transport, and registry. The Redis backend checks connectivity. Distributed memory prepares persistent logs and the Raft node.
2. Bind all required addresses first. If any bind fails, close listeners already opened and fail startup.
3. Start file content watching, the Echo API, and optional admin and metrics servers, then enable readiness.
4. On a shutdown signal or server error, disable readiness and call `Shutdown` on all servers with the same deadline.
5. When the deadline expires, cancel request contexts and close remaining connections. Stop file watching; after HTTP servers finish, clean up upstream idle connections and bucket clients. Distributed memory closes Raft workers, HTTP CONNECT connections, and storage.
6. Shut down telemetry providers with a context separate from the shutdown signal to export pending data. Apply `telemetry.shutdown_timeout` to this step.

Shutdown signal contexts and HTTP request contexts are separate, so receiving a signal does not immediately cancel active streams. A normal drain returns a successful exit code; startup errors or an exceeded shutdown deadline return a failure exit code.
