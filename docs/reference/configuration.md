# Application configuration

Startup configuration applies defaults, main YAML, environment variables, and explicit CLI arguments in order. A configured policy file applies last and overrides explicitly supplied upstream settings, rate limit values, and account settings. These policy settings can change at runtime; backend selection, listeners, and telemetry require a restart. See [Policy configuration and admin API](../operations/policy-configuration.md) for runtime updates.

## Application order

Configuration precedence follows this order. Later values override earlier ones.

1. Defaults from [`config.Defaults`](../../src/internal/config/config.go)
2. YAML file
3. Environment variables with the `TOKEN_ROUTER_` prefix
4. Explicit CLI arguments
5. Upstream settings, rate limit values, and account settings from `policy.file`, when specified

If `--config` is provided, that file is read. Otherwise, the path comes from `TOKEN_ROUTER_CONFIG`, then `config.yaml` in the current working directory. A missing implicit `config.yaml` allows startup with defaults. A missing or unreadable file explicitly selected by an argument or environment variable prevents startup.

CLI defaults do not override YAML or environment values. Explicit `false`, `0`, and empty strings apply where allowed by each setting. Unknown YAML fields, duplicate keys, `null`, multiple documents, invalid types, and invalid durations are errors. Empty files are also errors; use `{}` for defaults only. No command prints configuration values, and parsing errors exclude secrets.

## Configuration hierarchy

Fixed settings are in [`config.yaml`](../../config.yaml); upstream settings, rate limit values, and account settings are in [`policy.yaml`](../../policy.yaml). `policy.file` in `config.yaml` selects the policy file. Responsibilities are:

| Layer | Responsibility |
| --- | --- |
| `server` | API listener address, HTTP timeouts, header size, and shutdown wait time |
| `admin` | Optional dedicated admin API listener activation and address |
| `policy` | Separate file for upstream settings, rate limit values, and account settings, plus the content check interval |
| `upstream` (`policy.yaml`) | Default destinations and routing rules, authentication headers, shared connection pools, and upstream timeouts |
| `rate_limit` (`config.yaml`) | Activation, `memory` or `redis` backend, distributed memory and Redis connections |
| `rate_limit` (`policy.yaml`) | Per-second request and token refill rates, bursts, and account identification |
| `rate_limit.memory.distributed` | Raft activation, local and peer `IP:admin-port`, persistent paths, internal authentication token, and timeout |
| `rate_limit.account` (`policy.yaml`) | Per-account limit activation and Bearer JWT identifier claim |
| `rate_limit.redis` | Redis address, authentication, DB, shared key, TLS, and I/O timeouts |
| `telemetry` | Logs, Prometheus and OpenTelemetry settings, service resources, and export shutdown wait time |
| `telemetry.otlp` | OTLP export activation, shared HTTP base URL for all three signals, and export timeout |
| `telemetry.traces` | Trace export activation and root trace sampling ratio |
| `telemetry.metrics` | Metric activation and OTLP export interval |
| `telemetry.metrics.prometheus` | Prometheus activation, separate listener address, and path |
| `telemetry.logs` | Log activation, level, and local output format |

All leaf settings can be changed through environment variables and CLI arguments. Arrays use JSON or YAML array strings and are replaced in full. Fields inside `upstream.routes` belong in the array rather than individual arguments. YAML dots and underscores become underscores in environment variables and hyphens in CLI arguments.

