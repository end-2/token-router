# 프록시 서버 구성

## 패키지 경계

Go module은 저장소 루트에 있고 애플리케이션 코드는 `src/` 아래에 둔다.

| 경로 | 책임 |
| --- | --- |
| [`src/cmd/token-router`](../../src/cmd/token-router/main.go) | 종료 신호와 exit code 처리 |
| [`src/internal/cli`](../../src/internal/cli/root.go) | Cobra 명령과 명시적 인자 수집 |
| [`src/internal/config`](../../src/internal/config/config.go) | 설정 계층 병합, 타입 변환과 검증 |
| [`src/internal/app`](../../src/internal/app/app.go) | 의존성 생성, Echo 라우팅과 리스너 수명 관리 |
| [`src/internal/proxy`](../../src/internal/proxy/proxy.go) | 표준 `httputil.ReverseProxy`와 upstream 연결 풀 |
| [`src/internal/bucket`](../../src/internal/bucket/bucket.go) | `Limiter` 인터페이스와 memory, Redis 구현 |
| [`src/internal/observability`](../../src/internal/observability/metrics.go) | 인스턴스별 Prometheus registry, OpenTelemetry providers와 JSON 로그 |

`main`은 process 신호를 context로 바꾸고 CLI를 실행한다. CLI는 설정을 로드한 뒤 `app.Run`을 호출한다. `app`이 실제 구현을 조합하며 요청 처리 코드는 설정 파일이나 환경 변수를 직접 읽지 않는다.

## HTTP 프록시

Echo가 `/v1`과 `/v1/*`를 프록시로 연결한다. 메서드, 본문, query와 upstream 상태 코드를 전달한다. SSE는 전체 응답을 버퍼링하지 않고 flush하며 클라이언트 연결 종료는 upstream context에 전달된다. JSON API별 모델이나 응답 형식을 해석하지 않는다.

[`proxy.Handler.ServeHTTP`](../../src/internal/proxy/proxy.go)는 HTTP/1.x에서 `http.ResponseController.EnableFullDuplex`를 사용해 요청 본문 읽기와 응답 쓰기를 함께 허용한다. upstream transport가 요청 본문을 읽는 동안 응답 flush가 입력 본문을 닫지 않도록 한다.

목적지는 설정의 고정 URL이다. `Forwarded`와 `X-Forwarded-*`는 제거하고 `HTTP_PROXY` 같은 프로세스 환경 변수로 목적지 경로를 변경하지 않는다. 애플리케이션 수준 재시도와 redirect 추적은 추가하지 않는다. 표준 `http.Transport`는 재사용 연결 실패 시 재전송 가능한 요청을 재시도할 수 있다. WebSocket과 HTTP protocol upgrade는 지원하지 않는다.

응답 시작 전 연결 오류는 `502`, timeout은 `504`로 변환한다. upstream이 반환한 API 오류와 상태 코드는 그대로 전달한다. 응답 시작 후 전송 오류는 연결 종료로 표현된다. 자체 오류와 로그에 upstream URL, API key, 요청 본문을 기록하지 않는다.

## 전략 인터페이스

`bucket.Limiter`는 다음 동작을 제공한다.

```go
type Limiter interface {
    Allow(context.Context) (Decision, error)
    Close() error
}
```

`Decision`에는 허용 여부와 재시도 대기 시간이 있다. `bucket.New`가 설정에 따라 구현을 선택한다. memory는 `golang.org/x/time/rate`, Redis는 `go-redis`와 원자적인 Lua script를 사용한다. Redis mutation의 자동 재시도를 비활성화해 결과를 알 수 없는 요청의 중복 차감을 피한다.

새로운 버킷 저장소를 추가할 때는 이 인터페이스를 구현하고 설정 검증과 factory의 분기를 추가한다. HTTP handler는 변경할 필요가 없다. 현재 모든 API 요청은 하나의 전역 버킷을 공유한다. 사용자별 제한과 OpenAI token usage 집계는 구현하지 않는다.

