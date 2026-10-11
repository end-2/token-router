# Policy 설정과 admin API

`upstream` 전체와 rate limit의 수치 설정은 `policy.yaml`에서 관리하고 실행 중 교체할 수 있다. 파일 감시와 전용 admin API는 같은 검증과 적용 경로를 사용한다. 저장소의 `config.yaml`은 `policy.file: policy.yaml`로 파일 감시를 활성화하며 admin 리스너는 기본적으로 비활성화한다.

## 설정 분리

메인 `config.yaml`에는 리스너, 파일 경로, 전략과 관측 설정을 둔다.

```yaml
server:
  address: "127.0.0.1:8080"
admin:
  enabled: true
  address: "127.0.0.1:9091"
policy:
  file: policy.yaml
  watch_interval: 1s
rate_limit:
  enabled: true
  backend: memory
```

`policy.yaml`에는 다음 필드를 사용할 수 있다. 전체 기본 설정은 저장소 루트의 [`policy.yaml`](../../policy.yaml)에 있다.

```yaml
upstream:
  base_urls:
    - "http://127.0.0.1:8000"
  routes: []
  request_timeout: 0s
rate_limit:
  requests_per_second: 10
  burst: 20
  tokens_per_second: 1000
  token_burst: 60000
```

`upstream`은 목적지, Host와 model 라우팅, API key, 조직과 프로젝트 헤더, timeout과 연결 풀 설정을 모두 지원한다. `rate_limit`은 `requests_per_second`, `burst`, `tokens_per_second`, `token_burst`만 지원한다. `rate_limit.enabled`, `rate_limit.backend`, `rate_limit.redis`와 다른 최상위 설정은 메인 설정에 고정한다. 이 필드를 policy 파일이나 admin 요청에 넣으면 거부한다.

`policy.file`의 상대 경로는 메인 설정 파일의 디렉토리를 기준으로 해석한다. 파일 경로와 감시 간격 자체는 시작할 때 고정한다. `policy.watch_interval`은 양수여야 하며 기본값은 `1s`다. 다음 CLI와 환경 변수도 사용할 수 있다.

| YAML 키 | 환경 변수 | CLI |
| --- | --- | --- |
| `policy.file` | `TOKEN_ROUTER_POLICY_FILE` | `--policy-file` |
| `policy.watch_interval` | `TOKEN_ROUTER_POLICY_WATCH_INTERVAL` | `--policy-watch-interval` |
| `admin.enabled` | `TOKEN_ROUTER_ADMIN_ENABLED` | `--admin-enabled` |
| `admin.address` | `TOKEN_ROUTER_ADMIN_ADDRESS` | `--admin-address` |

```sh
go run ./src/cmd/token-router serve --config config.yaml --admin-enabled
```

메인 설정에는 기본값, 메인 YAML, 환경 변수, 명시적인 CLI 인자 순서로 값을 적용한다. policy 파일은 그 결과 위에 마지막으로 적용하므로 파일에 명시한 값은 환경 변수와 CLI보다 우선한다. 파일에서 생략한 값은 policy 파일을 적용하기 전 시작 설정을 사용한다. 이후 파일을 교체해도 같은 시작 설정을 기준으로 다시 병합하므로 필드를 삭제하면 시작 설정으로 돌아간다. 배열은 전체를 교체하며 `null`은 허용하지 않는다.

`policy.file`을 생략한 사용자 설정이나 `--policy-file=`을 사용하면 파일 감시 없이 시작 설정을 사용한다. 저장소의 `policy.yaml`은 `upstream.api_key: ""`를 포함하므로 기본적으로 클라이언트의 `Authorization`을 전달한다. 환경 변수 `TOKEN_ROUTER_UPSTREAM_API_KEY`를 적용하려면 policy 파일에서 `api_key` 필드를 생략한다.

## 파일 감시와 적용

파일 내용은 지정한 간격마다 읽어 SHA-256으로 비교한다. 같은 내용을 다시 읽으면 설정을 재생성하지 않는다. rename으로 파일을 교체하거나 symlink의 대상이 바뀌어도 다음 검사에서 내용을 읽는다. 메인 설정 파일과 환경 변수는 다시 읽지 않는다.

지정한 policy 파일이 시작 시 없거나 유효하지 않으면 시작에 실패한다. 시작 후 파일이 삭제되거나 읽기, 구문 또는 값 검증에 실패하면 마지막 정상 설정으로 계속 처리하고 경고 로그를 남긴다. 파일이 복구되면 감시가 계속 적용한다. 파일과 admin 요청 본문 크기는 각각 1 MiB 이하로 제한한다. 빈 파일은 오류이며 기본값만 적용하려면 `{}`를 사용한다.

교체할 내용을 다른 파일에 완성한 뒤 같은 파일시스템에서 rename하면 작성 중인 중간 내용이 노출되는 것을 피할 수 있다. 설정은 전체 검증 후 적용하며, upstream과 제한값 중 일부가 잘못되면 둘 다 바뀌지 않는다. upstream 변경은 새 요청부터 적용한다. 진행 중인 응답과 SSE는 기존 upstream에서 계속 처리하고, 완료된 이전 연결 풀은 정리한다. rate limit 수치만 변경하면 upstream 연결 풀과 라운드 로빈 순번을 유지한다. 토큰 사용량 관찰의 활성 상태가 바뀌면 새 프록시를 생성한다.

