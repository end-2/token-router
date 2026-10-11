# 애플리케이션 설정

## 적용 순서

설정 우선순위는 다음과 같다. 뒤쪽 값이 앞쪽 값을 덮어쓴다.

1. [`config.Defaults`](../../src/internal/config/config.go)의 기본값
2. YAML 파일
3. `TOKEN_ROUTER_` 접두사의 환경 변수
4. 명시적으로 전달한 CLI 인자

`--config`가 있으면 해당 파일을 읽는다. 이 인자가 없으면 `TOKEN_ROUTER_CONFIG`, 이후 현재 작업 디렉토리의 `config.yaml` 순서로 경로를 결정한다. 암시적인 `config.yaml`이 없을 때는 기본값으로 계속 진행한다. 인자 또는 환경 변수로 지정한 파일이 없거나 읽을 수 없으면 시작하지 않는다.

CLI 기본값은 YAML 또는 환경 변수 값을 덮어쓰지 않는다. 명시적인 `false`, `0`, 빈 문자열은 해당 설정이 허용하는 범위에서 적용한다. YAML의 알 수 없는 필드, 중복 키, `null`, 다중 문서, 잘못된 타입과 duration은 오류다. 빈 파일도 오류이며 기본값만 사용하려면 `{}`를 작성한다. 설정값을 출력하는 명령은 제공하지 않으며 파싱 오류에 비밀 값을 포함하지 않는다.

## 설정 계층

모든 기본값과 YAML 키는 [`config.yaml`](../../config.yaml)에 있다. 계층별 책임은 다음과 같다.

| 계층 | 책임 |
| --- | --- |
| `server` | API 리스너 주소, HTTP timeout, 헤더 크기, 종료 대기 시간 |
| `upstream` | 고정 목적지 URL, 인증 헤더, 연결 풀과 upstream timeout |
| `rate_limit` | 활성화 여부, `memory` 또는 `redis` 전략, 초당 요청 수와 burst |
| `rate_limit.redis` | Redis 주소, 인증, DB, 공용 key, TLS와 I/O timeout |
| `metrics` | Prometheus 활성화 여부, 별도 리스너 주소와 경로 |
| `log` | `json` 또는 `text` 출력, `debug`, `info`, `warn`, `error` 레벨 |
| `telemetry` | OpenTelemetry 활성화, service resource와 export 종료 대기 시간 |
| `telemetry.otlp` | 세 신호가 공유하는 OTLP HTTP base URL과 전송 timeout |
| `telemetry.traces` | trace export 활성화와 root trace sampling 비율 |
| `telemetry.metrics` | OTLP metric export 활성화와 전송 주기 |
| `telemetry.logs` | 구조화된 로그의 OTLP export 활성화 |

모든 leaf 설정은 환경 변수와 CLI 인자로 변경할 수 있다. YAML의 점과 밑줄을 환경 변수에서는 밑줄로, CLI에서는 하이픈으로 연결한다.

| YAML 키 | 환경 변수 | CLI |
| --- | --- | --- |
| `server.address` | `TOKEN_ROUTER_SERVER_ADDRESS` | `--server-address` |
| `upstream.base_url` | `TOKEN_ROUTER_UPSTREAM_BASE_URL` | `--upstream-base-url` |
| `upstream.api_key` | `TOKEN_ROUTER_UPSTREAM_API_KEY` | `--upstream-api-key` |
| `rate_limit.backend` | `TOKEN_ROUTER_RATE_LIMIT_BACKEND` | `--rate-limit-backend` |
| `rate_limit.redis.address` | `TOKEN_ROUTER_RATE_LIMIT_REDIS_ADDRESS` | `--rate-limit-redis-address` |
| `metrics.enabled` | `TOKEN_ROUTER_METRICS_ENABLED` | `--metrics-enabled=false` |
| `telemetry.enabled` | `TOKEN_ROUTER_TELEMETRY_ENABLED` | `--telemetry-enabled` |
| `telemetry.otlp.endpoint` | `TOKEN_ROUTER_TELEMETRY_OTLP_ENDPOINT` | `--telemetry-otlp-endpoint` |
| `telemetry.traces.sample_ratio` | `TOKEN_ROUTER_TELEMETRY_TRACES_SAMPLE_RATIO` | `--telemetry-traces-sample-ratio` |
| `telemetry.metrics.export_interval` | `TOKEN_ROUTER_TELEMETRY_METRICS_EXPORT_INTERVAL` | `--telemetry-metrics-export-interval` |

`go run ./src/cmd/token-router --help`는 전체 CLI와 대응 환경 변수 이름을 보여준다. 예를 들어 다음 명령은 YAML을 읽은 뒤 환경 변수의 주소, CLI의 burst를 적용한다.

```sh
TOKEN_ROUTER_SERVER_ADDRESS=127.0.0.1:8081 \
  go run ./src/cmd/token-router serve --config config.yaml --rate-limit-burst 50
```

불리언 인자는 `--metrics-enabled`처럼 값 없이 전달하면 `true`가 된다. `false`를 지정할 때는 `--metrics-enabled=false`처럼 등호를 사용한다. `TOKEN_ROUTER_CONFIG` 또는 `--config`에 빈 경로를 명시하면 오류다.

## Upstream 경로와 인증

기본 `upstream.base_url`은 `https://api.openai.com`이다. 수신한 `/v1/...` 경로를 목적지 URL의 base path 뒤에 붙인다. OpenAI로 연결할 때는 base URL에 `/v1`을 중복해서 넣지 않는다. 예를 들어 base URL이 `http://localhost:9000/gateway`이면 `/v1/models` 요청은 `/gateway/v1/models`로 전달된다.