| YAML key | Environment variable | CLI |
| --- | --- | --- |
| `server.address` | `TOKEN_ROUTER_SERVER_ADDRESS` | `--server-address` |
| `policy.file` | `TOKEN_ROUTER_POLICY_FILE` | `--policy-file` |
| `policy.watch_interval` | `TOKEN_ROUTER_POLICY_WATCH_INTERVAL` | `--policy-watch-interval` |
| `upstream.base_urls` | `TOKEN_ROUTER_UPSTREAM_BASE_URLS` | `--upstream-base-urls` |
| `upstream.routes` | `TOKEN_ROUTER_UPSTREAM_ROUTES` | `--upstream-routes` |
| `upstream.max_routing_body_bytes` | `TOKEN_ROUTER_UPSTREAM_MAX_ROUTING_BODY_BYTES` | `--upstream-max-routing-body-bytes` |
| `upstream.api_key` | `TOKEN_ROUTER_UPSTREAM_API_KEY` | `--upstream-api-key` |
| `rate_limit.backend` | `TOKEN_ROUTER_RATE_LIMIT_BACKEND` | `--rate-limit-backend` |
| `rate_limit.tokens_per_second` | `TOKEN_ROUTER_RATE_LIMIT_TOKENS_PER_SECOND` | `--rate-limit-tokens-per-second` |
| `rate_limit.token_burst` | `TOKEN_ROUTER_RATE_LIMIT_TOKEN_BURST` | `--rate-limit-token-burst` |
| `rate_limit.account.enabled` | `TOKEN_ROUTER_RATE_LIMIT_ACCOUNT_ENABLED` | `--rate-limit-account-enabled` |
| `rate_limit.account.jwt_claim` | `TOKEN_ROUTER_RATE_LIMIT_ACCOUNT_JWT_CLAIM` | `--rate-limit-account-jwt-claim` |
| `rate_limit.memory.distributed.enabled` | `TOKEN_ROUTER_RATE_LIMIT_MEMORY_DISTRIBUTED_ENABLED` | `--rate-limit-memory-distributed-enabled` |
| `rate_limit.redis.address` | `TOKEN_ROUTER_RATE_LIMIT_REDIS_ADDRESS` | `--rate-limit-redis-address` |
| `telemetry.otlp.enabled` | `TOKEN_ROUTER_TELEMETRY_OTLP_ENABLED` | `--telemetry-otlp-enabled` |
| `telemetry.otlp.endpoint` | `TOKEN_ROUTER_TELEMETRY_OTLP_ENDPOINT` | `--telemetry-otlp-endpoint` |
| `telemetry.traces.sample_ratio` | `TOKEN_ROUTER_TELEMETRY_TRACES_SAMPLE_RATIO` | `--telemetry-traces-sample-ratio` |
| `telemetry.metrics.enabled` | `TOKEN_ROUTER_TELEMETRY_METRICS_ENABLED` | `--telemetry-metrics-enabled=false` |
| `telemetry.metrics.export_interval` | `TOKEN_ROUTER_TELEMETRY_METRICS_EXPORT_INTERVAL` | `--telemetry-metrics-export-interval` |
| `telemetry.metrics.prometheus.enabled` | `TOKEN_ROUTER_TELEMETRY_METRICS_PROMETHEUS_ENABLED` | `--telemetry-metrics-prometheus-enabled=false` |
| `telemetry.metrics.prometheus.address` | `TOKEN_ROUTER_TELEMETRY_METRICS_PROMETHEUS_ADDRESS` | `--telemetry-metrics-prometheus-address` |
| `telemetry.metrics.prometheus.path` | `TOKEN_ROUTER_TELEMETRY_METRICS_PROMETHEUS_PATH` | `--telemetry-metrics-prometheus-path` |
| `telemetry.logs.enabled` | `TOKEN_ROUTER_TELEMETRY_LOGS_ENABLED` | `--telemetry-logs-enabled=false` |
| `telemetry.logs.level` | `TOKEN_ROUTER_TELEMETRY_LOGS_LEVEL` | `--telemetry-logs-level` |
| `telemetry.logs.format` | `TOKEN_ROUTER_TELEMETRY_LOGS_FORMAT` | `--telemetry-logs-format` |

Boolean arguments without values, such as `--telemetry-metrics-prometheus-enabled`, set `true`. Use an equals sign for `false`, as in `--telemetry-metrics-prometheus-enabled=false`. An explicitly empty `TOKEN_ROUTER_CONFIG` or `--config` path is an error.

`go run ./src/cmd/token-router --help` shows the full CLI and corresponding environment variable names. The following disables the policy file and applies main YAML settings, the environment address, and the CLI burst. With a policy file enabled, explicit file values override CLI values.

```sh
TOKEN_ROUTER_SERVER_ADDRESS=127.0.0.1:8081 \
  go run ./src/cmd/token-router serve --config config.yaml --policy-file= --rate-limit-burst 50
```

## Multiple upstream routing

`upstream.routes` in `policy.yaml` maps the request `Host` header and the JSON body's top-level `model` to destination arrays. `base_urls` accepts both `http` or `https` URLs and `host:port` addresses. Addresses without a scheme use HTTP and require a port. Enclose IPv6 addresses in brackets, such as `[::1]:9000`.

