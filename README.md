# Token router

OpenAI API 요청을 여러 upstream으로 전달하고 요청 수와 모델 토큰 사용량을 관찰하고 제한하는 Go 기반 프록시 서버다.

## 시작하기

처음 실행하려면 [프록시 서버 개발과 실행](docs/development/proxy-server.md)의 도구 준비와 실행 절차를 따른다. 빌드와 테스트 방법도 같은 문서에서 확인할 수 있다.

## 문서

전체 문서는 [문서 목차](docs/README.md)에서 찾을 수 있다.

- [애플리케이션 설정](docs/reference/configuration.md): upstream 라우팅, 인증과 사용량 제한
- [Policy 설정과 admin API](docs/operations/policy-configuration.md): 정책 파일 관리와 실행 중 변경
- [OpenTelemetry 관측 구성](docs/operations/observability.md): 로그, 메트릭과 tracing
- [프록시 서버 구성](docs/architecture/proxy-server.md): 패키지 구조와 요청 처리 흐름
