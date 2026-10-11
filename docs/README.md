# Documentation index

Start with [Proxy server development and execution](development/proxy-server.md) to build, run, and validate the application. Use [Application configuration](reference/configuration.md) for settings and precedence, and the operations guides for runtime policy changes, shared limits, and observability. Architecture and history provide implementation details and historical context.

## Architecture

- [Proxy server architecture](architecture/proxy-server.md): package hierarchy and responsibilities, proxy processing, bucket backends, metrics, and shutdown order

## Development

- [Proxy server development and execution](development/proxy-server.md): Docker builds, execution, linting, unit tests, and Redis connections
- [Proxy performance measurement](development/performance.md): request pipeline benchmarks, component benchmarks, profiling, and configuration costs
- [Ginkgo E2E tests](development/end-to-end-tests.md): Docker Compose scenario tests using actual server processes, Redis, and observability components
- [kind test environment](development/kind-test-environment.md): cluster configuration, commands, smoke tests, and local images
- [Deployment components](development/deployment-components.md): manifests, installation, and configuration management for Redis and observability components

## Operations

- [Distributed memory rate limiting](operations/distributed-rate-limiting.md): Raft clusters without Redis, admin-port peer discovery, shared per-account balances, and recovery
- [Policy configuration and admin API](operations/policy-configuration.md): separate upstream and limit configuration, file watching, a dedicated admin listener, and configuration persistence
- [OpenTelemetry observability](operations/observability.md): JSON logs, metrics and tracing configuration, collection paths, and queries

## Configuration

- [Application configuration](reference/configuration.md): YAML layers, environment variable and CLI precedence, Host and model upstream routing, transport tuning, timeouts, authentication, request counts, and token usage limits

## History

- [Proxy performance optimization](history/proxy-performance-optimization.md): recorded changes to forwarding, routing, usage observation, account identification, rate limiting, and observability
