# Proxy server development and execution

## Required tools

- A running Docker Engine or Docker Desktop with Docker BuildKit
- Docker Compose v2 for `make e2e`
- GNU Make and Bash
- Network access to download images and Go modules
- Go 1.27.2 for `make run` or `*-local` commands on the host
- Redis when selecting `rate_limit.backend=redis` or running integration tests against an actual Redis server

Run the following commands from the repository root. The build environment in [`Dockerfile`](../../Dockerfile) and [`go.mod`](../../go.mod) use Go 1.27.2. Builds, unit tests, vet, and lint run in Docker, so Go and `golangci-lint` are not required on the host.

## Docker builds and execution

```sh
make build
make image
docker run --rm token-router:local validate
docker run --rm --stop-timeout 40 \
  -p 127.0.0.1:8080:8080 \
  -p 127.0.0.1:9090:9090 \
  -v "$(pwd):/app/config:ro" \
  token-router:local serve --config /app/config/config.yaml
```

`make build` produces a Linux executable in Docker and exports it to `bin/token-router`. Use `make build-local` for a host-native executable. The default image name for `make image` is `token-router:local`; override it with a command such as `make image APP_IMAGE=token-router:dev`.

The runtime image contains CA certificates, the executable, and default [`config.yaml`](../../config.yaml) and [`policy.yaml`](../../policy.yaml) files. It runs with UID and GID `65532`, uses `/app` as its working directory, and defaults to `serve`. The example mounts the repository directory read-only and uses its configuration. A directory mount allows policy watching even when the host replaces the file by renaming it. Without an upstream API key, the client's `Authorization` header is used.

The image sets `TOKEN_ROUTER_SERVER_ADDRESS=:8080` and `TOKEN_ROUTER_TELEMETRY_METRICS_PROMETHEUS_ADDRESS=:9090` to allow connections from outside the container. These environment variables override YAML addresses; use `docker run -e` or CLI arguments to change them. The command above exposes host ports only on `127.0.0.1`.

`validate` checks configuration syntax and values only, without checking Redis or upstream connectivity. `tests/kind/config.yaml` configures the kind cluster separately from application configuration.

After starting the server, check these endpoints:

```sh
curl --fail http://127.0.0.1:8080/healthz
curl --fail http://127.0.0.1:8080/readyz
curl --fail http://127.0.0.1:9090/metrics
curl --fail http://127.0.0.1:8080/v1/models \
  -H "Authorization: Bearer ${OPENAI_API_KEY}"
```

`OPENAI_API_KEY` is a shell variable used by this `curl` example; the server does not read it automatically. Set a shared upstream API key through `upstream.api_key` in `policy.yaml`. If the field is omitted from the policy file, the startup environment variable `TOKEN_ROUTER_UPSTREAM_API_KEY` can supply it. Requests entering a proxy with an API key configured use that key. The proxy itself currently provides no user authentication. When run directly on the host, default API and metrics listeners bind to loopback addresses.

## Validation

```sh
make test
make vet
make lint
make e2e
```

`make test` runs ordinary Go tests with the race detector. It checks configuration layers, memory and Redis request and token usage buckets, Host and model routing, concurrent round-robin distribution, HTTP proxying, JSON and SSE usage accounting, SSE forwarding, HTTP status metrics, and graceful shutdown. Ordinary tests use local test upstreams and `miniredis`, so no OpenAI API key or external Redis server is required.

[`streaming_body_test.go`](../../src/internal/proxy/streaming_body_test.go) starts an SSE response while delaying the upstream transport's final request body read, then verifies that both body reading and stream forwarding finish.

`make vet` and `make lint` include the `e2e` build tag to check E2E test code as well. `make lint` runs `golangci-lint` v2.14.0 in Docker using [`.golangci.yaml`](../../.golangci.yaml). See [Ginkgo E2E tests](end-to-end-tests.md) for the configuration and scenarios of `make e2e`, which uses Ginkgo and an actual Redis server.

### Lint checks

The configuration keeps the standard linters (`errcheck`, `govet`, `ineffassign`, `staticcheck`, and `unused`) and the `gofmt` formatter. Additional linters target correctness, security, context propagation, and idiomatic Go:

