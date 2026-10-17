# control-center: a static Go binary on distroless. Build from the repository root:
#   docker build -f cmd/control-center/Dockerfile -t ghcr.io/usr-lex/swarmageddon/control-center:dev .
# The web UI build stage (Vite, into web/dist) will be added in front of the
# Go stage; until then web/embed.go serves a placeholder page.
FROM golang:1.26 AS build
ARG VERSION=dev
ENV GOTOOLCHAIN=go1.26.9 CGO_ENABLED=0
WORKDIR /src
# BUILD_CA (Makefile): a TLS-intercepting proxy's CA, for downloads only.
COPY go.mod go.sum ./
RUN --mount=type=secret,id=build_ca,required=false \
    if [ -s /run/secrets/build_ca ]; then export SSL_CERT_FILE=/run/secrets/build_ca; fi; \
    go mod download
COPY . .
RUN go build -trimpath -ldflags "-s -w -X github.com/usr-lex/swarmageddon/internal/version.Version=${VERSION}" \
      -o /out/control-center ./cmd/control-center

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/control-center /control-center
USER 65532:65532
EXPOSE 8443 8080
ENTRYPOINT ["/control-center"]
