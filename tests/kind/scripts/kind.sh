#!/usr/bin/env bash
set -Eeuo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
KIND_DIR="$(cd -- "${SCRIPT_DIR}/.." && pwd)"
readonly KIND_DIR
readonly CLUSTER_NAME="${KIND_CLUSTER_NAME:-token-router-test}"
readonly WAIT_TIMEOUT="${KIND_WAIT_TIMEOUT:-180s}"
readonly HTTP_URL="${KIND_HTTP_URL:-http://127.0.0.1:18080}"
readonly NAMESPACE=token-router-test
readonly EXPECTED_RESPONSE=token-router-kind-ready
export KIND_EXPERIMENTAL_PROVIDER=docker

if [[ ! "$CLUSTER_NAME" =~ ^[a-z0-9]([-a-z0-9]*[a-z0-9])?$ ]]; then
  printf 'Invalid KIND_CLUSTER_NAME: %s\n' "$CLUSTER_NAME" >&2
  exit 1
fi
readonly KUBECONFIG_PATH="${KIND_DIR}/.kube/${CLUSTER_NAME}.yaml"
readonly CONTEXT="kind-${CLUSTER_NAME}"

usage() {
  cat <<'USAGE'
make kind-up                  Create the cluster, deploy components and HTTP fixtures.
make kind-test                Check cluster DNS, HTTP, and Redis PING/SET/GET.
make kind-status              Show nodes and test namespace resources.
make kind-render              Render Kubernetes manifests without a cluster.
make kind-load IMAGE=name:tag  Load a local Docker image into all cluster nodes.
make kind-down                Delete the test cluster and its kubeconfig.

Environment: KIND_CLUSTER_NAME, KIND_WAIT_TIMEOUT, KIND_HTTP_URL
Direct usage: bash tests/kind/scripts/kind.sh {up|test|status|down|load IMAGE...}
USAGE
}

require_command() {
  if ! command -v "$1" >/dev/null 2>&1; then
    printf 'Required command is missing: %s\n' "$1" >&2
    exit 1
  fi
}

kube() {
  kubectl --kubeconfig "$KUBECONFIG_PATH" --context "$CONTEXT" "$@"
}

require_kubeconfig() {
  if [[ ! -f "$KUBECONFIG_PATH" ]]; then
    printf 'Test kubeconfig is missing. Run make kind-up first.\n' >&2
    exit 1
  fi
}

diagnostics() {
  local exit_status=$?
  trap - ERR
  printf 'Command failed. Test cluster diagnostics:\n' >&2
  if [[ -f "$KUBECONFIG_PATH" ]]; then
    kube --request-timeout=10s get nodes -o wide >&2 || true
    kube --request-timeout=10s -n "$NAMESPACE" get pods,services,statefulsets,pvc,jobs -o wide >&2 || true
    kube --request-timeout=10s -n "$NAMESPACE" get events --sort-by=.lastTimestamp >&2 || true
    kube --request-timeout=10s -n "$NAMESPACE" logs job/kind-smoke --all-containers=true >&2 || true
    kube --request-timeout=10s -n "$NAMESPACE" logs statefulset/redis --tail=50 >&2 || true
  fi
  exit "$exit_status"
}

wait_ready() {
  kube wait --for=condition=Ready nodes --all --timeout "$WAIT_TIMEOUT"
  kube -n kube-system rollout status deployment/coredns --timeout "$WAIT_TIMEOUT"
  kube -n "$NAMESPACE" rollout status deployment/kind-echo --timeout "$WAIT_TIMEOUT"
  kube -n "$NAMESPACE" rollout status statefulset/redis --timeout "$WAIT_TIMEOUT"
}

up() {
  local clusters
  docker info >/dev/null
  mkdir -p "${KIND_DIR}/.kube"
  clusters="$(kind get clusters)"
  if [[ $'\n'"$clusters"$'\n' == *$'\n'"$CLUSTER_NAME"$'\n'* ]]; then
    printf 'Reusing kind cluster: %s\n' "$CLUSTER_NAME"
    kind export kubeconfig --name "$CLUSTER_NAME" --kubeconfig "$KUBECONFIG_PATH"
  else
    kind create cluster --name "$CLUSTER_NAME" --config "${KIND_DIR}/config.yaml" \
      --kubeconfig "$KUBECONFIG_PATH" --wait "$WAIT_TIMEOUT"
  fi
  kube apply -k "${KIND_DIR}/manifests"
  wait_ready
  printf 'Test environment is ready: %s\nKubeconfig: %s\n' "$HTTP_URL" "$KUBECONFIG_PATH"
}

smoke_test() {
  require_kubeconfig
  wait_ready
  kube -n "$NAMESPACE" delete job kind-smoke --ignore-not-found --wait=true --timeout "$WAIT_TIMEOUT"
  kube apply -f "${KIND_DIR}/smoke/job.yaml"
  kube -n "$NAMESPACE" wait --for=condition=complete job/kind-smoke --timeout "$WAIT_TIMEOUT"
  kube -n "$NAMESPACE" logs job/kind-smoke --all-containers=true

  local response attempt
  for (( attempt=1; attempt<=30; attempt++ )); do
    if response="$(curl --noproxy '*' --fail --silent --show-error \
      --connect-timeout 2 --max-time 5 "$HTTP_URL")"; then
      if [[ "$response" == "$EXPECTED_RESPONSE" ]]; then
        printf 'Host HTTP check passed: %s\n' "$HTTP_URL"
        return 0
      fi
      printf 'Unexpected HTTP response: %s\n' "$response" >&2
      return 1
    fi
    sleep 1
  done
  printf 'Host HTTP check failed: %s\n' "$HTTP_URL" >&2
  return 1
}

load_images() {
  require_kubeconfig
  local platform
  platform="$(docker version --format '{{.Server.Os}}/{{.Server.Arch}}')"
  IMAGE_ARCHIVE="$(mktemp "${TMPDIR:-/tmp}/token-router-kind-images.XXXXXX")"
  trap 'unlink "$IMAGE_ARCHIVE"' EXIT
  docker image save --platform "$platform" --output "$IMAGE_ARCHIVE" "$@"
  kind load image-archive --name "$CLUSTER_NAME" "$IMAGE_ARCHIVE"
  printf 'Loaded images for %s into cluster %s.\n' "$platform" "$CLUSTER_NAME"
}

action="${1:-help}"
if (( $# > 0 )); then
  shift
fi
case "$action" in
  help|-h|--help)
    usage
    exit 0
    ;;
  up|test|status|down)
    if (( $# != 0 )); then
      usage >&2
      exit 1
    fi
    ;;
  load)
    if (( $# == 0 )) || [[ -z "$1" ]]; then
      printf 'Specify an image: make kind-load IMAGE=name:tag\n' >&2
      exit 1
    fi
    ;;
  *)
    usage >&2
    exit 1
    ;;
esac

require_command kubectl
case "$action" in
  up|down|load)
    require_command docker
    require_command kind
    ;;
  test)
    require_command curl
    ;;
esac
trap diagnostics ERR

case "$action" in
  up)
    up
    ;;
  test)
    smoke_test
    ;;
  status)
    require_kubeconfig
    kube get nodes -o wide
    kube -n "$NAMESPACE" get deployments,statefulsets,pods,services,pvc,jobs -o wide
    ;;
  down)
    kind delete cluster --name "$CLUSTER_NAME" --kubeconfig "$KUBECONFIG_PATH"
    if [[ -f "$KUBECONFIG_PATH" ]]; then
      unlink "$KUBECONFIG_PATH"
    fi
    ;;
  load)
    load_images "$@"
    ;;
esac
