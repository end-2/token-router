# 배포 컴포넌트와 Redis

`deploy/components/`는 배포할 컴포넌트를 디렉토리별로 관리한다. 각 컴포넌트는 독립적인 Kustomize 구성으로 정의하고 상위 `kustomization.yaml`에서 모은다. Namespace는 이 디렉토리에서 고정하지 않고 배포 환경에서 지정한다.

## 디렉토리 구성

```text
deploy/components/
  kustomization.yaml
  redis/
    kustomization.yaml
    redis.conf
    service.yaml
    statefulset.yaml
```

- [`components/kustomization.yaml`](../../deploy/components/kustomization.yaml): 전체 컴포넌트의 진입점
- [`redis/kustomization.yaml`](../../deploy/components/redis/kustomization.yaml): Redis 리소스와 ConfigMap 생성 설정
- [`redis/redis.conf`](../../deploy/components/redis/redis.conf): Redis 서버 설정
- [`redis/service.yaml`](../../deploy/components/redis/service.yaml): 클라이언트용 Service와 StatefulSet용 headless Service
- [`redis/statefulset.yaml`](../../deploy/components/redis/statefulset.yaml): Redis Pod, 상태 검사, 리소스 제한과 PVC

다른 컴포넌트를 추가할 때는 같은 구조로 하위 디렉토리를 만들고 상위 `kustomization.yaml`의 `resources`에 등록한다. 환경별 Namespace와 설정 변경은 해당 환경의 Kustomize 구성에서 관리한다.

## Redis 구성

Redis는 개발과 테스트용 단일 서버로 구성한다. 인증과 TLS는 설정하지 않으며, Service는 클러스터 내부에서 사용하는 `ClusterIP`이다.

| 항목 | 설정 |
| --- | --- |
| 이미지 | `redis:7.4.11-alpine3.21`, digest 고정 |
| StatefulSet | `redis`, replica 1개 |
| 클라이언트 Service | `redis:6379` |
| StatefulSet의 headless Service | `redis-headless:6379` |
| 설정 파일 | `/usr/local/etc/redis/redis.conf` |
| 데이터 경로 | `/data` |
| PVC | `data-redis-0`, `ReadWriteOnce`, `1Gi` |
| Redis 데이터 메모리 한도 | `maxmemory 128mb` |
| 메모리 초과 정책 | `noeviction` |
| Pod 메모리 한도 | `256Mi` |
| 저장 방식 | AOF, `appendfsync everysec`, RDB snapshot 비활성화 |

클러스터에 기본 StorageClass와 동적 볼륨 provisioner가 필요하다. `storageClassName`을 생략해 해당 클러스터의 기본 StorageClass를 사용한다. kind에서는 클러스터 생성 시 설치되는 기본 StorageClass를 사용한다.

Pod의 UID는 `999`, GID와 `fsGroup`은 `1000`이다. 이미지의 Redis 사용자와 그룹 정의는 [`redis/docker-library-redis`, `alpine/Dockerfile`, commit `74654c6`](https://github.com/redis/docker-library-redis/blob/74654c612ee06275377d483dc4e134e57b463e9e/alpine/Dockerfile)에 있다. Pod는 `fsGroup: 1000`을 볼륨에 사용할 그룹으로 지정한다. 컨테이너의 루트 파일 시스템은 읽기 전용이며 `/data`에 데이터를 기록한다.

startup probe, readiness probe와 liveness probe는 `redis-cli -e ping`을 사용한다. Redis 명령이 오류를 반환하면 `-e` 옵션으로 상태 검사를 실패 처리한다.

AOF와 메모리 설정의 근거는 [`redis/redis`, `redis.conf`, 태그 `7.4.11`](https://github.com/redis/redis/blob/7.4.11/redis.conf)이다. `appendfsync everysec`는 매 쓰기마다 디스크 동기화를 수행하는 설정이 아니다. 저장 정책은 [Redis persistence](https://redis.io/docs/latest/operate/oss_and_stack/management/persistence/)를 참고한다.

## kind에서 설치와 검증

[`tests/kind/manifests/kustomization.yaml`](../../tests/kind/manifests/kustomization.yaml)은 `deploy/components/`를 포함한다. 저장소 루트에서 기존 kind 명령을 실행하면 Redis도 `token-router-test` Namespace에 배포된다.

```sh
make kind-render
make kind-up
make kind-test
make kind-status
```

`make kind-up`은 Redis StatefulSet이 준비될 때까지 기다린다. `make kind-test`는 Job의 Redis 클라이언트에서 `redis.token-router-test.svc.cluster.local`을 통해 `PING`, `SET`과 `GET`을 검증한다. 테스트 키는 Pod별 이름과 60초 TTL을 사용하고, 검증에 성공하면 삭제한다.

직접 연결 상태를 확인할 수도 있다.

```sh
kubectl --kubeconfig tests/kind/.kube/token-router-test.yaml \
  --context kind-token-router-test -n token-router-test \
  exec statefulset/redis -- redis-cli -e ping
```

호스트에서 Redis 클라이언트를 연결하려면 다음 명령으로 포트를 전달한다. 전달 중 `redis://127.0.0.1:16379`로 접속한다.

```sh
kubectl --kubeconfig tests/kind/.kube/token-router-test.yaml \
  --context kind-token-router-test -n token-router-test \
  port-forward --address 127.0.0.1 service/redis 16379:6379
```

## 다른 배포 환경에서 사용

Redis만 렌더링하려면 다음 명령을 사용한다.

```sh
kubectl kustomize deploy/components/redis
```

이미 존재하는 Namespace에 설치할 때는 대상 클러스터 context와 Namespace를 지정한다.

```sh
kubectl --context <context-name> -n <namespace> apply -k deploy/components/redis
kubectl --context <context-name> -n <namespace> rollout status statefulset/redis
```

`<context-name>`과 `<namespace>`는 실제 값으로 바꾼다. 전체 컴포넌트를 설치할 때는 `-k deploy/components`를 사용한다.

## 설정 변경과 데이터 수명

`redis.conf`를 수정하고 다시 배포하면 Kustomize가 내용에 따른 이름의 ConfigMap을 생성한다. StatefulSet의 ConfigMap 참조도 변경되므로 Redis Pod를 교체해 새 설정을 읽는다.

StatefulSet은 삭제되거나 replica 수가 줄어들어도 PVC를 유지하도록 설정한다. 같은 Namespace에서 같은 이름으로 재배포하면 유지한 PVC를 다시 사용한다. Namespace 삭제나 PVC 삭제는 별도의 작업이다.

kind의 데이터는 Docker 노드 안에 저장된다. `make kind-down`으로 클러스터를 삭제하면 해당 테스트 환경의 Redis 데이터도 사라진다.
