.DEFAULT_GOAL := help

DOCKER ?= docker
APP_IMAGE ?= token-router:local
TEST_IMAGE ?= token-router-test:local
LINT_IMAGE ?= token-router-lint:local
export DOCKER

KIND_CLUSTER_NAME ?= token-router-test
KIND_WAIT_TIMEOUT ?= 180s
KIND_HTTP_URL ?= http://127.0.0.1:18080
export KIND_CLUSTER_NAME KIND_WAIT_TIMEOUT KIND_HTTP_URL IMAGE

.PHONY: help build image run test vet lint e2e fmt test-image lint-image \
	build-local test-local vet-local lint-local \
	kind-up kind-test kind-down kind-status kind-render kind-load

help:
	@echo 'Docker: make build, image, test, vet, lint, e2e'
	@echo 'Local Go: make run, fmt, build-local, test-local, vet-local, lint-local'
	@bash tests/kind/scripts/kind.sh help

build:
	$(DOCKER) build --target binary --output type=local,dest=bin .

image:
	$(DOCKER) build --target runtime --tag $(APP_IMAGE) .

run:
	go run ./src/cmd/token-router serve

test-image:
	$(DOCKER) build --target test --tag $(TEST_IMAGE) .

test: test-image
	$(DOCKER) run --rm $(TEST_IMAGE)

vet: test-image
	$(DOCKER) run --rm $(TEST_IMAGE) go vet -tags=e2e ./...

lint-image:
	$(DOCKER) build --target lint --tag $(LINT_IMAGE) .

lint: lint-image
	$(DOCKER) run --rm $(LINT_IMAGE)

e2e:
	@bash tests/e2e/run.sh

build-local:
	go build -o bin/token-router ./src/cmd/token-router

test-local:
	go test -race ./...

vet-local:
	go vet -tags=e2e ./...

lint-local:
	golangci-lint run

fmt:
	gofmt -w src tests/e2e

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
