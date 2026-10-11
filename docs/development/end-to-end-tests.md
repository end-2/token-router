# Ginkgo E2E tests

[`tests/e2e/`](../../tests/e2e/) uses Ginkgo v2 and Gomega to validate the built proxy executable. Each scenario starts the server as a separate process and checks HTTP requests, responses, metrics, logs, traces, and exit status. Upstreams use `httptest.Server` in the test process; Redis uses an actual server.

Collector, Prometheus, Tempo, and Loki also run as actual servers. No OpenAI API key or external OpenAI calls are required.

## Running in Docker

Docker, Docker Compose v2, GNU Make, and Bash are required. Run from the repository root:

```sh
make e2e
```

[`tests/e2e/compose.yaml`](../../tests/e2e/compose.yaml) starts the E2E runner and these services in separate containers. Images are pinned by version and digest.

| Service | Role |
| --- | --- |
| Redis | Actual request and token usage buckets shared by multiple proxy processes |
| OpenTelemetry Collector | Receives proxy OTLP/HTTP metrics, logs, and traces and forwards them to each backend |
| Prometheus | Scrapes Collector metrics and supports PromQL queries |
| Tempo | Stores traces and supports trace ID queries |
| Loki | Stores OTLP logs and supports LogQL queries |

Test upstreams and proxy processes run inside the runner, and other services connect through the Compose network. The runner starts after the Redis health check, and suite setup checks readiness endpoints for the Collector and each backend. Test ports are not exposed on the host, and the Docker socket is not mounted in the runner.

Prometheus, Tempo, and Loki store data in `tmpfs` configured for each process UID. Each store is limited to 256 MiB, and data is removed when its container stops.

Collector, Prometheus, Tempo, and Loki use the configuration from [`deploy/components/`](../../deploy/components/) unchanged. Metric validation queries values stored in Prometheus rather than only checking Collector responses. Traces are checked through Tempo's `/api/traces/{traceID}`, and logs through Loki's `/loki/api/v1/query_range`.

[`tests/e2e/run.sh`](../../tests/e2e/run.sh) creates a unique Compose project for every run. On exit it removes that project's containers, networks, storage volumes, and automatically generated runner image tag. Downloaded service images are retained. On failure, it prints recent container logs and exits unsuccessfully. The first run needs to download Go modules, build images, and service images.

## Test setup

E2E Go files use the `e2e` build tag. `make test` runs ordinary Go tests, and `make e2e` runs E2E tests separately. The Ginkgo CLI version is managed through the `tool` declaration and module dependencies in [`go.mod`](../../go.mod).

The runner uses a normally built executable, while Ginkgo test code uses the race detector. The suite timeout is 10 minutes; pending scenarios and empty suites also fail.

Proxy processes start in independent temporary directories. Existing `TOKEN_ROUTER_` and `OTEL_` environment variables are removed so only test-generated configuration applies, and listeners use free ports selected by the operating system. Redis tests create unique keys and delete only their own keys on exit. Telemetry tests use a unique `service.name` and trace ID for each scenario.

## Scenarios

The suite runs these scenarios:

