# control-center: a static Go binary on distroless. Build from the repository root:
#   docker build -f cmd/control-center/Dockerfile -t ghcr.io/usr-lex/swarmageddon/control-center:dev .
# The web UI is built by Vite into web/dist and embedded in the binary
# (web/embed.go); the build fails if the UI did not build, rather than ship
# the placeholder page.
FROM node:24 AS web
WORKDIR /web
COPY web/package.json web/package-lock.json ./
RUN --mount=type=secret,id=build_ca,required=false --mount=type=cache,target=/root/.npm \
    if [ -s /run/secrets/build_ca ]; then export NODE_EXTRA_CA_CERTS=/run/secrets/build_ca; fi; \
    npm ci
COPY web/ ./
RUN npm run build && test -s dist/index.html

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
COPY --from=web /web/dist/ web/dist/
RUN test -s web/dist/index.html
RUN go build -trimpath -ldflags "-s -w -X github.com/usr-lex/swarmageddon/internal/version.Version=${VERSION}" \
      -o /out/control-center ./cmd/control-center

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/control-center /control-center
USER 65532:65532
EXPOSE 8443 8080
ENTRYPOINT ["/control-center"]
