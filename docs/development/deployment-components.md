# 배포 컴포넌트와 OpenTelemetry

`deploy/components/`는 Redis와 OpenTelemetry 수집 및 조회 환경을 제공한다. 각 하위 디렉토리는 독립적인 Kustomize 구성이며, 상위 `kustomization.yaml`에서 전체 컴포넌트를 모은다. Namespace는 배포 환경에서 지정한다.

## 컴포넌트와 데이터 경로

```text
token-router
  metrics, logs, traces -> otel-collector:4318 (OTLP HTTP)
    metrics -> otel-collector:8889 <- prometheus:9090
    logs    -> loki:3100/otlp
    traces  -> tempo:4318

grafana:3000 -> prometheus:9090, loki:3100, tempo:3200
```

| 디렉토리 | 이미지 버전 | Service와 용도 | 저장소 |
| --- | --- | --- | --- |
| `redis/` | `redis:7.4.11-alpine3.21` | `redis:6379`, 요청 버킷 | `1Gi` PVC |
| `otel-collector/` | `otel/opentelemetry-collector-contrib:0.162.0` | `4318`, OTLP HTTP 수신. `8889`, Prometheus 수집. `13133`, 상태 검사 | 메모리 큐 |
| `prometheus/` | `prom/prometheus:v3.15.0` | `prometheus:9090`, 메트릭 조회 | `emptyDir`, 보존 기간 `24h` |
| `tempo/` | `grafana/tempo:2.10.8` | `tempo:4318`, OTLP HTTP 수신. `tempo:3200`, 트레이스 조회 | `emptyDir`, 보존 기간 `24h` |
| `loki/` | `grafana/loki:3.7.8` | `loki:3100`, OTLP 로그 수신과 조회 | `emptyDir`, 보존 기간 `24h` |
| `grafana/` | `grafana/grafana:13.2.3` | `grafana:3000`, 데이터 조회 UI | `emptyDir` |

모든 이미지는 태그와 digest로 고정한다. 각 컴포넌트의 `service.yaml`과 `deployment.yaml` 또는 `statefulset.yaml`에서 포트, 상태 검사, 리소스 제한과 볼륨을 확인할 수 있다. 설정 파일은 `config.yaml`이며, Grafana는 `datasources.yaml`, Redis는 `redis.conf`를 사용한다. Kustomize가 생성하는 ConfigMap 이름은 설정 내용에 따라 바뀌므로 설정 변경 후 적용하면 Pod가 교체된다.

이 구성은 단일 인스턴스의 개발 및 테스트 환경이다. 모든 Service는 `ClusterIP`이고, 인증과 TLS를 설정하지 않는다. Grafana는 익명 `Viewer` 접근을 사용하며 로그인과 초기 관리자 생성을 비활성화한다. 관측 데이터의 `emptyDir`은 Pod 교체 시 삭제된다. 표의 보존 기간은 설정된 정책이며 정확한 삭제 시점을 보장하지 않는다. 운영 환경에서는 인증, TLS, 영구 저장소, 보존 정책과 리소스 한도를 환경에 맞게 설정한다.

## 애플리케이션 연결

같은 Namespace의 애플리케이션에 다음 환경 변수를 설정한다. `TOKEN_ROUTER_TELEMETRY_OTLP_ENDPOINT`는 `/v1/traces` 같은 개별 신호 경로를 포함하지 않는 기본 URL이다.

```yaml
env:
  - name: TOKEN_ROUTER_TELEMETRY_OTLP_ENABLED
    value: "true"
  - name: TOKEN_ROUTER_TELEMETRY_OTLP_ENDPOINT
    value: http://otel-collector:4318
```

애플리케이션은 JSON 로그를 출력하고 활성화된 메트릭, 로그와 트레이스를 Collector로 전송한다. `service.name`, `service.instance.id`, `service.version`, `deployment.environment.name`은 애플리케이션의 `telemetry` 설정으로 구성한다. `service.namespace`는 현재 애플리케이션에서 지정하지 않지만 Collector는 이 속성을 가진 다른 서비스의 데이터도 수집할 수 있다. 설정 키와 우선순위는 [프록시 서버 개발과 실행](proxy-server.md)을 참고한다.

Collector는 메트릭을 `/metrics`로 노출하며 Prometheus가 5초마다 수집한다. `service.name`, `service.namespace`, `service.version`, `deployment.environment.name`만 메트릭의 추가 resource label로 허용한다. 기본 `instance` label에는 `service.instance.id`를 유지하므로 서로 다른 프로세스의 메트릭을 구분한다. Loki는 같은 네 resource attribute를 인덱스 label로 사용하고, `service.instance.id`와 `trace_id`는 structured metadata로 보관한다. native OTLP 로그는 TSDB schema `v13`에 저장하고, Tempo는 OTLP HTTP 트레이스를 받는다. Collector 배치는 최대 1초 간격으로 전송한다.

Grafana에는 `Prometheus`, `Loki`, `Tempo` datasource를 자동 등록한다. Loki의 `trace_id`에서 Tempo 트레이스를 열 수 있고, Tempo에서는 같은 서비스와 trace ID의 Loki 로그를 조회할 수 있다. Prometheus exemplar의 `trace_id`도 Tempo datasource와 연결한다.

