# kind 테스트 환경

`tests/kind/`는 Docker 위에서 Kubernetes 테스트 환경을 구성한다. `deploy/components/`의 Redis와 `kind-echo`라는 샘플 HTTP 서비스를 배포해 클러스터 준비 상태와 네트워크 경로를 확인한다. 현재 저장소에는 애플리케이션 코드와 `Dockerfile`이 없으므로 토큰 사용량 추적이나 제한 기능에 대한 애플리케이션 테스트는 포함하지 않는다. Redis manifest와 저장 설정은 [배포 컴포넌트와 Redis](deployment-components.md)에서 설명한다.

## 디렉토리 구성

```text
tests/kind/
  config.yaml
  manifests/
    kustomization.yaml
    namespace.yaml
    echo.yaml
  smoke/
    job.yaml
  scripts/
    kind.sh
```

- [`config.yaml`](../../tests/kind/config.yaml): kind 노드, Kubernetes 이미지와 호스트 포트 매핑
- [`manifests/`](../../tests/kind/manifests/): 테스트 Namespace, HTTP Deployment와 NodePort Service, 공통 배포 컴포넌트 참조
- [`smoke/job.yaml`](../../tests/kind/smoke/job.yaml): 클러스터 DNS, HTTP와 Redis 응답을 확인하는 Job
- [`scripts/kind.sh`](../../tests/kind/scripts/kind.sh): 환경 생성, 검증, 상태 조회, 이미지 로드와 삭제

## 필요한 도구

- 실행 중인 Docker Engine 또는 Docker Desktop
- `kind` v0.33.0
- Kubernetes v1.37과 같은 minor 버전의 `kubectl`
- Bash, `curl`과 GNU Make
- 컨테이너 이미지를 다운로드할 수 있는 네트워크 연결
- 호스트의 사용 가능한 TCP 포트 `18080`

