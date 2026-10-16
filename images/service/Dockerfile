# syntax=docker/dockerfile:1.7
#
# Default image for a single static Go binary (operator, control-center,
# messaging, mockmodel). `make images` uses cmd/<name>/Dockerfile when a
# component has its own, and this file otherwise:
#
#   docker buildx build -f images/service/Dockerfile --build-arg BINARY=operator .
#
# The binary is the ENTRYPOINT, so Kubernetes manifests pass only `args`.

ARG GO_VERSION=1.26

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION} AS build
ARG TARGETOS TARGETARCH
ARG BINARY
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=secret,id=build_ca,required=false \
    --mount=type=cache,target=/go/pkg/mod \
    if [ -s /run/secrets/build_ca ]; then export SSL_CERT_FILE=/run/secrets/build_ca; fi; \
    go mod download
COPY . .
RUN --mount=type=secret,id=build_ca,required=false \
    --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    if [ -s /run/secrets/build_ca ]; then export SSL_CERT_FILE=/run/secrets/build_ca; fi; \
    test -n "${BINARY}" && \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath \
      -ldflags "-s -w -X github.com/usr-lex/swarmageddon/internal/version.Version=${VERSION}" \
      -o /out/app ./cmd/${BINARY}

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/app /app
USER 65532:65532
ENTRYPOINT ["/app"]
