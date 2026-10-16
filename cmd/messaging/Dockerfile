# Build from the repository root:
#   docker build -f cmd/messaging/Dockerfile -t ghcr.io/usr-lex/swarmageddon/messaging:dev .
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath \
      -ldflags "-s -w -X github.com/usr-lex/swarmageddon/internal/version.Version=${VERSION}" \
      -o /out/messaging ./cmd/messaging

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/messaging /messaging
# 8443 Connect API (mTLS), 8080 health, 6222 NATS routes. NATS clients: 127.0.0.1:4222 only.
EXPOSE 8443 8080 6222
VOLUME /data
USER 65532:65532
ENTRYPOINT ["/messaging"]