로컬 이미지 로드에는 `docker image save --platform`을 지원하는 Docker CLI와 Docker API 1.48 이상이 추가로 필요하다. 옵션의 요구사항은 [Docker 이미지 저장 안내](https://docs.docker.com/reference/cli/docker/image/save/#save-a-specific-platform---platform)에 있다.

도구 설치 방법은 [kind Quick Start](https://kind.sigs.k8s.io/docs/user/quick-start/)와 [kubectl 설치 안내](https://kubernetes.io/docs/tasks/tools/#kubectl)를 참고한다.

```sh
docker info
kind version
kubectl version --client
```

노드 이미지는 [kind v0.33.0 릴리스](https://github.com/kubernetes-sigs/kind/releases/tag/v0.33.0)에 포함된 Kubernetes v1.37.0의 태그와 digest를 사용한다. `kind`의 [`pkg/apis/config/defaults/image.go`, `Image`](https://github.com/kubernetes-sigs/kind/blob/v0.33.0/pkg/apis/config/defaults/image.go)와 같은 이미지다. 샘플 HTTP 서버와 smoke Job 이미지도 digest로 고정한다.

## 클러스터 구성

| 항목 | 설정 |
| --- | --- |
| 클러스터 이름 | `token-router-test` |
| kubeconfig context | `kind-token-router-test` |
| 노드 | control-plane 1개, worker 2개 |
| 테스트 Namespace | `token-router-test` |
| HTTP Deployment | `kind-echo`, replica 2개 |
| 클러스터 내부 HTTP 주소 | `http://kind-echo.token-router-test.svc.cluster.local` |
| 호스트 HTTP 주소 | `http://127.0.0.1:18080` |
| 포트 경로 | 호스트 `18080` -> control-plane `30080` -> Service `80` -> Pod `5678` |
| HTTP 응답 본문 | `token-router-kind-ready` |
| Redis 내부 주소 | `redis.token-router-test.svc.cluster.local:6379` |
| Redis StatefulSet | `redis`, replica 1개, `1Gi` PVC |

`podAntiAffinity`는 두 HTTP Pod가 서로 다른 노드에 배치되도록 한다. 기본 control-plane의 taint에 대한 toleration을 추가하지 않으므로 HTTP Pod는 두 worker에 배치된다.

Deployment의 `maxSurge: 0`과 `maxUnavailable: 1`은 두 worker에 추가 Pod를 배치할 여유가 없어도 HTTP Pod를 하나씩 교체할 수 있도록 한다.

kind의 `extraPortMappings.containerPort`와 Service의 `nodePort`는 모두 `30080`이다. 이 두 값은 일치해야 한다. 구성 근거는 [kind Configuration의 NodePort 설명](https://kind.sigs.k8s.io/docs/user/configuration/#nodeport-with-port-mappings)에 있다. Kubernetes API와 HTTP 포트는 호스트의 `127.0.0.1`에 바인딩한다.

`kind-echo`는 HashiCorp `http-echo` v1.0.0의 `-text`와 `-listen` 인자로 응답과 포트를 설정한다. readiness probe와 liveness probe는 `/health`를 사용한다. 구현 근거는 [`hashicorp/http-echo`, `main.go`, `main`과 `httpHealth`, v1.0.0](https://github.com/hashicorp/http-echo/blob/v1.0.0/main.go)이다.

## 생성과 검증

저장소 루트에서 실행한다.

```sh
make kind-render
make kind-up
make kind-test
make kind-status
```

`make kind-render`는 클러스터에 접근하지 않고 Kustomize로 배포 리소스를 출력한다. smoke Job은 테스트할 때 따로 배포한다.

`make kind-up`은 클러스터를 생성하고 샘플 HTTP 서비스와 Redis를 배포한다. 같은 이름의 클러스터가 있으면 재사용하며, 노드와 CoreDNS, HTTP Deployment, Redis StatefulSet이 준비될 때까지 기다린다. 기존 클러스터를 재사용할 때 `config.yaml`의 노드 구성과 포트 매핑은 변경되지 않는다.

`make kind-test`는 다음 항목을 검증한다.

1. 모든 노드와 CoreDNS, HTTP Deployment, Redis StatefulSet의 준비 상태
2. Job에서 Service DNS 이름으로 HTTP 요청을 보내고 응답 본문 확인
3. 호스트에서 Docker 포트 매핑과 NodePort Service를 통해 HTTP 요청을 보내고 응답 본문 확인
4. Job에서 Redis Service DNS 이름으로 `PING`, `SET`과 `GET` 응답 확인

Job은 실행할 때마다 다시 생성한다. 완료 후 300초가 지나면 Kubernetes가 Job을 정리한다. 실패하면 스크립트가 노드, Pod, Service, StatefulSet, PVC, Job, 이벤트와 컨테이너 로그를 출력한다.

호스트에서도 응답을 직접 확인할 수 있다.

```sh
curl --noproxy '*' --fail http://127.0.0.1:18080
```

## kubeconfig와 설정 변경

스크립트는 `tests/kind/.kube/<cluster-name>.yaml`에 테스트 전용 kubeconfig를 저장하고 모든 `kubectl` 명령에 파일 경로와 context를 지정한다. 이 디렉토리는 `.gitignore`에 포함되어 있다. 기존 기본 kubeconfig와 context는 수정하지 않는다. Docker provider를 명시적으로 사용한다.

직접 리소스를 조회할 때도 테스트 kubeconfig를 지정한다.

```sh
kubectl --kubeconfig tests/kind/.kube/token-router-test.yaml \
  --context kind-token-router-test -n token-router-test get pods -o wide
```

| 환경 변수 또는 Make 변수 | 기본값 | 용도 |
| --- | --- | --- |
| `KIND_CLUSTER_NAME` | `token-router-test` | 클러스터 이름과 kubeconfig 파일명 |
| `KIND_WAIT_TIMEOUT` | `180s` | 클러스터 생성과 Kubernetes 준비 상태 대기 시간 |
| `KIND_HTTP_URL` | `http://127.0.0.1:18080` | 호스트 HTTP 검증 주소 |
| `IMAGE` | 없음 | `make kind-load`로 로드할 로컬 이미지 |

```sh
make kind-up KIND_CLUSTER_NAME=token-router-integration KIND_WAIT_TIMEOUT=300s
make kind-test KIND_CLUSTER_NAME=token-router-integration KIND_WAIT_TIMEOUT=300s
make kind-down KIND_CLUSTER_NAME=token-router-integration
```

클러스터 이름을 바꿔도 Namespace와 호스트 포트는 그대로다. 같은 설정의 클러스터 여러 개를 동시에 실행하려면 각각 다른 호스트 포트가 필요하다.

호스트 포트를 바꾸려면 `config.yaml`의 `hostPort`를 수정한 뒤 클러스터를 삭제하고 다시 생성한다. 생성과 검증 명령에는 변경한 주소를 `KIND_HTTP_URL`로 전달한다. `KIND_HTTP_URL`만 바꿔서는 실제 포트 매핑이 변경되지 않는다.

Kubernetes 버전을 변경할 때는 `config.yaml`의 모든 노드 이미지와 digest를 함께 변경하고 해당 이미지에 맞는 `kind` 릴리스와 `kubectl`을 사용한다.

## 로컬 애플리케이션 이미지 사용

별도로 빌드한 로컬 Docker 이미지가 있을 때 레지스트리에 업로드하지 않고 테스트 클러스터에 로드할 수 있다.

```sh
make kind-load IMAGE=example-image:dev
```

`example-image:dev`는 실제 로컬 이미지 이름과 태그로 바꾼다. 이 명령은 이미지를 빌드하거나 애플리케이션을 배포하지 않는다. 애플리케이션 Deployment는 해당 이미지 이름과 `imagePullPolicy: IfNotPresent` 또는 `Never`를 사용해야 한다. 이미 실행 중인 Pod는 이미지 로드만으로 교체되지 않는다. `tests/kind/manifests/`에 애플리케이션 리소스를 추가하면 `kustomization.yaml`의 `resources`에도 등록한다.

스크립트는 Docker daemon의 플랫폼에 해당하는 이미지만 임시 아카이브로 내보내고 `kind load image-archive`로 모든 노드에 로드한다. 아카이브는 명령 종료 시 삭제한다. 따라서 다른 플랫폼의 내려받지 않은 이미지 manifest를 함께 로드하지 않는다. `kind load docker-image`의 기본 저장 경로는 플랫폼을 지정하지 않는다. 구현 근거는 [`kubernetes-sigs/kind`, `pkg/cmd/kind/load/docker-image/docker-image.go`, `save`, v0.33.0](https://github.com/kubernetes-sigs/kind/blob/v0.33.0/pkg/cmd/kind/load/docker-image/docker-image.go)이다.

## 환경 삭제와 문제 확인

```sh
make kind-down
```

이 명령은 선택한 테스트 클러스터와 전용 kubeconfig를 삭제한다. 클러스터 내부에 저장된 Redis 데이터도 삭제된다. 호스트의 Docker 이미지는 삭제하지 않는다.

생성 실패나 테스트 실패 시 다음 항목을 확인한다.

- Docker daemon에 접근할 수 있는지 확인한다.
- `18080` 포트를 다른 프로세스가 사용 중인지 확인한다.
- `make kind-status`와 스크립트가 출력한 이벤트에서 이미지 다운로드 실패 또는 Pod 스케줄링 실패를 확인한다.
- HTTP Pod가 `Pending`이면 두 worker가 준비되어 있는지 확인한다.
- Redis Pod가 `Pending`이면 `make kind-status`에서 PVC가 `Bound` 상태인지 확인한다.
- 이미지 다운로드나 클러스터 준비가 오래 걸리면 `KIND_WAIT_TIMEOUT`을 늘린다.
- `config.yaml`의 노드, 이미지 또는 포트 설정을 변경했다면 `make kind-down` 후 다시 생성한다.
