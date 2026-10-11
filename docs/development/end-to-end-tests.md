# Ginkgo E2E 테스트

[`tests/e2e/`](../../tests/e2e/)는 Ginkgo v2와 Gomega로 빌드된 프록시 실행 파일을 검증한다. 각 시나리오는 서버를 별도 프로세스로 시작하고 HTTP 요청, 응답, 메트릭, 로그, trace와 종료 상태를 확인한다. upstream은 테스트 프로세스의 `httptest.Server`로 구성하고 Redis는 실제 서버를 사용한다.

Collector, Prometheus, Tempo와 Loki는 실제 서버를 실행한다. OpenAI API key와 외부 OpenAI 호출은 필요 없다.

## Docker에서 실행

Docker, Docker Compose v2, GNU Make와 Bash가 필요하다. 저장소 루트에서 실행한다.

```sh
make e2e
```

[`tests/e2e/compose.yaml`](../../tests/e2e/compose.yaml)은 E2E runner와 다음 서비스를 별도 컨테이너로 시작한다. 이미지는 버전과 digest로 고정한다.

| 서비스 | 역할 |
| --- | --- |
| Redis | 여러 프록시 프로세스가 공유하는 실제 request bucket |
| OpenTelemetry Collector | 프록시의 OTLP/HTTP metrics, logs와 traces 수신, backend별 전달 |
| Prometheus | Collector가 노출한 메트릭 scrape와 PromQL 조회 |
| Tempo | trace 저장과 trace ID 조회 |
| Loki | OTLP log 저장과 LogQL 조회 |

runner 안에서 테스트 upstream과 프록시 프로세스가 실행되고, 다른 서비스는 Compose 네트워크로 연결한다. Redis health check 이후 runner가 시작되며 suite 준비 단계에서 Collector와 각 backend의 readiness endpoint를 확인한다. 테스트 포트를 호스트에 공개하거나 Docker socket을 runner에 마운트하지 않는다.

Prometheus, Tempo와 Loki의 데이터는 각 프로세스 UID에 맞춘 `tmpfs`에 저장한다. 각 저장소의 상한은 256 MiB이며 컨테이너 종료 시 데이터를 제거한다.

Collector, Prometheus, Tempo와 Loki는 [`deploy/components/`](../../deploy/components/)의 설정을 그대로 사용한다. 메트릭 검증은 Collector 응답만 확인하는 데 그치지 않고 Prometheus에 저장된 값을 조회한다. Trace는 Tempo의 `/api/traces/{traceID}`, 로그는 Loki의 `/loki/api/v1/query_range`에서 확인한다.

[`tests/e2e/run.sh`](../../tests/e2e/run.sh)는 실행마다 고유한 Compose project를 만들고 종료할 때 해당 project의 컨테이너, 네트워크, 저장 볼륨과 자동 생성한 runner 이미지 태그를 정리한다. 내려받은 서비스 이미지는 유지한다. 테스트가 실패하면 최근 컨테이너 로그를 출력하고 명령도 실패 상태로 종료한다. 초기 실행에는 Go 모듈, 빌드 이미지와 서비스 이미지 다운로드가 필요하다.

## 테스트 구성

E2E Go 파일에는 `e2e` build tag가 있다. `make test`는 일반 Go 테스트를 실행하며 E2E 실행은 `make e2e`로 분리한다. Ginkgo CLI 버전은 [`go.mod`](../../go.mod)의 `tool` 선언과 모듈 의존성으로 관리한다.

runner는 일반 빌드에서 생성한 실행 파일을 사용하고 Ginkgo 테스트 코드에는 race detector를 적용한다. suite의 전체 제한 시간은 10분이며 pending 시나리오나 빈 suite도 실패로 처리한다.

프록시 프로세스는 독립적인 임시 디렉토리에서 시작한다. 테스트가 만든 설정만 적용하도록 기존 `TOKEN_ROUTER_`와 `OTEL_` 환경 변수를 제거하고, 리스너는 운영체제가 선택하는 빈 포트를 사용한다. Redis 테스트는 고유한 key를 생성하며 종료할 때 자신이 만든 key만 삭제한다. Telemetry 테스트는 시나리오마다 고유한 `service.name`과 trace ID를 사용한다.

## 시나리오

총 20개 시나리오를 실행한다.

