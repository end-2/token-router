# syntax=docker/dockerfile:1

FROM golang:1.27.2-bookworm@sha256:5cf287a799e6b94384bad13d16b14904c531f51ba65792237e122ce42b392f61 AS dependencies
WORKDIR /workspace
ENV GOTOOLCHAIN=local
COPY go.mod go.sum ./
RUN go mod download

FROM dependencies AS source
COPY src ./src
COPY tests/e2e ./tests/e2e
COPY config.yaml .golangci.yaml ./

FROM source AS test
CMD ["go", "test", "-race", "-count=1", "./..."]

FROM dependencies AS lint-tool
ARG GOLANGCI_LINT_VERSION=v2.14.0
RUN --mount=type=cache,target=/root/.cache/go-build \
    go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@${GOLANGCI_LINT_VERSION}

FROM source AS lint
COPY --from=lint-tool /go/bin/golangci-lint /usr/local/bin/golangci-lint
CMD ["golangci-lint", "run"]

FROM source AS builder
RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/token-router ./src/cmd/token-router

FROM scratch AS binary
COPY --from=builder /out/token-router /token-router

FROM dependencies AS ginkgo-tool
RUN --mount=type=cache,target=/root/.cache/go-build \
    go install github.com/onsi/ginkgo/v2/ginkgo

FROM source AS e2e
COPY --from=ginkgo-tool /go/bin/ginkgo /usr/local/bin/ginkgo
COPY --from=builder /out/token-router /usr/local/bin/token-router
ENV TOKEN_ROUTER_E2E_BINARY=/usr/local/bin/token-router
CMD ["ginkgo", "--tags=e2e", "--race", "--timeout=10m", "--fail-on-pending", "--fail-on-empty", "./tests/e2e"]

FROM scratch AS runtime
COPY --from=dependencies /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=builder /out/token-router /usr/local/bin/token-router
WORKDIR /app
COPY --chown=65532:65532 config.yaml ./config.yaml
USER 65532:65532
ENV TOKEN_ROUTER_SERVER_ADDRESS=:8080 \
    TOKEN_ROUTER_TELEMETRY_METRICS_PROMETHEUS_ADDRESS=:9090
EXPOSE 8080 9090
STOPSIGNAL SIGTERM
ENTRYPOINT ["/usr/local/bin/token-router"]
CMD ["serve"]
