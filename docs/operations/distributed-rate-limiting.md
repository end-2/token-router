# 분산 memory rate limit

`rate_limit.backend: memory`에서 `rate_limit.memory.distributed.enabled: true`를 설정하면 Redis 없이 여러 token-router가 요청 수와 토큰 사용량 잔액을 공유한다. 각 프로세스는 Raft voter로 참여하고, 선출된 leader가 허용 판단과 사용량 기록을 직렬화한다. 과반수에 기록된 명령을 적용한 뒤 결과를 반환한다. 계정 제한을 켜면 같은 계정의 잔액을 공유하고, 끄면 클러스터 전체가 하나의 버킷을 공유한다.

## 설정과 서비스 디스커버리

다음은 첫 번째 노드의 `config.yaml` 설정이다. `peers`는 자신을 포함한 전체 노드의 `IP:admin-port` 목록이다. 모든 노드가 같은 목록과 공유 token을 사용하고, `address`와 데이터 디렉토리는 각 노드에 맞게 설정한다.

```yaml
server:
  address: "0.0.0.0:8080"
admin:
  enabled: true
  address: "0.0.0.0:9091"
policy:
  file: policy.yaml
rate_limit:
  enabled: true
  backend: memory
  memory:
    distributed:
      enabled: true
      address: "10.0.0.11:9091"
      peers:
        - "10.0.0.11:9091"
        - "10.0.0.12:9091"
        - "10.0.0.13:9091"
      data_dir: data/rate-limit
      request_timeout: 5s
```

공유 token은 `TOKEN_ROUTER_RATE_LIMIT_MEMORY_DISTRIBUTED_TOKEN` 환경 변수로 주입한다. 위 예제에는 실제 token을 저장하지 않는다. 두 번째와 세 번째 노드는 `address`를 각각 자신의 IP로 바꾼다. 동일 호스트에서도 서로 다른 admin 포트와 데이터 디렉토리로 여러 노드를 실행할 수 있다. API와 Prometheus 리스너 포트도 충돌하지 않게 설정한다.

| 설정 | 의미와 기본값 |
| --- | --- |
| `rate_limit.memory.distributed.enabled` | 기본값 `false`, `memory` 선택 시 분산 모드 활성화 |
| `rate_limit.memory.distributed.address` | 다른 노드가 접속할 자신의 `IP:admin-port`, 활성화 시 필수 |
| `rate_limit.memory.distributed.peers` | 자신을 포함한 전체 voter 주소 배열, 모든 노드에 동일하게 지정 |
| `rate_limit.memory.distributed.data_dir` | Raft 로그, 투표 상태와 snapshot 저장 경로, 기본값 `data/rate-limit` |
| `rate_limit.memory.distributed.token` | 내부 요청 인증용 공통 비밀 값, 활성화 시 필수 |
| `rate_limit.memory.distributed.request_timeout` | 개별 허용 판단과 사용량 기록의 대기 상한, 기본값 `5s`, 양수 |

`address`와 `peers`에는 정규화된 IP 주소와 1부터 65535 사이의 포트만 허용한다. IPv6는 `[2001:db8::11]:9091` 형식이다. 호스트명, URL, 중복 peer, wildcard 주소는 허용하지 않는다. `admin.address`는 실제 bind 주소이므로 `0.0.0.0:9091`이나 `[::]:9091`을 사용할 수 있으며, 광고하는 `address`와 포트가 같아야 한다.

서비스 디스커버리는 정적 peer 목록으로 수행한다. seed 목록을 통한 자동 가입, DNS 조회와 실행 중 membership 변경은 지원하지 않는다. `peers` 순서는 달라도 되지만 구성원은 같아야 한다. 주소와 membership을 저장된 데이터 디렉토리의 식별 정보와 비교하므로, 기존 데이터로 다른 주소나 voter 목록을 사용하면 시작을 거부한다.

각 설정은 같은 경로의 CLI와 환경 변수로도 전달할 수 있다.

```sh
go run ./src/cmd/token-router serve --config config.yaml \
  --rate-limit-memory-distributed-enabled \
  --rate-limit-memory-distributed-address=10.0.0.11:9091 \
  --rate-limit-memory-distributed-peers='["10.0.0.11:9091","10.0.0.12:9091","10.0.0.13:9091"]'
```

## 정책과 계정

제한값과 계정 추출 설정은 `policy.yaml`에 둔다. 다음 설정은 모든 계정에 요청 burst 20과 토큰 burst 60,000을 각각 적용한다.

```yaml
rate_limit:
  requests_per_second: 10
  burst: 20
  tokens_per_second: 1000
  token_burst: 60000
  account:
    enabled: true
    jwt_claim: sub
```