| 범위 | 검증 내용 |
| --- | --- |
| 설정 우선순위 | upstream 인증이 기본값, YAML, 환경 변수, CLI 인자 순으로 적용되는지 확인 |
| HTTP 전달 | 메서드, base URL 경로, query, 본문, OpenAI 조직과 프로젝트 헤더 및 upstream 오류 응답 보존 |
| 상태와 메트릭 | `/healthz`, `/readyz`, 별도 리스너의 사용자 지정 메트릭 경로와 Prometheus 요청 counter |
| upstream timeout | 응답 헤더 대기 시간 초과 시 `504`와 `upstream_timeout` 오류, upstream 요청 취소 |
| 메모리 버킷 | burst 소진 후 `429`와 `Retry-After`, 거절된 요청이 upstream에 전달되지 않는지 확인 |
| Redis 공유 버킷 | 서버 프로세스 2개에 동시 요청 30개를 보내 공유 burst 5개만 허용하는지 확인 |
| Redis 명령 실패 | 테스트 key의 타입 오류가 발생하면 `503`을 반환하고 upstream 요청을 차단하는지 확인 |
| 잘못된 설정 | 알 수 없는 YAML 필드, 지원하지 않는 backend, `0s` shutdown timeout을 리스너 생성 전에 거절 (3개 table entry) |
| SSE 취소 | 첫 이벤트를 받은 후 두 번째 이벤트를 생성해 점진적 전달을 확인하고 클라이언트 취소를 upstream에 전달 |
| 정상 종료 | 진행 중인 SSE 응답을 `SIGTERM` 이후 완료하고 종료 코드 `0` 반환 |
| 종료 시간 초과 | 완료되지 않은 SSE를 제한 시간 후 끊고 upstream 요청을 취소하며 종료 코드 `1` 반환 |
| Telemetry 연결 | 수신한 W3C `traceparent`, server span, upstream client span의 부모 관계, JSON stderr와 Loki 로그의 `trace_id`, `span_id`, Prometheus 요청 counter 확인 |
| 민감한 요청 정보 | API key, query 값, 요청 본문이 수집한 trace, Loki 로그와 JSON stderr에 포함되지 않는지 확인 |
| 개별 signal 설정 | metrics만, traces만, logs만 활성화하고 나머지 backend에 해당 데이터가 없는지 확인 (3개 table entry) |
| 오류 trace | upstream `503`일 때 server span과 client span이 오류 상태로 저장되는지 확인 |
| Telemetry 비활성화 | OTLP 데이터를 내보내지 않으면서 JSON access log와 기존 Prometheus 리스너를 유지하는지 확인 |
| OTLP rate limit 메트릭 | 허용과 거절 counter 및 HTTP `429` counter를 Prometheus에서 확인 |
| 종료 시 telemetry flush | SSE 응답을 종료할 때 최종 span, log와 export interval이 `1h`인 메트릭까지 backend에 전달되는지 확인 |

HTTP와 설정 시나리오는 [`proxy_test.go`](../../tests/e2e/proxy_test.go), SSE와 종료 시나리오는 [`streaming_test.go`](../../tests/e2e/streaming_test.go), telemetry 시나리오는 [`telemetry_test.go`](../../tests/e2e/telemetry_test.go)에 있다. Backend 반영은 polling으로 확인하고, 비활성화된 signal은 Collector batch와 Prometheus scrape interval을 포함하는 시간 동안 데이터가 없는지 확인한다.

## 호스트에서 실행

Docker Compose를 사용하면 실행 파일 빌드와 모든 의존 서비스 준비를 함께 처리한다. 호스트에서 직접 실행하려면 Go 1.27.2, 빌드된 실행 파일과 접근 가능한 테스트용 서비스를 준비하고 다음 환경 변수를 설정한다.

| 환경 변수 | 값 |
| --- | --- |
| `TOKEN_ROUTER_E2E_BINARY` | 프록시 실행 파일의 절대 경로 |
| `TOKEN_ROUTER_E2E_REDIS_ADDRESS` | 실제 Redis의 `host:port` |
| `TOKEN_ROUTER_E2E_OTLP_ENDPOINT` | Collector OTLP/HTTP base URL, 예: `http://127.0.0.1:4318` |
| `TOKEN_ROUTER_E2E_COLLECTOR_HEALTH_URL` | Collector readiness URL, 예: `http://127.0.0.1:13133` |
| `TOKEN_ROUTER_E2E_PROMETHEUS_URL` | Prometheus HTTP base URL, 예: `http://127.0.0.1:9090` |
| `TOKEN_ROUTER_E2E_TEMPO_URL` | Tempo HTTP base URL, 예: `http://127.0.0.1:3200` |
| `TOKEN_ROUTER_E2E_LOKI_URL` | Loki HTTP base URL, 예: `http://127.0.0.1:3100` |

환경 변수를 모두 설정한 뒤 실행한다.

```sh
make build-local
export TOKEN_ROUTER_E2E_BINARY="$(pwd)/bin/token-router"
go tool ginkgo --tags=e2e --race --timeout=10m \
  --fail-on-pending --fail-on-empty ./tests/e2e
```

필수 환경 변수가 없거나 서비스에 접근할 수 없으면 suite 준비 단계가 실패한다. 테스트 backend는 인증과 TLS를 사용하지 않으며 Redis key와 고유한 서비스 이름에 해당하는 데이터를 생성한다. 배포 구성은 [배포 컴포넌트와 OpenTelemetry](deployment-components.md)를 참고한다.
