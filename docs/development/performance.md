# Proxy performance measurement

The benchmarks separate proxy processing costs from upstream network latency and model inference. Run comparisons with the same Go toolchain, CPU allocation, benchmark inputs, and configuration. Avoid other builds or benchmarks during a timing comparison. Use repeated runs, and compare both time and allocations.

## Request pipeline

[`BenchmarkProxyPipeline`](../../src/internal/app/performance_test.go) exercises Echo routing, admission, proxy forwarding, and enabled observability with an in-process upstream transport. It checks response status and byte count. The scenarios cover disabled observability, default logging and Prometheus metrics, account identification, model routing, JSON usage, and SSE usage.

The upstream response and log destination are in memory. These results do not measure socket writes, TLS, external log collection, Redis round trips, Raft commit latency, model inference, or time to first token. Validate those costs with representative deployment traffic and upstreams before selecting production settings. Allocation rates describe bytes allocated per operation, not retained heap size.

```sh
go test ./src/internal/app -run '^$' \
  -bench '^BenchmarkProxyPipeline$' -benchmem -benchtime=1s -count=5 -cpu=1,4
```

## Component benchmarks

The proxy benchmarks include response copying, routing, and SSE event observation. The memory limiter benchmarks compare serial and parallel admission for one or many accounts. The distributed limiter benchmark measures usage accounting when token limiting is disabled. Observability benchmarks cover disabled signals, filtered logs, enabled logs and metrics, and sampled or unsampled upstream traces.

```sh
go test ./src/internal/proxy/... ./src/internal/bucket/... ./src/internal/observability/... \
  -run '^$' -bench . -benchmem -benchtime=1s -count=5 -cpu=1,4
go test ./src/internal/app -run '^$' \
  -bench '^BenchmarkAccountIdentifier$' -benchmem -benchtime=1s -count=5
```

## CPU and allocation profiles

Measure normal builds for performance and use the race detector separately for correctness. Instrumentation changes timings and allocation behavior.

Store profiles outside the repository. The following commands use a temporary directory and the benchmark's compiled binary for symbol resolution:

```sh
profile_dir=$(mktemp -d)
go test ./src/internal/app -run '^$' \
  -bench '^BenchmarkProxyPipeline/defaults$' -benchtime=5s -cpu=1 \
  -cpuprofile="$profile_dir/cpu.out" -memprofile="$profile_dir/memory.out" \
  -o "$profile_dir/app.test"
go tool pprof -top "$profile_dir/app.test" "$profile_dir/cpu.out"
go tool pprof -alloc_space -top "$profile_dir/app.test" "$profile_dir/memory.out"
```

## Configuration costs

- `upstream.max_idle_conns` and `upstream.max_idle_conns_per_host` control idle connection retention. Size them for the number of destinations and observed connection reuse. They do not cap active requests. See [Application configuration](../reference/configuration.md).
- Model conditions require reading a JSON request body before selecting an upstream. Host-only routing streams request bodies without this inspection. `upstream.max_routing_body_bytes` bounds inspection size. See [Multiple upstream routing](../reference/configuration.md#multiple-upstream-routing).
- Token usage limiting adds bounded JSON or SSE observation to eligible generation responses. Request-only limiting does not install this observer. See [Request count and token usage limits](../reference/configuration.md#request-count-and-token-usage-limits).
- Local memory, Redis, and distributed memory have different consistency and latency properties. Redis admission performs an atomic server-side script; distributed memory admission waits for a majority-backed Raft commit. Select a backend according to the required sharing semantics. See [Proxy server architecture](../architecture/proxy-server.md).
- Access logging formats and writes one record per completed request at `info` level. `telemetry.logs.level=warn` filters these records; `telemetry.logs.enabled=false` disables application logs. Prometheus and OTLP metrics have separate enable settings. Trace sampling controls exported trace volume while preserving trace context propagation. See [OpenTelemetry observability](../operations/observability.md).

See the [proxy performance optimization history](../history/proxy-performance-optimization.md) for the recorded implementation changes and their scope.