| Scope | Validation |
| --- | --- |
| Configuration precedence | Upstream authentication follows defaults, YAML, environment variables, then CLI arguments |
| Policy configuration | File replacement detection, dedicated admin port isolation, API changes and persisted values after restart, backend change rejection, and token limit activation for memory and Redis (2 table entries) |
| HTTP forwarding | Preserves methods, base URL paths, queries, bodies, OpenAI organization and project headers, and upstream error responses |
| Upstream routing | Host and model conditions, merging URL and address arrays with identical conditions, round robin, and default destinations |
| Routed SSE | Incremental SSE forwarding from a model-selected upstream and shared token usage limits |
| Health and metrics | `/healthz`, `/readyz`, a custom metrics path on a separate listener, and Prometheus request counters |
| Upstream timeout | Response header timeouts return `504` and `upstream_timeout` and cancel upstream requests |
| Memory bucket | Exhausted bursts return `429` and `Retry-After`; denied requests never reach the upstream |
| Shared Redis bucket | Thirty concurrent requests to two server processes admit only the shared burst of five |
| Token usage limits | JSON and SSE usage exceeding the burst causes subsequent requests to return `429` and `Retry-After` without upstream forwarding for memory and Redis. Redis also verifies shared usage across two server processes (4 table entries) |
| Redis command failure | A test key type error returns `503` and blocks upstream requests |
| Invalid configuration | Unknown YAML fields, unsupported backends, and a `0s` shutdown timeout are rejected before listeners are created (3 table entries) |
| SSE cancellation | A second event is generated after receipt of the first to verify incremental delivery, and client cancellation propagates upstream |
| Graceful shutdown | An active SSE response completes after `SIGTERM`, with exit code `0` |
| Shutdown timeout | An unfinished SSE response is interrupted after the deadline, the upstream request is canceled, and exit code `1` is returned |
| Telemetry correlation | Incoming W3C `traceparent`, parent relationships of server and upstream client spans, `trace_id` and `span_id` in JSON stderr and Loki logs, and Prometheus request counters |
| Sensitive request information | API keys, query values, and request bodies are absent from collected traces, Loki logs, and JSON stderr |
| Individual signal settings | Enable only metrics, traces, or logs and verify corresponding data is absent from other backends (3 table entries) |
| Error traces | An upstream `503` stores both server and client spans with error status |
| Disabled OTLP export | No OTLP data is exported while JSON access logs and the existing Prometheus listener remain available |
| OTLP rate limit metrics | Prometheus contains allowed and denied counters and HTTP `429` counters |
| Telemetry flush on shutdown | Ending an SSE response delivers final spans, logs, and even metrics with a `1h` export interval to the backends |

HTTP and configuration scenarios are in [`proxy_test.go`](../../tests/e2e/proxy_test.go), policy configuration in [`policy_test.go`](../../tests/e2e/policy_test.go), token usage limits in [`token_usage_test.go`](../../tests/e2e/token_usage_test.go), SSE and shutdown in [`streaming_test.go`](../../tests/e2e/streaming_test.go), and telemetry in [`telemetry_test.go`](../../tests/e2e/telemetry_test.go). Backend ingestion is checked by polling; disabled signals are checked for absent data over a period that includes Collector batching and Prometheus scrape intervals.

## Running on the host

Docker Compose handles executable builds and all service dependencies together. To run directly on the host, prepare Go 1.27.2, a built executable, and accessible test services, then set these environment variables:

| Environment variable | Value |
| --- | --- |
| `TOKEN_ROUTER_E2E_BINARY` | Absolute path to the proxy executable |
| `TOKEN_ROUTER_E2E_REDIS_ADDRESS` | Actual Redis `host:port` |
| `TOKEN_ROUTER_E2E_OTLP_ENDPOINT` | Collector OTLP/HTTP base URL, such as `http://127.0.0.1:4318` |
| `TOKEN_ROUTER_E2E_COLLECTOR_HEALTH_URL` | Collector readiness URL, such as `http://127.0.0.1:13133` |
| `TOKEN_ROUTER_E2E_PROMETHEUS_URL` | Prometheus HTTP base URL, such as `http://127.0.0.1:9090` |
| `TOKEN_ROUTER_E2E_TEMPO_URL` | Tempo HTTP base URL, such as `http://127.0.0.1:3200` |
| `TOKEN_ROUTER_E2E_LOKI_URL` | Loki HTTP base URL, such as `http://127.0.0.1:3100` |

Run after setting all environment variables:

```sh
make build-local
export TOKEN_ROUTER_E2E_BINARY="$(pwd)/bin/token-router"
go tool ginkgo --tags=e2e --race --timeout=10m \
  --fail-on-pending --fail-on-empty ./tests/e2e
```

Suite setup fails if a required environment variable is missing or a service is inaccessible. Test backends use neither authentication nor TLS, and tests create Redis keys and data associated with unique service names. See [Deployment components and OpenTelemetry](deployment-components.md) for deployment configuration.