| Focus | Additional linters |
| --- | --- |
| HTTP resources and context propagation | `bodyclose`, `contextcheck`, `noctx` |
| Errors and nil results | `errorlint`, `nilerr`, `nilnesserr`, `nilnil` |
| Security and logging | `gosec`, `loggercheck` |
| Common mistakes and redundant code | `copyloopvar`, `durationcheck`, `gocritic`, `makezero`, `unconvert`, `unparam` |
| Style, spelling, and standard library usage | `misspell`, `revive`, `usestdlibvars` |
| Tests | `ginkgolinter`, `usetesting` |
| Lint suppression hygiene | `nolintlint` |

Issue counts are unlimited so repeated findings remain visible. Every `nolint` directive must name its linter and explain its purpose. Ginkgo and Gomega are the only allowed dot import packages. A targeted `contextcheck` exclusion covers `WithUsageRecorder` callback construction in `src/internal/app/`: those callbacks inherit the HTTP request context when they execute, rather than the startup or policy watcher context. Inline `gosec` exceptions identify operator-selected configuration paths and credential-free routing fixtures. The distributed limiter's inline `contextcheck` exception documents cancellation linked to both the caller and the stream lifetime.

For local configuration and formatting validation with the same installed version:

```sh
golangci-lint config verify
golangci-lint fmt --diff
make lint-local
```

The enabled checks and settings are based on the [v2.14.0 configuration reference](https://github.com/golangci/golangci-lint/blob/v2.14.0/.golangci.reference.yml). The reference lists available options, rather than prescribing a universal linter selection.

## Using host tools

With automatic Go toolchain selection enabled, Go may download the version specified in `go.mod`.

```sh
go run ./src/cmd/token-router --help
go run ./src/cmd/token-router validate --config config.yaml
make run
```

Omitting `serve` also starts the server. Use these commands to build and validate with host tools:

```sh
make build-local
make test-local
make vet-local
go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.14.0
make lint-local
make fmt
```

## Running the Redis backend

Use the port-forwarding procedure in [Deployment components and Redis](deployment-components.md) to reach kind's Redis at host address `127.0.0.1:16379`. Keep the port forward running and execute:

```sh
go run ./src/cmd/token-router serve \
  --rate-limit-backend redis \
  --rate-limit-redis-address 127.0.0.1:16379
```

With Redis selected, startup must complete `PING` before opening HTTP listeners. Redis failures during admission return `503`. Instances sharing the same Redis DB and `rate_limit.redis.key` share request and token usage limits and must use the same limit values. See [Request count and token usage limits](../reference/configuration.md#request-count-and-token-usage-limits) for token configuration and constraints on recording usage after responses.

Run the actual Lua concurrency test against an accessible test Redis server. It creates a unique key and deletes only that key on completion.

```sh
TOKEN_ROUTER_TEST_REDIS_ADDR=127.0.0.1:16379 \
  go test -race ./src/internal/bucket -run TestRedisIntegrationSharedAtomicBucket
```

## Shutdown

On `SIGINT` or `SIGTERM`, readiness is disabled and listeners close. Active requests may finish within `server.shutdown_timeout`. After the deadline, connections are forcibly closed and upstream request contexts are canceled. A forced shutdown makes the process exit with an error.

In containers, the executable receives signals as PID 1. Set Docker's stop timeout longer than the sum of `server.shutdown_timeout` and `telemetry.shutdown_timeout`. The example above uses `--stop-timeout 40`, longer than the default total of 35 seconds.

Enable OpenTelemetry export with `telemetry.otlp.enabled=true`. See [OpenTelemetry observability](../operations/observability.md) for execution examples and Collector connections.

Main configuration and environment variables are read at startup. Upstream settings and rate limit values can change through watching the separate `policy.file` or using the dedicated admin API. See [Policy configuration and admin API](../operations/policy-configuration.md) for commands, file permissions, and API examples. Changes to the backend, distributed memory cluster, listeners, or telemetry require a process restart.

## Validating distributed memory and account limits

Follow [Distributed memory rate limiting](../operations/distributed-rate-limiting.md) for cluster configuration and [Per-account limits](../reference/configuration.md#per-account-limits) for Basic and Bearer identifiers. Ordinary Go tests use three Raft nodes on actual HTTP admin ports to verify shared limits, leader replacement, majority loss, and persistent log and snapshot recovery. No external Redis or separate cluster is required.

```sh
go test -race ./src/internal/bucket -run TestDistributed
go test -race ./src/internal/app ./src/internal/bucket -run TestAccount
```