## 설치와 조회

전체 컴포넌트 또는 필요한 컴포넌트만 렌더링한다.

```sh
kubectl kustomize deploy/components
kubectl kustomize deploy/components/otel-collector
```

기존 Namespace에 설치하려면 대상 context와 Namespace를 지정한다.

```sh
kubectl --context <context-name> -n <namespace> apply -k deploy/components
kubectl --context <context-name> -n <namespace> rollout status deployment/otel-collector
kubectl --context <context-name> -n <namespace> rollout status statefulset/redis
```

`<context-name>`과 `<namespace>`는 실제 환경 값으로 바꾼다. Collector, Prometheus, Tempo, Loki, Grafana는 같은 Namespace에 배포한다. 다른 Namespace나 외부 백엔드에 연결하려면 각 설정 파일의 Service 주소도 변경한다.

Grafana UI는 로컬 포트 전달로 조회한다.

```sh
kubectl --context <context-name> -n <namespace> \
  port-forward --address 127.0.0.1 service/grafana 13000:3000
```

브라우저에서 `http://127.0.0.1:13000`에 접속하고 Explore에서 datasource를 선택한다. 애플리케이션에서 데이터를 전송한 뒤 Prometheus 메트릭, Loki 로그와 Tempo 트레이스를 조회할 수 있다.

## kind와 Docker E2E 검증

[Kind 테스트 환경](kind-test-environment.md)은 전체 컴포넌트를 `token-router-test` Namespace에 배포한다.

```sh
make kind-render
make kind-up
make kind-test
make kind-status
```

`make kind-up`은 Redis StatefulSet과 모든 관측 Deployment가 준비될 때까지 기다린다. `make kind-test`는 클러스터 DNS와 HTTP, Redis의 `PING`, `SET`, `GET`, 관측 Service 상태 검사, Collector의 세 OTLP HTTP 수신 경로를 확인한다. 상태 검사는 저장된 데이터의 조회까지 보장하지 않는다.

[Ginkgo E2E 테스트](end-to-end-tests.md)는 같은 Collector, Prometheus, Tempo, Loki 설정 파일을 Docker Compose에서 읽기 전용으로 마운트한다. 실제 애플리케이션 요청을 발생시켜 메트릭, 트레이스와 로그를 각 백엔드에서 조회한다. Grafana UI는 애플리케이션 E2E 경로에 포함하지 않는다.

## Redis 저장소

Redis는 replica 1개의 StatefulSet이다. 클라이언트는 `redis:6379`로 연결하고, StatefulSet은 `redis-headless:6379`를 사용한다. 데이터는 `/data`에 저장하며 PVC 이름은 `data-redis-0`, 크기는 `1Gi`, 접근 모드는 `ReadWriteOnce`이다. 클러스터에 기본 StorageClass와 동적 볼륨 provisioner가 필요하다.

`maxmemory 128mb`와 `noeviction`을 사용하고 Pod 메모리 한도는 `256Mi`이다. AOF를 활성화하고 `appendfsync everysec`을 사용하며 RDB snapshot은 비활성화한다. startup, readiness, liveness probe는 `redis-cli -e ping`이다.

Pod의 UID는 `999`, GID와 `fsGroup`은 `1000`이다. 컨테이너의 루트 파일 시스템은 읽기 전용이다. StatefulSet 삭제나 replica 축소 시 PVC를 유지하며 같은 Namespace에 같은 이름으로 재배포하면 유지한 PVC를 다시 사용한다. Namespace 또는 PVC 삭제는 별도 작업이다. `make kind-down`은 클러스터와 그 안의 모든 데이터를 삭제한다.

## 설정 근거

- Collector `v0.162.0`: [Prometheus exporter 설정](https://github.com/open-telemetry/opentelemetry-collector-contrib/blob/v0.162.0/exporter/prometheusexporter/README.md), [health check extension](https://github.com/open-telemetry/opentelemetry-collector-contrib/blob/v0.162.0/extension/healthcheckextension/README.md).
- Tempo `v2.10.8`: [공식 로컬 실행 설정](https://github.com/grafana/tempo/blob/v2.10.8/example/docker-compose/local/tempo.yaml). `2.10.x`는 Kafka 없는 단일 프로세스 구성을 제공하며, [유지보수 버전 안내](https://grafana.com/docs/tempo/latest/set-up-for-tracing/setup-tempo/recommended-versions/)에 따라 유지되는 계열이다.
- Loki `v3.7.8`: [공식 로컬 실행 설정](https://github.com/grafana/loki/blob/v3.7.8/cmd/loki/loki-local-config.yaml), [OTLP 로그 수집](https://grafana.com/docs/loki/latest/send-data/otel/).
- Grafana `v13.2.3`: [기본 설정](https://github.com/grafana/grafana/blob/v13.2.3/conf/defaults.ini), [datasource provisioning](https://grafana.com/docs/grafana/latest/administration/provisioning/#data-sources).
- Redis `7.4.11`: [서버 설정](https://github.com/redis/redis/blob/7.4.11/redis.conf), [이미지 UID와 GID 정의](https://github.com/redis/docker-library-redis/blob/74654c612ee06275377d483dc4e134e57b463e9e/alpine/Dockerfile).
