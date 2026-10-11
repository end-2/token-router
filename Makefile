.DEFAULT_GOAL := help

KIND_CLUSTER_NAME ?= token-router-test
KIND_WAIT_TIMEOUT ?= 180s
KIND_HTTP_URL ?= http://127.0.0.1:18080
export KIND_CLUSTER_NAME KIND_WAIT_TIMEOUT KIND_HTTP_URL IMAGE

.PHONY: help kind-up kind-test kind-down kind-status kind-render kind-load

help:
	@bash tests/kind/scripts/kind.sh help

kind-up:
	@bash tests/kind/scripts/kind.sh up

kind-test:
	@bash tests/kind/scripts/kind.sh test

kind-down:
	@bash tests/kind/scripts/kind.sh down

kind-status:
	@bash tests/kind/scripts/kind.sh status

kind-render:
	@kubectl kustomize tests/kind/manifests

kind-load:
	@bash tests/kind/scripts/kind.sh load "$${IMAGE:-}"
