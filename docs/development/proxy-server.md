# 프록시 서버 개발과 실행

## 필요한 도구

- 실행 중인 Docker Engine 또는 Docker Desktop과 Docker BuildKit
- Docker Compose v2는 `make e2e`에 필요하다.
- GNU Make와 Bash
- 이미지와 Go 모듈을 다운로드할 수 있는 네트워크 연결
- Go 1.27.2는 호스트에서 `make run` 또는 `*-local` 명령을 사용할 때 필요하다.
- Redis는 `rate_limit.backend=redis`를 선택하거나 실제 Redis 통합 테스트를 실행할 때 필요하다.

다음 명령은 저장소 루트를 기준으로 한다. [`Dockerfile`](../../Dockerfile)의 빌드 환경과 [`go.mod`](../../go.mod)는 Go 1.27.2를 사용한다. Docker에서 빌드, 단위 테스트, vet와 lint를 실행하므로 호스트에 Go나 `golangci-lint`를 설치할 필요가 없다.

## Docker 빌드와 실행

```sh
make build
make image
docker run --rm token-router:local validate
docker run --rm --stop-timeout 40 \
  -p 127.0.0.1:8080:8080 \
  -p 127.0.0.1:9090:9090 \
  -v "$(pwd)/config.yaml:/app/config.yaml:ro" \
  -e TOKEN_ROUTER_UPSTREAM_API_KEY \
  token-router:local
```

`make build`는 Docker 빌드에서 Linux 실행 파일을 생성해 `bin/token-router`로 내보낸다. 호스트 운영체제용 실행 파일은 `make build-local`로 빌드한다. `make image`의 기본 이미지 이름은 `token-router:local`이며 `make image APP_IMAGE=token-router:dev`처럼 변경할 수 있다.

실행 이미지는 CA 인증서와 실행 파일, 기본 [`config.yaml`](../../config.yaml)을 포함하며 UID와 GID `65532`로 실행한다. 기본 작업 디렉토리는 `/app`, 기본 명령은 `serve`다. `-v` 옵션은 저장소의 설정 파일을 이미지의 기본 설정 위치에 마운트한다. `-e TOKEN_ROUTER_UPSTREAM_API_KEY`는 같은 이름의 호스트 환경 변수를 전달한다. upstream API key가 없으면 클라이언트 요청의 `Authorization`을 사용한다.

이미지는 `TOKEN_ROUTER_SERVER_ADDRESS=:8080`과 `TOKEN_ROUTER_METRICS_ADDRESS=:9090`을 설정해 컨테이너 외부에서 연결할 수 있게 한다. 이 환경 변수는 YAML의 주소보다 우선하므로 주소를 변경하려면 `docker run -e`나 CLI 인자로 재정의한다. 위 명령은 호스트 포트를 `127.0.0.1`에만 공개한다.

`validate`는 설정 구문과 값만 검사하며 Redis나 upstream 연결은 검사하지 않는다. `tests/kind/config.yaml`은 애플리케이션 설정과 별개의 kind 클러스터 설정이다.

서버 실행 후 다음 경로를 확인한다.

```sh
curl --fail http://127.0.0.1:8080/healthz
curl --fail http://127.0.0.1:8080/readyz
curl --fail http://127.0.0.1:9090/metrics
curl --fail http://127.0.0.1:8080/v1/models \
  -H "Authorization: Bearer ${OPENAI_API_KEY}"
```

`OPENAI_API_KEY`는 이 `curl` 예시에서 사용하는 셸 변수다. 서버가 이 변수를 자동으로 읽지는 않는다. 서버에서 공통 upstream API key를 주입하려면 `TOKEN_ROUTER_UPSTREAM_API_KEY`를 사용한다. API key를 설정한 프록시로 들어오는 요청은 해당 key를 사용한다. 현재 프록시 자체의 사용자 인증은 없다. 호스트에서 직접 실행할 때 기본 API와 메트릭 리스너는 loopback 주소에 바인딩한다.

## 검증

```sh
make test
make vet
make lint
make e2e
```