- When both `host` and `model` are specified, both must match. Omitted or empty conditions match all values. Rules omitting both conditions are shared defaults. Each rule's `base_urls` must contain at least one address.
- Rules with more specified conditions take precedence: combined `host` and `model`, then single conditions, then shared defaults. If distinct rules with the same condition count both match, the first configured rule wins.
- `host` ignores case and a trailing hostname dot. Without a configured port, the request port is ignored; with a configured port, it is also compared. `X-Forwarded-Host` is not used. Wildcards, regular expressions, and URL-form conditions are unsupported.
- `model` uses exact, case-sensitive string matching. Missing, `null`, or empty `model` values do not satisfy model conditions. Nested `model` fields and other keys such as `MODEL` are not used.
- Repeated identical `host` and `model` conditions merge address arrays in registration order. Conditions identical after host normalization are also merged.
- Addresses in the selected rule use per-process round robin. Each rule has an independent counter that also applies to concurrent requests. Separate health checks, weights, and retries to other addresses after failure are not implemented.

If no rule matches, `upstream.base_urls` supplies the default pool, defaulting to `["https://api.openai.com"]`. Even a single upstream uses a one-element array. Set only `base_urls` to distribute requests across destinations without conditions.

To reject unmatched requests, empty the default pool with `upstream.base_urls: []`. Requests without a matching rule then return `404` and `route_not_found`. Configuration with no destinations anywhere fails startup validation.

In the following example, `api.example.com` requests for `chat-model` cycle through three addresses from the two rules with identical conditions.

```yaml
upstream:
  base_urls:
    - "http://default-a:9000"
    - "http://default-b:9000"
  routes:
    - host: "api.example.com"
      model: "chat-model"
      base_urls:
        - "10.0.0.11:8000"
        - "10.0.0.12:8000"
    - host: "api.example.com"
      model: "chat-model"
      base_urls:
        - "https://inference.example.com/gateway"
    - host: "api.example.com"
      base_urls:
        - "http://host-default:8000"
    - model: "embedding-model"
      base_urls:
        - "http://embedding-a:8000"
        - "http://embedding-b:8000"
```

Arrays can also be supplied through environment variables and CLI arguments. These examples disable the policy file and supply arrays through startup configuration.

```sh
TOKEN_ROUTER_UPSTREAM_BASE_URLS='["10.0.0.11:8000","https://inference.example.com"]' \
  go run ./src/cmd/token-router serve --policy-file=

go run ./src/cmd/token-router serve --policy-file= \
  --upstream-base-urls='[]' \
  --upstream-routes='[{"host":"api.example.com","model":"chat-model","base_urls":["10.0.0.11:8000","10.0.0.12:8000"]}]'
```

### Model inspection and body size

Bodies are inspected only when model conditions can apply to the request host. If `Content-Type` is absent, `application/json`, or a media type ending in `+json`, the entire body is read to inspect the top-level `model`, then the same bytes are forwarded upstream. This applies regardless of method; bodies are not reserialized and model names are not changed. Requests using only Host conditions are not read in advance.

`upstream.max_routing_body_bytes` limits bodies used for model inspection and defaults to `8388608` (8 MiB). It must be positive and applies regardless of `Content-Length`. Oversized inspection bodies return `413` and `routing_body_too_large`; invalid JSON objects return `400` and `invalid_request_body`; a `model` that is neither a string nor `null` returns `400` and `invalid_model`. An empty body is treated as a request without a model.

JSON bodies inspected for models must have no `Content-Encoding` or use `identity`. Compressed bodies return `415` and `unsupported_content_encoding`. Models are not extracted from multipart, form, or other non-JSON bodies; those use Host conditions or the default pool. Use Host conditions to distinguish multipart image and audio API requests.

`server.read_timeout` also applies to body reception. If configured, `upstream.request_timeout` bounds the request including body reads during model inspection. After request body inspection, SSE responses continue to be forwarded without full buffering.

## Upstream paths and authentication

The incoming `/v1/...` path is appended to the selected destination URL's base path. When connecting to OpenAI, do not duplicate `/v1` in the base URL. For example, with base URL `http://localhost:9000/gateway`, `/v1/models` is forwarded as `/gateway/v1/models`. The forwarded `Host` is replaced with the selected destination's host and port.

