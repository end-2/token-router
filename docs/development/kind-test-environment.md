# kind test environment

`tests/kind/` creates a Kubernetes test environment on Docker. It deploys Redis, Collector, Prometheus, Tempo, Loki, and Grafana from `deploy/components/`, together with a sample HTTP service named `kind-echo`, to check cluster readiness and network paths. It does not automatically deploy the proxy application in `src/`. See [Proxy server development and execution](proxy-server.md) for proxy image builds and execution, [Ginkgo E2E tests](end-to-end-tests.md) for application scenarios, and [Deployment components and OpenTelemetry](deployment-components.md) for component settings and data paths.

## Required tools

- A running Docker Engine or Docker Desktop
- `kind` v0.33.0
- `kubectl` matching the Kubernetes v1.37 minor version
- Bash, `curl`, and GNU Make
- Network access to download container images
- Available host TCP port `18080`

Local image loading additionally requires a Docker CLI supporting `docker image save --platform` and Docker API 1.48 or newer. See [Docker image save guidance](https://docs.docker.com/reference/cli/docker/image/save/#save-a-specific-platform---platform) for option requirements.

See the [kind Quick Start](https://kind.sigs.k8s.io/docs/user/quick-start/) and [kubectl installation guidance](https://kubernetes.io/docs/tasks/tools/#kubectl) for tool installation.

```sh
docker info
kind version
kubectl version --client
```

Node images use the Kubernetes v1.37.0 tag and digest from the [kind v0.33.0 release](https://github.com/kubernetes-sigs/kind/releases/tag/v0.33.0), matching `kind`'s [`pkg/apis/config/defaults/image.go`, `Image`](https://github.com/kubernetes-sigs/kind/blob/v0.33.0/pkg/apis/config/defaults/image.go). Sample HTTP server and smoke Job images are also pinned by digest.

## Creation and validation

Run from the repository root:

```sh
make kind-render
make kind-up
make kind-test
make kind-status
```

`make kind-render` prints deployment resources through Kustomize without accessing a cluster. The smoke Job is deployed separately during testing.

`make kind-up` creates the cluster and deploys the sample HTTP service and all deployment components. An existing cluster of the same name is reused. It waits for nodes, CoreDNS, the HTTP Deployment, Redis StatefulSet, and observability Deployments to become ready. Reusing a cluster does not change node layout or port mappings from `config.yaml`.

`make kind-test` verifies:

1. Readiness of all nodes, CoreDNS, the HTTP Deployment, Redis StatefulSet, and observability Deployments
2. HTTP requests from a Job using the Service DNS name and the response body
3. HTTP requests from the host through Docker port mapping and the NodePort Service and the response body
4. Redis `PING`, `SET`, and `GET` responses from a Job using the Service DNS name
5. Collector, Prometheus, Tempo, Loki, and Grafana health checks, and OTLP HTTP ingestion at Collector `/v1/metrics`, `/v1/logs`, and `/v1/traces`

The Job is recreated for each run. Kubernetes cleans it up 300 seconds after completion. On failure, the script prints nodes, Pods, Services, StatefulSets, PVCs, Jobs, events, and container logs.

Check the response directly from the host:

```sh
curl --noproxy '*' --fail http://127.0.0.1:18080
```

## Cluster configuration

| Item | Setting |
| --- | --- |
| Cluster name | `token-router-test` |
| kubeconfig context | `kind-token-router-test` |
| Nodes | One control-plane and two workers |
| Test namespace | `token-router-test` |
| HTTP Deployment | `kind-echo`, two replicas |
| In-cluster HTTP address | `http://kind-echo.token-router-test.svc.cluster.local` |
| Host HTTP address | `http://127.0.0.1:18080` |
| Port path | Host `18080` -> control-plane `30080` -> Service `80` -> Pod `5678` |
| HTTP response body | `token-router-kind-ready` |
| In-cluster Redis address | `redis.token-router-test.svc.cluster.local:6379` |
| Redis StatefulSet | `redis`, one replica, `1Gi` PVC |

`podAntiAffinity` places the two HTTP Pods on separate nodes. No toleration is added for the default control-plane taint, so HTTP Pods run on the two workers.

The Deployment's `maxSurge: 0` and `maxUnavailable: 1` allow HTTP Pods to be replaced one at a time even when the two workers cannot accommodate an additional Pod.

The kind `extraPortMappings.containerPort` and Service `nodePort` both use `30080` and must match. See [kind Configuration's NodePort guidance](https://kind.sigs.k8s.io/docs/user/configuration/#nodeport-with-port-mappings). Kubernetes API and HTTP ports bind to host address `127.0.0.1`.

`kind-echo` configures its response and port using HashiCorp `http-echo` v1.0.0 arguments `-text` and `-listen`. Readiness and liveness probes use `/health`. See [`hashicorp/http-echo`, `main.go`, `main` and `httpHealth`, v1.0.0](https://github.com/hashicorp/http-echo/blob/v1.0.0/main.go) for implementation details.

## kubeconfig and configuration changes

The script stores a dedicated test kubeconfig at `tests/kind/.kube/<cluster-name>.yaml` and specifies its path and context for every `kubectl` command. This directory is in `.gitignore`. Existing default kubeconfig files and contexts are not changed. The Docker provider is selected explicitly.

Also specify the test kubeconfig when inspecting resources directly:

```sh
kubectl --kubeconfig tests/kind/.kube/token-router-test.yaml \
  --context kind-token-router-test -n token-router-test get pods -o wide
```

| Environment or Make variable | Default | Purpose |
| --- | --- | --- |
| `KIND_CLUSTER_NAME` | `token-router-test` | Cluster name and kubeconfig filename |
| `KIND_WAIT_TIMEOUT` | `180s` | Wait time for cluster creation and Kubernetes readiness |
| `KIND_HTTP_URL` | `http://127.0.0.1:18080` | Host HTTP validation address |
| `IMAGE` | None | Local image loaded by `make kind-load` |

```sh
make kind-up KIND_CLUSTER_NAME=token-router-integration KIND_WAIT_TIMEOUT=300s
make kind-test KIND_CLUSTER_NAME=token-router-integration KIND_WAIT_TIMEOUT=300s
make kind-down KIND_CLUSTER_NAME=token-router-integration
```

Changing the cluster name leaves the namespace and host port unchanged. Multiple clusters using this configuration concurrently need different host ports.

To change the host port, edit `hostPort` in `config.yaml`, then delete and recreate the cluster. Pass the updated address through `KIND_HTTP_URL` to creation and validation commands. Changing `KIND_HTTP_URL` alone does not change the actual port mapping.

When changing Kubernetes versions, update all node images and digests in `config.yaml` together, and use a matching `kind` release and `kubectl`.

## Using local application images

Build an application Docker image and load it into the test cluster without uploading it to a registry:

```sh
make image APP_IMAGE=token-router:dev
make kind-load IMAGE=token-router:dev
```

`make kind-load` neither builds images nor deploys the application. Application Deployments must use the image name and `imagePullPolicy: IfNotPresent` or `Never`. Loading an image alone does not replace running Pods. If adding application resources to `tests/kind/manifests/`, also register them in `kustomization.yaml` under `resources`.

The script exports only the Docker daemon's platform image to a temporary archive and loads it into all nodes with `kind load image-archive`. The archive is removed when the command exits. This avoids loading undownloaded image manifests for other platforms. The default save path for `kind load docker-image` does not specify a platform. See [`kubernetes-sigs/kind`, `pkg/cmd/kind/load/docker-image/docker-image.go`, `save`, v0.33.0](https://github.com/kubernetes-sigs/kind/blob/v0.33.0/pkg/cmd/kind/load/docker-image/docker-image.go).

## Directory layout

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

- [`config.yaml`](../../tests/kind/config.yaml): kind nodes, Kubernetes images, and host port mappings
- [`manifests/`](../../tests/kind/manifests/): test namespace, HTTP Deployment and NodePort Service, and shared deployment component references
- [`smoke/job.yaml`](../../tests/kind/smoke/job.yaml): Job checking cluster DNS, HTTP, Redis, and observability Service responses
- [`scripts/kind.sh`](../../tests/kind/scripts/kind.sh): environment creation, validation, status inspection, image loading, and deletion

## Deletion and troubleshooting

```sh
make kind-down
```

This command deletes the selected test cluster and its dedicated kubeconfig. Redis and observability data inside the cluster are also deleted. Host Docker images are retained.

If creation or testing fails, check the following:

- Verify Docker daemon access.
- Check whether another process is using port `18080`.
- Look for image download or Pod scheduling failures in `make kind-status` and events printed by the script.
- If HTTP Pods are `Pending`, check that both workers are ready.
- If the Redis Pod is `Pending`, check that its PVC is `Bound` in `make kind-status`.
- Increase `KIND_WAIT_TIMEOUT` if image downloads or cluster readiness take longer.
- If nodes, images, or ports in `config.yaml` changed, run `make kind-down` and recreate the cluster.