base URL은 호스트가 있는 `http` 또는 `https` URL이어야 하며 사용자 정보, query 또는 fragment를 포함할 수 없다. `upstream.max_idle_conns`와 `upstream.max_idle_conns_per_host`는 양수다.

`upstream.api_key`가 비어 있으면 클라이언트의 `Authorization`을 전달한다. 값이 있으면 `Bearer <configured-key>`로 교체한다. `upstream.organization`과 `upstream.project`도 값이 있을 때 해당 OpenAI 헤더를 덮어쓴다. key와 password는 환경 변수로 주입할 수 있으며 저장소의 예시 YAML에는 실제 값을 기록하지 않는다.

## Timeout과 스트리밍

duration 값은 `500ms`, `10s`, `2m`, `0s` 같은 Go duration 문자열을 사용한다. 음수는 허용하지 않는다.

- `server.read_header_timeout`: 클라이언트 요청 헤더 수신 시간이다.
- `server.read_timeout`: 요청 본문 수신까지 포함하는 시간이다. 기본값 `0s`는 전체 읽기 제한을 비활성화한다.
- `server.write_timeout`: 응답 쓰기 시간이다. 기본값 `0s`는 긴 SSE 응답을 허용한다.
- `server.idle_timeout`: 클라이언트 keep-alive 연결의 다음 요청 대기 시간이다.
- `server.shutdown_timeout`: 종료 시 활성 요청을 기다리는 총 시간이며 양수여야 한다.
- `upstream.dial_timeout`: TCP 연결과 TLS handshake 제한이다.
- `upstream.response_header_timeout`: upstream 응답 헤더 대기 시간이다.
- `upstream.request_timeout`: 전체 요청과 SSE 스트림을 포함하는 제한이다. 기본값 `0s`는 비활성화한다.
- `upstream.idle_conn_timeout`: upstream 유휴 연결 유지 시간이다.
- `rate_limit.redis.dial_timeout`, `read_timeout`, `write_timeout`: Redis 연결과 I/O 제한이다. Redis 전략에서는 양수여야 하며 각 명령에는 이 세 값의 최댓값을 총 제한 시간으로 추가 적용한다.

전체 요청 timeout은 응답을 이미 시작한 SSE에도 적용된다. 응답 헤더를 보낸 뒤 오류가 발생하면 HTTP 상태 코드를 바꿀 수 없으므로 연결을 종료한다.

## OpenTelemetry 설정

`telemetry.enabled`의 기본값은 `false`다. 활성화하면 `traces.enabled`, `metrics.enabled`, `logs.enabled`로 각 신호를 선택하며 세 신호의 기본값은 모두 `true`다. `metrics.enabled`는 기존 Prometheus 리스너를 제어하고, `telemetry.metrics.enabled`는 OTLP 전송을 제어한다. 두 설정은 독립적이다.

`telemetry.otlp.endpoint`는 `http://otel-collector:4318` 같은 OTLP HTTP base URL이다. 신호별 `/v1/traces`, `/v1/metrics`, `/v1/logs` 경로는 exporter에서 추가한다. `https`를 사용할 수 있으며 사용자 정보, query와 fragment는 허용하지 않는다. 전송 timeout의 기본값은 `5s`, 메트릭 전송 주기는 `15s`다.

`telemetry.service_name`, `telemetry.service_version`, `telemetry.environment`는 모든 신호의 resource에 적용한다. 기본값은 각각 `token-router`, `dev`, `development`다. `telemetry.traces.sample_ratio`는 0 이상 1 이하의 유한한 값이며 기본값은 `1`이다. root trace에 비율을 적용하고 유효한 부모 span의 sampling 결정은 따른다.

`telemetry.service_instance_id`가 비어 있으면 프로세스 시작마다 고유 ID를 생성한다. 직접 지정할 때는 실행 중인 인스턴스마다 다른 값을 사용한다. 이 값으로 같은 서비스의 여러 프로세스에서 전송한 메트릭을 구분한다.

HTTP 요청 drain 이후 `telemetry.shutdown_timeout`까지 export를 마무리한다. 기본값은 `5s`다. 배포의 종료 대기 시간은 `server.shutdown_timeout`과 `telemetry.shutdown_timeout`의 합보다 길게 설정한다.

JSON 로그, trace 연결과 수집 구성은 [OpenTelemetry 관측 구성](../operations/observability.md)을 참고한다.

## 요청 수 제한

버킷은 요청을 허용할 때마다 한 단위를 소비한다. `requests_per_second`는 초당 보충량, `burst`는 최대 용량이다. 제한이 활성화되어 있으면 두 값은 양수여야 한다. 보충량에는 `NaN`과 무한대를 사용할 수 없으며 전체 버킷 보충 시간은 Go `time.Duration`으로 표현할 수 있어야 한다. 시작 시 버킷은 가득 차 있다. 제한을 넘으면 대기열 없이 `429`와 초 단위의 `Retry-After`를 반환한다. health, readiness와 메트릭은 버킷을 소비하지 않는다.

`memory`는 프로세스마다 독립적이다. `redis`는 같은 DB와 key를 사용하는 프로세스끼리 공유하며 Redis 서버 시간을 사용한다. Redis key는 버킷을 완전히 채울 수 있는 유휴 시간이 지난 뒤 만료된다. `rate_limit.enabled=false`이면 Redis 연결 없이 제한을 비활성화한다. backend 선택과 유효 설정은 시작할 때 결정한다.