## 메트릭과 상태 검사

| 경로 또는 메트릭 | 의미 |
| --- | --- |
| `/healthz` | 프로세스 HTTP handler의 생존 여부 |
| `/readyz` | HTTP 리스너 시작과 종료 상태, upstream 또는 Redis의 지속적인 상태 검사는 하지 않음 |
| `token_router_http_requests_total` | 처리 종료된 요청 수, `method`, `route`, `status` label |
| `token_router_http_request_duration_seconds` | SSE 전체 수명을 포함한 요청 처리 시간 |
| `token_router_http_requests_in_flight` | 현재 처리 중인 요청 수 |
| `token_router_rate_limit_decisions_total` | `allowed`, `denied`, `error` 결과별 버킷 호출 수 |
| `go_*`, `process_*` | Prometheus 기본 Go runtime과 프로세스 정보 |

`route`는 `/v1/*` 같은 등록된 route이고 매칭되지 않은 경로는 `unmatched`로 기록한다. 알 수 없는 HTTP 메서드는 `OTHER`로 기록해 임의 입력에 따른 label 증가를 막는다. API key, 사용자 식별자와 raw URL은 label에 포함하지 않는다. 스트림 중단 후에는 이미 전송한 HTTP 상태가 기록될 수 있으므로 상태 코드만으로 스트림 완료를 판단할 수 없다.

메트릭 리스너는 API 리스너와 분리되어 자체 scrape 요청을 API 메트릭에 포함하지 않는다. `metrics.enabled=false`이면 메트릭 registry와 리스너를 생성하지 않는다.

## OpenTelemetry와 JSON 로그

애플리케이션은 설정에 따라 인스턴스별 trace, metric, log provider를 생성한다. OTLP HTTP exporter가 Collector로 세 신호를 전송한다. 기존 Prometheus scrape와 OTLP metric export는 독립적으로 활성화할 수 있다.

Echo middleware가 서버 span을 만들고 upstream transport를 감싼 `http.RoundTripper`가 client span과 W3C trace context 전파를 담당한다. client span은 스트리밍 응답 본문이 끝나거나 닫힐 때 완료된다. 요청 완료 로그는 `slog`로 기록하며 기본 출력은 JSON이다. context에 유효한 span이 있으면 `trace_id`와 `span_id`를 포함한다.

로그와 span에는 요청 본문, 인증 헤더, query와 raw URL을 기록하지 않는다. HTTP route와 메서드는 메트릭과 같은 제한된 값으로 기록한다. 수집 경로와 조회 방법은 [OpenTelemetry 관측 구성](../operations/observability.md)에 있다.

## 시작과 종료

1. 설정을 검증하고 telemetry providers, logger, 버킷, upstream transport와 registry를 생성한다. Redis 전략은 연결을 확인한다.
2. 필요한 모든 주소를 먼저 bind한다. 일부 bind가 실패하면 이미 연 리스너를 닫고 시작을 실패 처리한다.
3. Echo API와 선택적인 메트릭 서버를 실행하고 readiness를 활성화한다.
4. 종료 신호나 서버 오류가 발생하면 readiness를 내리고 모든 서버에 같은 deadline으로 `Shutdown`을 호출한다.
5. deadline이 지나면 request context를 취소하고 남은 연결을 닫는다. HTTP 서버가 끝나면 upstream 유휴 연결과 버킷 client를 정리한다.
6. 종료 신호와 분리된 context로 telemetry providers를 종료해 대기 중인 데이터를 전송한다. 이 단계에는 `telemetry.shutdown_timeout`을 적용한다.

종료 신호 context와 HTTP request context를 분리하므로 신호를 받았다는 이유만으로 진행 중인 스트림을 즉시 취소하지 않는다. 정상적인 drain은 성공 exit code, 시작 오류 또는 종료 deadline 초과는 실패 exit code를 반환한다.
