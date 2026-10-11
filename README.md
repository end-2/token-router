# Token router

AI Token usage를 추적, 관찰, 제한할 수 있도록 제공하는 프레임워크

`src/`에는 Echo 기반 OpenAI API 프록시 서버가 있다. YAML, 환경 변수와 CLI 인자로 upstream, 요청 수와 모델 토큰 사용량 제한을 설정하고, Prometheus 메트릭을 별도 포트에서 제공한다. 토큰 제한은 선택적으로 활성화하며 OpenAI 형식의 JSON과 SSE 응답에서 받은 `usage`를 사용한다. 사용량은 응답을 받은 뒤 반영하므로 진행 중인 요청은 한도를 초과할 수 있다. 설정과 스트리밍 조건은 [요청 수와 토큰 사용량 제한](docs/reference/configuration.md#요청-수와-토큰-사용량-제한)에 있다.

JSON 로그와 OpenTelemetry의 metrics, logs, traces를 지원한다. OTLP HTTP로 Collector에 전송하고, 배포 컴포넌트의 Prometheus, Loki, Tempo와 Grafana에서 조회할 수 있다.

Docker와 Docker Compose v2, GNU Make를 사용해 저장소 루트에서 빌드하고 검증한다. Docker 빌드 환경과 Go 모듈은 Go 1.27.2를 사용한다.

```sh
make build
make image
make test
make lint
make e2e
```

`make build`는 Linux 실행 파일 `bin/token-router`를 만들고, `make image`는 실행용 컨테이너 이미지를 만든다. 실행과 설정 파일 마운트 방법은 [프록시 서버 개발과 실행](docs/development/proxy-server.md)에 있다. upstream API key를 설정하지 않으면 클라이언트의 `Authorization`을 전달한다.

## 문서

- [문서 목차](docs/README.md)
- [프록시 서버 개발과 실행](docs/development/proxy-server.md)
- [Ginkgo E2E 테스트](docs/development/end-to-end-tests.md)
- [프록시 서버 구성](docs/architecture/proxy-server.md)
- [애플리케이션 설정](docs/reference/configuration.md)
- [OpenTelemetry 관측 구성](docs/operations/observability.md)
- [kind 테스트 환경](docs/development/kind-test-environment.md)
- [배포 컴포넌트](docs/development/deployment-components.md)