Destinations must be `http` or `https` URLs with a host, or `host:port` addresses, and cannot contain user information, queries, or fragments. Explicit ports range from 1 to 65535. `upstream.max_idle_conns` and `upstream.max_idle_conns_per_host` must be positive. All destinations share transport and connection pool settings.

An empty `upstream.api_key` forwards the client's `Authorization`; a configured value replaces it with `Bearer <configured-key>`. Nonempty `upstream.organization` and `upstream.project` also override the corresponding OpenAI headers. These authentication settings apply to all routing destinations. Keys and passwords can be injected through environment variables; example YAML in the repository contains no actual values.

## Timeouts and streaming

Durations use Go duration strings such as `500ms`, `10s`, `2m`, and `0s`. Negative values are rejected.

- `server.read_header_timeout`: time allowed to receive client request headers.
- `server.read_timeout`: time allowed through request body reception. The default `0s` disables the overall read limit.
- `server.write_timeout`: time allowed to write responses. The default `0s` permits long SSE responses.
- `server.idle_timeout`: time waiting for the next request on a client keep-alive connection.
- `server.shutdown_timeout`: total wait for active requests during shutdown; must be positive.
- `upstream.dial_timeout`: TCP connection and TLS handshake limit.
- `upstream.response_header_timeout`: wait for upstream response headers.
- `upstream.request_timeout`: overall request limit including SSE streams. The default `0s` disables it.
- `upstream.idle_conn_timeout`: upstream idle connection lifetime.
- `rate_limit.redis.dial_timeout`, `read_timeout`, `write_timeout`: Redis connection and I/O limits. All must be positive for the Redis backend; each command also has a total limit equal to the maximum of these three values.

Overall request timeouts also apply to SSE responses already started. After response headers have been sent, an error cannot change the HTTP status code, so the connection closes.

## Telemetry configuration

All observability settings live under `telemetry`. `telemetry.otlp.enabled` controls only OTLP export and defaults to `false`. `telemetry.metrics.enabled` and `telemetry.logs.enabled` control metrics and logs respectively and both default to `true`. Defaults create no OTLP exporters and use a Prometheus listener and JSON stderr logs. Enabling OTLP also sends enabled signals to the Collector. `telemetry.traces.enabled` defaults to `true`, and tracing operates when OTLP export is also enabled.

With metrics enabled, `telemetry.metrics.prometheus.enabled` controls the Prometheus listener and defaults to `true`. `telemetry.metrics.prometheus.address` and `telemetry.metrics.prometheus.path` default to `127.0.0.1:9090` and `/metrics`. Prometheus works even with `telemetry.otlp.enabled=false`. With `telemetry.metrics.enabled=false`, no Prometheus registry, listener, or OTLP metric exporter is created.

`telemetry.logs.level` supports `debug`, `info`, `warn`, and `error`, defaulting to `info`. Local logs go to stderr, with `telemetry.logs.format` selecting `json` or `text` and defaulting to `json`. Levels apply to both local output and OTLP export. Local logs work even with `telemetry.otlp.enabled=false`. `telemetry.logs.enabled=false` disables both local application logs and OTLP log export.

`telemetry.otlp.endpoint` is an OTLP HTTP base URL such as `http://otel-collector:4318`. Exporters append `/v1/traces`, `/v1/metrics`, and `/v1/logs`. The `https` scheme is supported; user information, queries, and fragments are rejected. The export timeout defaults to `5s`, and the metric export interval to `15s`.

`telemetry.service_name`, `telemetry.service_version`, and `telemetry.environment` apply to resources for all signals and default to `token-router`, `dev`, and `development`. `telemetry.traces.sample_ratio` is a finite value from 0 to 1, defaulting to `1`. The ratio applies to root traces, while valid parent spans determine inherited sampling decisions.

An empty `telemetry.service_instance_id` generates a unique ID on each process start. If specified manually, use a different value for each running instance. This distinguishes metrics from multiple processes of the same service.

After HTTP requests drain, export completes within `telemetry.shutdown_timeout`, defaulting to `5s`. Set deployment shutdown wait times longer than the sum of `server.shutdown_timeout` and `telemetry.shutdown_timeout`.