버킷 구현과 Redis client는 교체하지 않는다. 활성화된 버킷의 수치를 바꿀 때 잔액을 가득 채우지 않으며, 용량 축소는 양수 잔액에 상한을 적용하고 음수 토큰 잔액은 유지한다. memory는 변경 시점까지 기존 보충량으로 잔액을 계산한다. Redis는 다음 Lua 호출부터 새 값으로 잔액, 이전 호출 이후의 보충량과 TTL을 계산하며, 설정 변경 자체는 Redis key를 수정하지 않는다. 같은 Redis key를 공유하는 프로세스에는 같은 설정을 배포해야 하며 admin API는 수신한 프로세스의 설정과 파일만 수정한다.

`tokens_per_second`와 `token_burst`를 모두 `0`으로 설정하면 토큰 사용량 제한을 끈다. 다시 켤 때는 두 값을 모두 양수로 지정한다. 사용량 관찰 여부는 요청이 선택한 프록시에 따르고, 관찰한 사용량은 기록 시점의 버킷 설정으로 처리한다. 토큰 제한이 꺼져 있을 때 시작한 스트림의 사용량을 나중에 소급해서 수집하지 않는다.

## Admin API

`admin.enabled=true`이면 별도 HTTP 리스너를 연다. 기본 주소는 `127.0.0.1:9091`이며 API와 Prometheus 주소와 달라야 한다. admin 리스너가 bind에 실패하면 다른 리스너도 닫고 시작에 실패한다. 종료 시 다른 HTTP 리스너와 같은 graceful shutdown 절차를 사용한다.

| 메서드와 경로 | 동작 |
| --- | --- |
| `GET /config` | 현재 유효한 policy 설정 전체 조회 |
| `PATCH /config` | 현재 값 위에 요청 필드만 병합하고 검증 후 적용 |
| `PUT /config` | 요청을 새 policy 문서로 적용. 생략한 값은 시작 설정 사용 |

입력은 JSON 또는 YAML 매핑이다. 응답은 JSON이며 키 이름은 YAML과 같고 duration은 `10s` 같은 문자열이다. `GET` 응답을 그대로 `PUT`할 수 있다. `PATCH`에서도 배열 전체를 교체한다. 요청은 순서대로 적용하며 별도 버전 충돌 검사는 없다.

```sh
curl --fail http://127.0.0.1:9091/config

curl --fail -X PATCH http://127.0.0.1:9091/config \
  -H 'Content-Type: application/json' \
  --data '{"rate_limit":{"requests_per_second":25,"burst":50}}'

curl --fail -X PATCH http://127.0.0.1:9091/config \
  -H 'Content-Type: application/json' \
  --data '{"upstream":{"base_urls":["http://127.0.0.1:8001"],"routes":[]}}'

curl --fail -X PUT http://127.0.0.1:9091/config \
  -H 'Content-Type: application/yaml' --data-binary @policy.yaml
```

성공하면 `200`과 적용된 설정을 반환한다. 잘못된 설정과 고정 필드 변경은 `400`, 본문 크기 초과는 `413`, 파일 저장 실패는 `500`, 종료 중 변경 요청은 `503`을 반환한다. 오류 본문에는 설정 입력값을 포함하지 않는다. `/config`는 public API나 Prometheus 리스너에 등록하지 않으며 admin 요청은 프록시 rate limit을 소비하지 않는다.

`policy.file`이 있으면 API는 유효 설정 전체를 YAML로 직렬화하고 같은 디렉토리의 임시 파일을 rename한 뒤 메모리에 적용한다. 저장 실패 시 메모리 설정도 유지한다. 저장된 파일은 재시작 후에도 사용하고 기존 YAML 주석과 표현 형식은 유지하지 않는다. 시작 설정에서 상속한 API key를 포함한 유효값도 파일에 저장한다. 생성한 파일 권한은 `0600`이다.

API 저장에는 일반 파일과 디렉토리 쓰기 권한이 필요하다. symlink 파일은 감시만 지원하며 API 저장은 거부한다. 읽기 전용 마운트나 단일 파일 bind mount처럼 rename이 불가능한 위치에서도 저장은 실패한다. 컨테이너에서 API 저장을 사용하려면 쓰기 가능한 디렉토리를 마운트하고 실행 UID에 디렉토리 쓰기 권한을 부여한다. `policy.file`이 비어 있으면 API 변경은 해당 프로세스 메모리에만 남는다.

Admin API는 인증 기능을 제공하지 않고 응답에 `upstream.api_key`를 포함한다. 기본 loopback 주소를 사용하거나 접근을 제한한 관리 네트워크에서 사용한다. 컨테이너 외부로 연결하려면 `--admin-address=:9091`을 명시하고 필요한 호스트 포트만 공개한다.
