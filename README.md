# Token router

A Go proxy server that forwards OpenAI API requests to multiple upstreams and observes and limits request counts and model token usage.

## Getting started

For your first run, follow the tool requirements and execution steps in [Proxy server development and execution](docs/development/proxy-server.md). The same document covers building and testing.

## Documentation

Find the complete documentation in the [Documentation index](docs/README.md).

- [Application configuration](docs/reference/configuration.md): upstream routing, authentication, and usage limits
- [Policy configuration and admin API](docs/operations/policy-configuration.md): policy file management and runtime updates
- [OpenTelemetry observability](docs/operations/observability.md): logs, metrics, and tracing
- [Proxy server architecture](docs/architecture/proxy-server.md): package structure and request processing
