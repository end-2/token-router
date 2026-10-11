# 문서 목차

## 아키텍처

- [프록시 서버 구성](architecture/proxy-server.md): 패키지 책임, 프록시 처리, 버킷 전략, 메트릭과 종료 순서

## 개발

- [프록시 서버 개발과 실행](development/proxy-server.md): Docker 빌드, 실행, lint, 단위 테스트와 Redis 연결
- [Ginkgo E2E 테스트](development/end-to-end-tests.md): 실제 서버 프로세스, Redis와 관측 컴포넌트를 사용하는 Docker Compose 시나리오 테스트
- [kind 테스트 환경](development/kind-test-environment.md): 클러스터 구성, 실행 명령, smoke test와 로컬 이미지 사용 방법
- [배포 컴포넌트](development/deployment-components.md): Redis와 관측 컴포넌트의 manifest, 설치와 설정 관리

## 운영

- [OpenTelemetry 관측 구성](operations/observability.md): JSON 로그, metrics와 tracing 설정, 수집 경로와 조회

## 설정

- [애플리케이션 설정](reference/configuration.md): YAML 계층, 환경 변수와 CLI 우선순위, timeout, 인증, 요청 수와 토큰 사용량 제한