See [OpenTelemetry observability](../operations/observability.md) for JSON logs, trace correlation, and collection configuration.

## Request count and token usage limits

Request buckets consume one unit for every admitted request. `rate_limit.requests_per_second` is the refill rate, and `rate_limit.burst` the maximum request count. Both must be positive with `rate_limit.enabled=true`.

Token usage buckets use `rate_limit.tokens_per_second` and `rate_limit.token_burst`, both defaulting to `0` for request-only limiting. To enable token limits, set both the per-second refill rate `rate_limit.tokens_per_second` and capacity `rate_limit.token_burst` positive. Setting exactly one value to `0` is invalid. Bursts are integers; refill rates may be fractional. Refill rates cannot be `NaN` or infinite, and a full refill period for an active bucket must fit in Go `time.Duration`.

Buckets start full. Admission requires at least one request unit and, when enabled, at least one token unit. If either is insufficient, no request unit is deducted, and the response is `429` with `Retry-After` in seconds. `Retry-After` rounds up the wait until both balances recover, capped at the maximum Go `time.Duration`. There is no queue; health, readiness, and metrics do not consume limits.

Tokens are neither estimated nor reserved at admission; they are deducted after usage is observed in upstream responses. Usage exceeding the balance or `rate_limit.token_burst` produces a negative balance, blocking subsequent requests until refill restores at least one token. Already admitted requests and concurrent streams continue, so this is not a strict token cap covering in-progress requests. For example, a request admitted with 60,000 tokens that uses 70,000 blocks later requests until 10,001 tokens refill.

Keep `rate_limit.enabled: true` and `rate_limit.backend: memory` in `config.yaml` and set these values in `policy.yaml`. Requests refill at 10 per second with a maximum of 20; model tokens refill at 1,000 per second with a maximum of 60,000.

```yaml
rate_limit:
  requests_per_second: 10
  burst: 20
  tokens_per_second: 1000
  token_burst: 60000
```

### Per-account limits

The default `rate_limit.account.enabled: false` uses one global bucket. Enabling the following in `policy.yaml` creates separate request and token usage buckets per account. The same `requests_per_second`, `burst`, `tokens_per_second`, and `token_burst` values apply to every account. Individual account-specific values and an additional global cap above account buckets are not provided.

This feature only extracts identifiers. It does not verify Basic passwords or JWT signatures, issuers, audiences, or expiry. Clients can change identifiers in unverified JWT payloads; trusting per-account limits requires an authentication layer in front of the proxy to validate the same header. Even with `upstream.api_key` configured, accounts are determined from the incoming `Authorization` before upstream header replacement.

```yaml
rate_limit:
  requests_per_second: 10
  burst: 20
  tokens_per_second: 1000
  token_burst: 60000
  account:
    enabled: true
    jwt_claim: sub
```

Authentication schemes in `Authorization` are case-insensitive.

- Basic uses the `id` before the first colon in Base64-decoded `id:password`. The same ID identifies the same account even with different passwords.
- Bearer requires a three-part, dot-separated JWT form. Its second part is Base64URL-decoded, and the string named by `jwt_claim` is read from the top-level JSON object. The default is `sub`; alternatives such as `account_id` or `user_id` are allowed. Nested paths, numeric claims, and object claims are unsupported. Changes to other token fields do not change the account if the selected claim is unchanged.
- Identical identifier strings from either scheme share a bucket. Issuers and authentication schemes are not added as separate namespaces. Case and surrounding whitespace are not normalized.

Missing or multiple `Authorization` headers, unsupported schemes, invalid encoding, and missing identifiers return `401` and `rate_limit_account_required`. Identifiers must be UTF-8 strings without control characters, no more than 1,024 bytes, and not whitespace-only. Authentication headers up to 16 KiB are accepted for extraction. There is no fallback to an anonymous global bucket. With `rate_limit.enabled: false`, account extraction is also skipped.

Backends receive SHA-256 identifier hashes; Basic passwords, raw JWTs, and raw identifiers are not used as storage keys. Redis account keys append `:account:<hash>` to `rate_limit.redis.key`. Memory and distributed memory maintain per-account state, clean up fully refilled idle state, and retain remaining token debt.

