#!/usr/bin/env bash
set -Eeuo pipefail

E2E_SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
E2E_PROJECT="token-router-e2e-$(date +%s)-$$-${RANDOM}"
readonly E2E_SCRIPT_DIR E2E_PROJECT
readonly DOCKER_COMMAND="${DOCKER:-docker}"
E2E_COMMAND_PID=""

if ! command -v "$DOCKER_COMMAND" >/dev/null 2>&1; then
  printf 'Required command is missing: %s\n' "$DOCKER_COMMAND" >&2
  exit 1
fi
"$DOCKER_COMMAND" compose version >/dev/null
readonly -a COMPOSE=("$DOCKER_COMMAND" compose --project-name "$E2E_PROJECT" --file "${E2E_SCRIPT_DIR}/compose.yaml")

cleanup() {
  local exit_status=$?
  trap - EXIT INT TERM
  if [[ -n "$E2E_COMMAND_PID" ]]; then
    kill -TERM "$E2E_COMMAND_PID" 2>/dev/null || true
    wait "$E2E_COMMAND_PID" 2>/dev/null || true
  fi
  if (( exit_status != 0 )); then
    "${COMPOSE[@]}" logs --no-color --tail=100 >&2 || true
  fi
  if ! "${COMPOSE[@]}" down --volumes --remove-orphans --rmi local --timeout 10; then
    if (( exit_status == 0 )); then
      exit_status=1
    fi
  fi
  exit "$exit_status"
}

run_compose() {
  local exit_status=0
  "${COMPOSE[@]}" "$@" &
  E2E_COMMAND_PID=$!
  wait "$E2E_COMMAND_PID" || exit_status=$?
  E2E_COMMAND_PID=""
  return "$exit_status"
}

trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

printf 'Running E2E scenarios in Compose project %s\n' "$E2E_PROJECT"
run_compose build e2e
run_compose up --detach --wait --wait-timeout 60 redis otel-collector prometheus tempo loki
run_compose run --rm --no-deps e2e
