# syntax=docker/dockerfile:1.7
#
# The CI runner image (docs/design.md §9.4): the room image plus
# forgejo-runner. One pod runs one job (`forgejo-runner one-job ...`), with
# Podman serving /var/run/docker.sock for the job's containers.
#
#   docker buildx build -f images/runner/Dockerfile \
#     --build-arg ROOM_IMAGE=ghcr.io/usr-lex/swarmageddon/room:<version> .

ARG ROOM_IMAGE=ghcr.io/usr-lex/swarmageddon/room:dev
FROM ${ROOM_IMAGE}
ARG TARGETARCH
ARG RUNNER_VERSION=13.2.0
ARG RUNNER_SHA256_AMD64=fadaec897f5e6641c363f87ecaf75f866f99c81b7097ec6b433e4c48123fcad1
ARG RUNNER_SHA256_ARM64=9d3d462ee83e4e629f959faeacceb20eed2f25290bddea402e65ace46a31afd9
# Where the release binaries come from; override to use a mirror.
ARG RUNNER_URL=https://code.forgejo.org/forgejo/runner/releases/download

RUN --mount=type=secret,id=build_ca,required=false \
    set -eu; \
    if [ -s /run/secrets/build_ca ]; then export CURL_CA_BUNDLE=/run/secrets/build_ca; fi; \
    case "${TARGETARCH}" in \
      amd64) sum=${RUNNER_SHA256_AMD64} ;; \
      arm64) sum=${RUNNER_SHA256_ARM64} ;; \
      *) echo "unsupported arch ${TARGETARCH}" >&2; exit 1 ;; \
    esac; \
    curl -fsSL -o /usr/local/bin/forgejo-runner \
      "${RUNNER_URL}/v${RUNNER_VERSION}/forgejo-runner-${RUNNER_VERSION}-linux-${TARGETARCH}"; \
    echo "${sum}  /usr/local/bin/forgejo-runner" | sha256sum -c -; \
    chmod 0755 /usr/local/bin/forgejo-runner; \
    forgejo-runner --version

COPY --chmod=0755 images/runner/runner-entrypoint /usr/local/bin/runner-entrypoint
WORKDIR /data
ENTRYPOINT ["tini", "--", "/usr/local/bin/runner-entrypoint"]
CMD ["forgejo-runner", "--help"]