Response usage is recorded against the account selected at admission. Changing `account.enabled` or `jwt_claim` during a stream does not change that request's account. Account setting changes apply to new requests and neither split existing global balances among accounts nor merge balances of different identifiers. Change limits and account settings through the [policy file and admin API](../operations/policy-configuration.md).

### Usage in OpenAI responses

When token limiting is enabled, usage is read from successful `POST` responses (HTTP `2xx`) on these paths. Paths are evaluated on the incoming request before the selected destination's base path is appended. Requests for the same account share request and token usage buckets across routing rules and destinations. With account limiting disabled, they share global buckets.

| API | Counted paths |
| --- | --- |
| Chat Completions and Completions | `/v1/chat/completions`, `/v1/completions` |
| Responses | `/v1/responses`, `/v1/responses/compact` |
| Embeddings | `/v1/embeddings` |
| Images | `/v1/images/generations`, `/v1/images/edits`, `/v1/images/variations` |
| Audio | `/v1/audio/speech`, `/v1/audio/transcriptions`, `/v1/audio/translations` |

Stored-response retrieval `GET` requests, Chat Completions metadata update `POST` requests, and unlisted paths are not counted. Even supported paths must supply one of the following token usage formats. Audio durations, image counts, and similar values are not converted to tokens.

- Prefer valid `usage.total_tokens` in JSON responses. If absent or invalid, use a valid sum of `usage.prompt_tokens` and `usage.completion_tokens`, or `usage.input_tokens` and `usage.output_tokens`. Counts must be nonnegative JSON integers within the `int64` range.
- Chat Completions SSE reads `usage` from usage chunks. Clients must include `"stream_options": {"include_usage": true}` for OpenAI to send these chunks. The proxy does not alter the request body or add this option automatically. See the [OpenAI Chat Completions streaming reference](https://developers.openai.com/api/reference/resources/chat/subresources/completions/streaming-events).
- Responses SSE reads `response.usage` from terminal `response.completed`, `response.incomplete`, and `response.failed` events. It records usage only when present. See the [OpenAI Responses streaming reference](https://developers.openai.com/api/reference/resources/responses/streaming-events).

If a response contains multiple usage chunks, only increases in cumulative token counts are applied to avoid double counting. Missing or invalid usage is not estimated. Interrupted streams record only valid usage actually received before interruption; missing final usage chunks may leave tokens undeducted. Detailed input and output token statistics are not exported as separate metrics.

For asynchronous Responses, tokens are not counted when the initial `POST` returns without usage and subsequent `GET` polling or a resumed stream supplies it. The proxy neither tracks response IDs across requests nor separately fetches usage. Such [OpenAI background mode](https://developers.openai.com/api/docs/guides/background) flows may apply only request limits.

Usage observation is limited to 8 MiB per JSON body or SSE event; larger bodies or events are not counted. JSON is copied separately while body fragments are forwarded to clients, then parsed when reading ends or the body closes. Even after interrupted transmission, already received complete valid JSON contributes usage; incomplete JSON does not. SSE is parsed per event, and events after oversized events continue to be observed. Supported `POST` paths request `Accept-Encoding: identity` during observation; compressed upstream responses are forwarded unchanged without counting. Response bodies and SSE forwarding formats are not modified.

### Storage and error handling

`memory` is independent per process by default. `rate_limit.memory.distributed.enabled: true` shares balances across nodes through Raft. See [Distributed memory rate limiting](../operations/distributed-rate-limiting.md) for admin-port `IP:port` lists, persistent data, and majority operation rules. `redis` shares request and token usage across processes using the same DB and `rate_limit.redis.key`, using Redis server time. Sharing instances must configure identical limits. Redis keys expire after enough idle time to refill all buckets, including negative balances. If a usage deduction pushes refill time beyond the supported duration, the key does not expire, preserving debt.

Redis or distributed memory errors during admission return `503`. Usage recording failures on responses already started emit warnings without changing responses. Deductions with uncertain outcomes are not automatically retried to avoid duplicates, so storage failures can leave usage uncounted.

`rate_limit.enabled=false` disables both limits without Redis connections, Raft node creation, or usage observation. Activation, backend selection, distributed memory, and Redis connections are determined at startup. Upstream settings, rate limit values, and account settings can change at runtime through [Policy configuration and admin API](../operations/policy-configuration.md).
