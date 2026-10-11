# Deployment components and OpenTelemetry

`deploy/components/` provides Redis and an environment for collecting and querying OpenTelemetry data. Each subdirectory is an independent Kustomize configuration, and the parent `kustomization.yaml` combines all components. The deployment environment specifies the namespace.

## Components and data paths

```text
token-router
  metrics, logs, traces -> otel-collector:4318 (OTLP HTTP)
    metrics -> otel-collector:8889 <- prometheus:9090
    logs    -> loki:3100/otlp
    traces  -> tempo:4318

grafana:3000 -> prometheus:9090, loki:3100, tempo:3200
```

| Directory | Image version | Service and purpose | Storage |
| --- | --- | --- | --- |
| `redis/` | `redis:7.4.11-alpine3.21` | `redis:6379`, request buckets | `1Gi` PVC |
| `otel-collector/` | `otel/opentelemetry-collector-contrib:0.162.0` | `4318`, OTLP HTTP ingestion. `8889`, Prometheus scraping. `13133`, health checks | In-memory queue |
| `prometheus/` | `prom/prometheus:v3.15.0` | `prometheus:9090`, metric queries | `emptyDir`, `24h` retention |
| `tempo/` | `grafana/tempo:2.10.8` | `tempo:4318`, OTLP HTTP ingestion. `tempo:3200`, trace queries | `emptyDir`, `24h` retention |
| `loki/` | `grafana/loki:3.7.8` | `loki:3100`, OTLP log ingestion and queries | `emptyDir`, `24h` retention |
| `grafana/` | `grafana/grafana:13.2.3` | `grafana:3000`, data query UI | `emptyDir` |

All images are pinned by tag and digest. Each component's `service.yaml` and `deployment.yaml` or `statefulset.yaml` define ports, health checks, resource limits, and volumes. Configuration uses `config.yaml`, except Grafana uses `datasources.yaml` and Redis uses `redis.conf`. Kustomize-generated ConfigMap names depend on their contents, so applying configuration changes replaces Pods.

This is a single-instance development and test environment. All Services are `ClusterIP`, with no authentication or TLS configured. Grafana uses anonymous `Viewer` access and disables login and initial administrator creation. Observability data in `emptyDir` is deleted when Pods are replaced. The table lists configured retention policies without guaranteeing exact deletion times. Configure authentication, TLS, persistent storage, retention, and resource limits for your production environment.

## Connecting the application

Set these environment variables on an application in the same namespace. `TOKEN_ROUTER_TELEMETRY_OTLP_ENDPOINT` is a base URL without individual signal paths such as `/v1/traces`.

```yaml
env:
  - name: TOKEN_ROUTER_TELEMETRY_OTLP_ENABLED
    value: "true"
  - name: TOKEN_ROUTER_TELEMETRY_OTLP_ENDPOINT
    value: http://otel-collector:4318
```

The application emits JSON logs and sends enabled metrics, logs, and traces to the Collector. Application `telemetry` settings configure `service.name`, `service.instance.id`, `service.version`, and `deployment.environment.name`. The application currently does not set `service.namespace`, but the Collector can ingest data from other services that supply it. See [Proxy server development and execution](proxy-server.md) for configuration keys and precedence.

The Collector exposes metrics at `/metrics`, scraped by Prometheus every five seconds. Only `service.name`, `service.namespace`, `service.version`, and `deployment.environment.name` are allowed as additional metric resource labels. The default `instance` label retains `service.instance.id` to distinguish processes. Loki uses the same four resource attributes as index labels, retaining `service.instance.id` and `trace_id` as structured metadata. Native OTLP logs use TSDB schema `v13`, and Tempo accepts OTLP HTTP traces. Collector batches are sent at intervals of at most one second.

Grafana automatically provisions `Prometheus`, `Loki`, and `Tempo` data sources. Loki's `trace_id` links to Tempo traces, and Tempo can query Loki logs for the same service and trace ID. Prometheus exemplar `trace_id` values also link to the Tempo data source.

## Installation and queries

Render all components or only the ones you need:

