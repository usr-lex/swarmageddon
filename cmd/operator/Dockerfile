# Build from the repository root:
#   docker build -f cmd/operator/Dockerfile --build-arg VERSION=v0.1.0 -t ghcr.io/usr-lex/swarmageddon/operator:v0.1.0 .
FROM --platform=$BUILDPLATFORM golang:1.26 AS build
ARG TARGETOS TARGETARCH VERSION=dev
WORKDIR /src
# BUILD_CA (Makefile): a TLS-intercepting proxy's CA, for downloads only.
COPY go.mod go.sum ./
RUN --mount=type=secret,id=build_ca,required=false \
    if [ -s /run/secrets/build_ca ]; then export SSL_CERT_FILE=/run/secrets/build_ca; fi; \
    go mod download
COPY api/ api/
COPY gen/ gen/
COPY internal/ internal/
COPY cmd/operator/ cmd/operator/
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
      -ldflags "-s -w -X github.com/usr-lex/swarmageddon/internal/version.Version=${VERSION}" \
      -o /out/operator ./cmd/operator

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/operator /operator
USER 65532:65532
ENTRYPOINT ["/operator"]
