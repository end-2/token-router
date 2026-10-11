# Policy configuration and admin API

All `upstream` settings and rate limit values and account settings are managed in `policy.yaml` and can be replaced at runtime. File watching and the dedicated admin API share the same validation and application path. The repository's `config.yaml` enables file watching through `policy.file: policy.yaml` and disables the admin listener by default.

## Configuration separation

The main `config.yaml` contains listeners, file paths, backend selection, and observability settings.

```yaml
server:
  address: "127.0.0.1:8080"
admin:
  enabled: true
  address: "127.0.0.1:9091"
policy:
  file: policy.yaml
  watch_interval: 1s
rate_limit:
  enabled: true
  backend: memory
```

The following fields are allowed in `policy.yaml`. See the root [`policy.yaml`](../../policy.yaml) for the full default configuration.

```yaml
upstream:
  base_urls:
    - "http://127.0.0.1:8000"
  routes: []
  request_timeout: 0s
rate_limit:
  requests_per_second: 10
  burst: 20
  tokens_per_second: 1000
  token_burst: 60000
  account:
    enabled: true
    jwt_claim: sub
```

`upstream` supports destinations, Host and model routing, API keys, organization and project headers, timeouts, and connection pool settings. `rate_limit` supports `requests_per_second`, `burst`, `tokens_per_second`, `token_burst`, `account.enabled`, and `account.jwt_claim`. See [Per-account limits](../reference/configuration.md#per-account-limits) for identifier rules. `rate_limit.enabled`, `rate_limit.backend`, `rate_limit.memory`, `rate_limit.redis`, and other top-level settings are fixed in the main configuration. These fields are rejected in policy files and admin requests.

Relative `policy.file` paths are resolved from the main configuration file's directory. The path and watch interval are fixed at startup. `policy.watch_interval` must be positive and defaults to `1s`. These CLI arguments and environment variables are also available:

| YAML key | Environment variable | CLI |
| --- | --- | --- |
| `policy.file` | `TOKEN_ROUTER_POLICY_FILE` | `--policy-file` |
| `policy.watch_interval` | `TOKEN_ROUTER_POLICY_WATCH_INTERVAL` | `--policy-watch-interval` |
| `admin.enabled` | `TOKEN_ROUTER_ADMIN_ENABLED` | `--admin-enabled` |
| `admin.address` | `TOKEN_ROUTER_ADMIN_ADDRESS` | `--admin-address` |

```sh
go run ./src/cmd/token-router serve --config config.yaml --admin-enabled
```

Main configuration applies defaults, main YAML, environment variables, and explicit CLI arguments in that order. The policy file is applied last, so explicitly specified file values override environment variables and CLI arguments. Omitted values use the startup configuration before policy application. File replacements are merged against that same startup configuration, so removing a field restores its startup value. Arrays are replaced in full, and `null` is not allowed.

A custom configuration omitting `policy.file`, or `--policy-file=`, uses startup configuration without file watching. The repository's `policy.yaml` includes `upstream.api_key: ""`, so it forwards the client's `Authorization` by default. To use `TOKEN_ROUTER_UPSTREAM_API_KEY`, omit the `api_key` field from the policy file.

## File watching and application

File contents are read at the configured interval and compared using SHA-256. Identical contents do not recreate configuration. File replacement by rename or a changed symlink target is read on the next check. The main configuration file and environment variables are not reread.

A missing or invalid policy file at startup prevents startup. After startup, deletion or read, syntax, or value validation failures retain the last valid configuration and emit a warning. Watching resumes applying changes when the file recovers. Policy files and admin request bodies are each limited to 1 MiB. Empty files are errors; use `{}` to apply only defaults.

Complete replacement content in another file, then rename it on the same filesystem to avoid exposing partially written content. Configuration applies only after full validation; an invalid upstream setting or limit prevents both from changing. Upstream changes apply to new requests. Active responses and SSE streams continue on the existing upstream, and old connection pools are cleaned up after completion. Changes only to rate limit values or account identifier settings preserve upstream pools and round-robin counters. Changing whether token usage is observed creates a new proxy.

Bucket implementations and Redis clients are not replaced. Changing active bucket values does not refill balances; reduced capacities cap positive balances while preserving negative token balances. Local memory computes balances up to the update time using the previous refill rate. Redis uses new values from the next Lua call to calculate balances, refill since the previous call, and TTL; configuration updates alone do not modify Redis keys. Processes sharing Redis or distributed memory must receive the same limits and account settings; the admin API changes only the receiving process's configuration and file. Distributed memory computes each account balance with its previous refill rate before applying new values on the next command. See [Distributed memory rate limiting](distributed-rate-limiting.md).

Set both `tokens_per_second` and `token_burst` to `0` to disable token usage limiting, and set both positive to enable it again. Whether usage is observed depends on the proxy selected by the request; observed usage is processed using bucket settings at recording time. Usage from streams started while token limiting was disabled is not collected retroactively.

## Admin API

`admin.enabled=true` opens a separate HTTP listener. Its default address is `127.0.0.1:9091` and must differ from the API and Prometheus addresses. An admin bind failure closes other listeners and fails startup. Shutdown follows the same graceful procedure as the other HTTP listeners.

| Method and path | Behavior |
| --- | --- |
| `GET /config` | Retrieve the complete currently effective policy configuration |
| `PATCH /config` | Merge only request fields into current values, then validate and apply |
| `PUT /config` | Apply the request as a new policy document; omitted values use startup configuration |

Input is a JSON or YAML mapping. Responses are JSON with the same key names as YAML and durations as strings such as `10s`. A `GET` response can be submitted unchanged with `PUT`. `PATCH` also replaces entire arrays. Requests apply sequentially, with no separate version conflict detection.

```sh
curl --fail http://127.0.0.1:9091/config

curl --fail -X PATCH http://127.0.0.1:9091/config \
  -H 'Content-Type: application/json' \
  --data '{"rate_limit":{"requests_per_second":25,"burst":50}}'

curl --fail -X PATCH http://127.0.0.1:9091/config \
  -H 'Content-Type: application/json' \
  --data '{"upstream":{"base_urls":["http://127.0.0.1:8001"],"routes":[]}}'

curl --fail -X PUT http://127.0.0.1:9091/config \
  -H 'Content-Type: application/yaml' --data-binary @policy.yaml
```

Success returns `200` and the applied configuration. Invalid configuration and fixed-field changes return `400`, oversized bodies return `413`, file persistence failures return `500`, and updates during shutdown return `503`. Error bodies do not include configuration input values. `/config` is not registered on the public API or Prometheus listener, and admin requests do not consume proxy rate limits.

When `policy.file` is configured, the API serializes the complete effective configuration to YAML, renames a temporary file in the same directory, then applies the configuration in memory. Persistence failure also preserves the in-memory configuration. The saved file is used after restart; existing YAML comments and formatting are not preserved. Effective values, including API keys inherited from startup configuration, are also saved. Created files have permissions `0600`.

API persistence requires a regular file and directory write permission. Symlink files support watching only; API persistence rejects them. Read-only mounts and locations that do not allow rename, such as single-file bind mounts, also fail persistence. For container API persistence, mount a writable directory and grant its write permission to the runtime UID. If `policy.file` is empty, API changes remain only in the process's memory.

The `/config` API provides no authentication and includes `upstream.api_key` in responses. Use the default loopback address or an access-restricted management network. For connections from outside a container, explicitly set `--admin-address=:9091` and expose only the required host port.

Enabling distributed memory registers `/internal/rate-limit/` routes on the same admin port. Internal routes authenticate with a shared token, which does not affect access to `/config`. Retain management network access restrictions when exposing the admin listener for peer communication.