```sh
kubectl kustomize deploy/components
kubectl kustomize deploy/components/otel-collector
```

To install into an existing namespace, specify the target context and namespace:

```sh
kubectl --context <context-name> -n <namespace> apply -k deploy/components
kubectl --context <context-name> -n <namespace> rollout status deployment/otel-collector
kubectl --context <context-name> -n <namespace> rollout status statefulset/redis
```

Replace `<context-name>` and `<namespace>` with actual environment values. Deploy Collector, Prometheus, Tempo, Loki, and Grafana in the same namespace. To connect to another namespace or external backends, also change the Service addresses in each configuration file.

Access the Grafana UI through local port forwarding:

```sh
kubectl --context <context-name> -n <namespace> \
  port-forward --address 127.0.0.1 service/grafana 13000:3000
```

Open `http://127.0.0.1:13000` in a browser and select a data source in Explore. After the application sends data, query Prometheus metrics, Loki logs, and Tempo traces.

## kind and Docker E2E validation

The [kind test environment](kind-test-environment.md) deploys all components in the `token-router-test` namespace.

```sh
make kind-render
make kind-up
make kind-test
make kind-status
```

`make kind-up` waits for the Redis StatefulSet and all observability Deployments to become ready. `make kind-test` checks cluster DNS and HTTP, Redis `PING`, `SET`, and `GET`, observability Service health checks, and the Collector's three OTLP HTTP ingestion paths. Health checks do not guarantee that stored data can be queried.

[Ginkgo E2E tests](end-to-end-tests.md) mount the same Collector, Prometheus, Tempo, and Loki configuration files read-only in Docker Compose. They generate actual application requests and query metrics, traces, and logs from each backend. The Grafana UI is outside the application E2E path.

## Redis storage

Redis is a StatefulSet with one replica. Clients connect to `redis:6379`, while the StatefulSet uses `redis-headless:6379`. Data lives in `/data`; the PVC is named `data-redis-0`, has size `1Gi`, and uses `ReadWriteOnce`. The cluster needs a default StorageClass and dynamic volume provisioner.

Redis uses `maxmemory 128mb` and `noeviction`, with a Pod memory limit of `256Mi`. AOF is enabled with `appendfsync everysec`, and RDB snapshots are disabled. Startup, readiness, and liveness probes use `redis-cli -e ping`.

The Pod UID is `999`; GID and `fsGroup` are `1000`. The container root filesystem is read-only. PVCs are retained when the StatefulSet is deleted or scaled down; redeploying under the same name in the same namespace reuses them. Namespace or PVC deletion is a separate operation. `make kind-down` deletes the cluster and all data within it.

## Configuration references

- Collector `v0.162.0`: [Prometheus exporter configuration](https://github.com/open-telemetry/opentelemetry-collector-contrib/blob/v0.162.0/exporter/prometheusexporter/README.md), [health check extension](https://github.com/open-telemetry/opentelemetry-collector-contrib/blob/v0.162.0/extension/healthcheckextension/README.md).
- Tempo `v2.10.8`: [Official local configuration](https://github.com/grafana/tempo/blob/v2.10.8/example/docker-compose/local/tempo.yaml). `2.10.x` provides a single-process setup without Kafka and is a maintained series according to the [supported version guidance](https://grafana.com/docs/tempo/latest/set-up-for-tracing/setup-tempo/recommended-versions/).
- Loki `v3.7.8`: [Official local configuration](https://github.com/grafana/loki/blob/v3.7.8/cmd/loki/loki-local-config.yaml), [OTLP log ingestion](https://grafana.com/docs/loki/latest/send-data/otel/).
- Grafana `v13.2.3`: [Default configuration](https://github.com/grafana/grafana/blob/v13.2.3/conf/defaults.ini), [data source provisioning](https://grafana.com/docs/grafana/latest/administration/provisioning/#data-sources).
- Redis `7.4.11`: [Server configuration](https://github.com/redis/redis/blob/7.4.11/redis.conf), [image UID and GID definitions](https://github.com/redis/docker-library-redis/blob/74654c612ee06275377d483dc4e134e57b463e9e/alpine/Dockerfile).