계정 추출과 인증의 구분은 [계정별 제한](../reference/configuration.md#계정별-제한)에 있다. `account.enabled: false`이면 이 수치를 클러스터 전체에 적용한다.

모든 노드에 같은 policy를 배포한다. policy 파일과 admin `/config` 변경은 해당 노드에만 적용되며 Raft로 정책 파일을 전파하지 않는다. follower가 전달한 제한값이나 신규 요청의 계정 추출 설정이 leader와 다르면 `503`을 반환한다. 변경을 순차 배포하는 동안 일시적으로 이 오류가 발생할 수 있다. 사용량 기록은 요청 허용 시 결정한 계정을 유지한다.

분산 버킷은 새 값이 포함된 명령이 적용될 때 기존 보충량으로 해당 계정의 잔액을 계산한 후 새 용량을 적용한다. 용량을 늘려도 기존 잔액을 가득 채우지 않으며, 토큰 부채를 유지한다. 토큰 제한 비활성화와 재활성화는 [Policy 설정](policy-configuration.md#파일-감시와-적용)의 사용량 관찰 규칙을 따른다.

## 복구와 가용성

운영에서는 3개 이상의 홀수 voter 구성을 사용한다. 3개 중 2개, 5개 중 3개처럼 과반수가 통신해야 제한 판단을 완료할 수 있다. 단일 노드도 허용하지만 장애 허용 능력은 없다. 2개 노드는 하나만 중단되어도 과반수를 잃는다.

leader 장애 시 남은 과반수가 leader를 다시 선출하고 복제된 잔액을 이어받는다. 선출 중이거나 과반수에 기록할 수 없으면 요청 허용 판단은 `503`으로 실패한다. `memory`의 프로세스별 버킷으로 자동 전환하지 않는다. `/readyz`는 HTTP 서버의 시작과 종료 상태만 표시하므로 클러스터의 과반수 가용성을 보장하지 않는다.

`data_dir`의 상대 경로는 메인 설정 파일의 디렉토리를 기준으로 한다. 노드마다 별도 디렉토리를 사용하고 재시작 시 동일한 주소와 디렉토리를 유지한다. 컨테이너에서는 실행 UID가 쓸 수 있는 영속 볼륨을 사용한다. 프로세스 재시작 시 snapshot과 로그로 잔액을 복구한다. 로그와 snapshot은 계정 식별자의 SHA-256 해시를 사용하며 인증 헤더나 토큰 원문을 저장하지 않는다. 모든 노드의 데이터를 삭제하면 기존 사용량을 복구할 수 없다.

leader가 명령에 기록한 시간을 모든 노드가 사용한다. leader 교체 후 시계가 뒤로 이동하면 해당 버킷의 마지막 기록 시각보다 앞선 시간으로 보충하지 않는다. 시계가 앞으로 크게 이동하면 그만큼 보충될 수 있으므로 노드의 시계를 동기화한다.

응답을 받기 전에 연결이 끊기거나 timeout이 발생하면 명령이 이미 기록되었을 수 있다. 내부 mutation은 자동 재시도하지 않는다. 요청이 `503`으로 끝났어도 요청 수가 차감될 수 있으며, 사용량 기록 실패 시 일부 사용량이 누락될 수 있다. 이미 시작한 upstream 응답은 그대로 전달하고 사용량 기록 실패를 경고 로그로 남긴다.

허용 판단과 사용량 기록마다 leader 및 과반수의 디스크 기록을 기다린다. 네트워크 지연과 저장 장치 성능이 처리량에 영향을 준다. 계정 상태는 모든 버킷을 완전히 보충할 만큼 유휴 시간이 지난 뒤 정리할 수 있고, 토큰 부채가 남은 상태는 유지한다.

## Admin 포트와 내부 통신

추가 Raft 포트는 필요하지 않다. admin 리스너에 다음 내부 경로가 등록된다.

| 메서드와 경로 | 동작 |
| --- | --- |
| `CONNECT /internal/rate-limit/raft` | 인증 후 Raft 연결을 넘겨받아 투표, 로그 복제와 snapshot 전송 |
| `POST /internal/rate-limit/apply` | follower가 leader에 허용 판단 또는 사용량 기록 전달 |

내부 요청은 공유 token과 peer 목록에서 계산한 클러스터 식별자를 확인한다. 다른 peer 목록이나 잘못된 token이면 요청을 거부한다. 내부 경로는 API 리스너와 Prometheus 리스너에 노출하지 않는다. 프록시 환경 변수와 HTTP redirect를 사용하지 않는다.

admin 통신은 HTTP이며 TLS를 제공하지 않는다. peer 간 직접 연결이 가능한 신뢰된 관리 네트워크에서 접근을 제한한다. 내부 경로의 token 검사는 `/config`를 보호하지 않으며, `/config`는 기존과 같이 별도 인증이 없다. 해당 포트의 외부 접근과 정책 변경 권한은 네트워크 계층에서 제한해야 한다.

## 구현 근거

[`distributed.go`](../../src/internal/bucket/distributed.go)는 `hashicorp/raft` v1.8.0의 `BootstrapCluster`, `Apply`, `Shutdown`과 `raft-boltdb/v2` v2.4.2의 `New`를 사용한다. `Apply` timeout은 명령 enqueue만 제한하므로, 호출자의 전체 대기 시간은 별도 context로 제한한다. 관련 공식 구현은 [Raft v1.8.0 api.go](https://github.com/hashicorp/raft/blob/v1.8.0/api.go)와 [raft-boltdb v2.4.2 bolt_store.go](https://github.com/hashicorp/raft-boltdb/blob/v2.4.2/bolt_store.go)에 있다.

[`distributed_transport.go`](../../src/internal/bucket/distributed_transport.go)는 [Raft v1.8.0 net_transport.go의 StreamLayer](https://github.com/hashicorp/raft/blob/v1.8.0/net_transport.go)를 admin HTTP CONNECT 위에 구현한다. [`distributed_state.go`](../../src/internal/bucket/distributed_state.go)는 복제된 시간, 계정별 잔액과 snapshot을 관리한다.