`make test`는 race detector를 포함한 일반 Go 테스트를 실행한다. 설정 계층, 메모리와 Redis 버킷, HTTP 프록시, SSE 전달, HTTP 상태 메트릭과 graceful shutdown을 검사한다. 일반 테스트는 로컬 테스트 upstream과 `miniredis`를 사용하므로 OpenAI API key나 외부 Redis 서버가 필요 없다.

[`streaming_body_test.go`](../../src/internal/proxy/streaming_body_test.go)는 upstream transport의 마지막 요청 본문 읽기를 지연시킨 상태에서 SSE 응답을 시작해 본문 읽기와 스트림 전달이 모두 완료되는지 확인한다.

`make vet`와 `make lint`는 `e2e` build tag를 포함해 E2E 테스트 코드도 검사한다. `make lint`는 Docker에서 `golangci-lint` v2.14.0을 실행하며 [`.golangci.yaml`](../../.golangci.yaml)의 기본 검사에 `bodyclose`, `ginkgolinter`와 `gofmt` 검사를 추가한다. Ginkgo와 실제 Redis를 사용하는 `make e2e`의 구성과 시나리오는 [Ginkgo E2E 테스트](end-to-end-tests.md)에 있다.

## 호스트 도구 사용

Go의 자동 toolchain 선택을 사용하는 환경에서는 `go.mod`에 지정한 버전의 Go가 다운로드될 수 있다.

```sh
go run ./src/cmd/token-router --help
go run ./src/cmd/token-router validate --config config.yaml
make run
```

`serve`를 생략해도 서버가 실행된다. 호스트 도구를 사용하는 빌드와 검증은 다음과 같다.

```sh
make build-local
make test-local
make vet-local
go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.14.0
make lint-local
make fmt
```

## Redis 전략 실행

[배포 컴포넌트와 Redis](deployment-components.md)의 포트 전달 절차를 사용하면 kind의 Redis를 호스트 `127.0.0.1:16379`로 연결할 수 있다. 포트 전달을 유지한 상태에서 다음과 같이 실행한다.

```sh
go run ./src/cmd/token-router serve \
  --rate-limit-backend redis \
  --rate-limit-redis-address 127.0.0.1:16379
```

Redis를 선택하면 시작 시 `PING`에 성공해야 HTTP 리스너가 열린다. 요청 처리 중 Redis에 접근할 수 없으면 `503`을 반환한다. Redis의 같은 DB와 `rate_limit.redis.key`를 사용하는 인스턴스는 요청 수 제한을 공유하므로 같은 rate와 burst를 설정한다.

접근 가능한 테스트용 Redis에서 실제 Lua 동시성 테스트를 실행할 수 있다. 이 테스트는 고유 key를 만들고 종료 시 해당 key만 삭제한다.

```sh
TOKEN_ROUTER_TEST_REDIS_ADDR=127.0.0.1:16379 \
  go test -race ./src/internal/bucket -run TestRedisIntegrationSharedAtomicBucket
```

## 종료

`SIGINT` 또는 `SIGTERM`을 받으면 readiness가 내려가고 리스너를 닫는다. 진행 중인 요청은 `server.shutdown_timeout`까지 기다린다. 제한 시간이 지나면 연결을 강제로 닫고 upstream 요청 context를 취소한다. 강제 종료가 발생하면 프로세스는 오류로 종료한다.

컨테이너에서는 실행 파일이 PID 1로 신호를 받는다. Docker의 종료 대기 시간은 `server.shutdown_timeout`과 `telemetry.shutdown_timeout`의 합보다 길게 설정한다. 위 실행 예시는 기본 합계인 35초보다 긴 `--stop-timeout 40`을 사용한다.

OpenTelemetry export는 `telemetry.enabled=true`로 활성화한다. 실행 예시와 Collector 연결 방법은 [OpenTelemetry 관측 구성](../operations/observability.md)에 있다.

설정은 시작할 때 읽는다. YAML 또는 환경 변수 값을 바꾸면 프로세스를 재시작해야 한다.
