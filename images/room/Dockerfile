# syntax=docker/dockerfile:1.7
#
# The room image (docs/design.md §6.1). One image for every room; the room's
# identity is applied at start by the supervisor, never baked in.
#
# Ubuntu 26.04 rather than Fedora (design §6.1 says Fedora): Ubuntu's archive
# is reachable from our build environments and Fedora's mirrors are not.
#
# Build context: the repository root.
#   docker buildx build -f images/room/Dockerfile --platform linux/amd64,linux/arm64 .
#
# Behind a TLS-intercepting proxy, pass its CA as a build secret:
#   --secret id=build_ca,src=/path/to/ca.crt
# It is trusted during the build and removed from the final trust store.

ARG UBUNTU_VERSION=26.04
ARG GO_VERSION=1.26

# ---- our binaries: supervisor and swarm (cross-compiled, no QEMU) ----
FROM --platform=$BUILDPLATFORM golang:${GO_VERSION} AS gobuild
ARG TARGETOS TARGETARCH
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
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath \
      -ldflags "-s -w -X github.com/usr-lex/swarmageddon/internal/version.Version=${VERSION}" \
      -o /out/ ./cmd/supervisor ./cmd/swarm

# ---- the room ----
FROM ubuntu:${UBUNTU_VERSION} AS room
ARG TARGETARCH
ARG NVM_VERSION=v0.40.8
ARG NODE_VERSION=24
ARG CLAUDE_CODE_VERSION=2.1.301
ARG ORE_VERSION=1.161.0
ARG PNPM_VERSION=12.14.0
ARG UV_VERSION=0.13.1
ARG UV_SHA256_AMD64=eadd9d27c134ab5ac1ffd4473b97a741407f3ea238288f03121fc4deddf90220
ARG UV_SHA256_ARM64=9041c73c99fdc5892cf128140beb354eca9231d502068cc36e6cb53a8e59ad14
ARG TEA_VERSION=0.16.0
ARG TEA_SHA256_AMD64=9c142c8a0b31485eea2387031c1c404f12b7141075eb4ffbd7ced49441590636
ARG TEA_SHA256_ARM64=0e14a036322eeb1fc55e266acafeb8490e523ed99f9232034ae74861e2339879
# Firefox ESR from Mozilla's tarballs: Ubuntu's firefox package is a snap shim.
# Set FIREFOX_VERSION="" to leave the browser out.
ARG FIREFOX_VERSION=153.5.0esr
ARG FIREFOX_SHA256_AMD64=2cdcc982f742664fb98579b1f1faa3238d574b01d66711e65b05c7df01562d38
ARG FIREFOX_SHA256_ARM64=6579949c1d2d98f0d045cec780fa24752b57984f6e52d189ccfe003cf97f3d0b
# room-desktop variant: VS Code. Empty means no VS Code.
ARG VSCODE_VERSION=""

ENV DEBIAN_FRONTEND=noninteractive \
    LANG=C.UTF-8 \
    LC_ALL=C.UTF-8

# Base packages, the container engine and the display stack.
# Ubuntu ships a "ubuntu" user with uid 1000; the supervisor creates the agent
# user with that uid at start, so it goes.
RUN --mount=type=secret,id=build_ca,required=false \
    --mount=type=cache,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,target=/var/lib/apt/lists,sharing=locked \
    rm -f /etc/apt/apt.conf.d/docker-clean \
    && apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates \
    && if [ -s /run/secrets/build_ca ]; then \
         cp /run/secrets/build_ca /usr/local/share/ca-certificates/zz-build-ca.crt && update-ca-certificates; \
       fi \
    && apt-get install -y --no-install-recommends \
        git curl gnupg openssh-client sudo tini \
        build-essential pkg-config python3 python3-venv python3-pip \
        jq ripgrep tmux less nano vim-tiny unzip zip xz-utils file rsync \
        procps psmisc iproute2 iputils-ping dnsutils lsof strace \
        locales tzdata bash-completion shellcheck \
        podman crun fuse-overlayfs uidmap slirp4netns passt netavark aardvark-dns catatonit nftables \
        xvfb xauth x11-xserver-utils openbox x11vnc xterm x11-utils dbus-x11 \
        fonts-dejavu-core fonts-liberation2 fonts-noto-color-emoji \
        libgtk-3-0t64 libasound2t64 libx11-xcb1 libdbus-glib-1-2 libxtst6 libpci3 \
    && (userdel -r ubuntu 2>/dev/null || true)

# Docker CLI and the compose plugin (no daemon: Podman serves
# /var/run/docker.sock, ADR 0001).
RUN --mount=type=cache,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,target=/var/lib/apt/lists,sharing=locked \
    install -m 0755 -d /etc/apt/keyrings \
    && curl -fsSL https://download.docker.com/linux/ubuntu/gpg -o /etc/apt/keyrings/docker.asc \
    && . /etc/os-release \
    && echo "deb [arch=${TARGETARCH} signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/ubuntu ${VERSION_CODENAME} stable" \
        > /etc/apt/sources.list.d/docker.list \
    && apt-get update \
    && apt-get install -y --no-install-recommends docker-ce-cli docker-compose-plugin

# uv and tea, pinned and checksummed.
RUN set -eu; \
    case "${TARGETARCH}" in \
      amd64) uvarch=x86_64; uvsum=${UV_SHA256_AMD64}; teasum=${TEA_SHA256_AMD64} ;; \
      arm64) uvarch=aarch64; uvsum=${UV_SHA256_ARM64}; teasum=${TEA_SHA256_ARM64} ;; \
      *) echo "unsupported arch ${TARGETARCH}" >&2; exit 1 ;; \
    esac; \
    curl -fsSL -o /tmp/uv.tgz "https://github.com/astral-sh/uv/releases/download/${UV_VERSION}/uv-${uvarch}-unknown-linux-gnu.tar.gz"; \
    echo "${uvsum}  /tmp/uv.tgz" | sha256sum -c -; \
    tar -xzf /tmp/uv.tgz -C /usr/local/bin --strip-components=1; \
    curl -fsSL -o /usr/local/bin/tea "https://dl.gitea.com/tea/${TEA_VERSION}/tea-${TEA_VERSION}-linux-${TARGETARCH}"; \
    echo "${teasum}  /usr/local/bin/tea" | sha256sum -c -; \
    chmod 0755 /usr/local/bin/tea /usr/local/bin/uv /usr/local/bin/uvx; \
    rm -f /tmp/uv.tgz

# Firefox ESR.
RUN set -eu; \
    [ -n "${FIREFOX_VERSION}" ] || exit 0; \
    case "${TARGETARCH}" in \
      amd64) ffarch=x86_64; ffsum=${FIREFOX_SHA256_AMD64} ;; \
      arm64) ffarch=aarch64; ffsum=${FIREFOX_SHA256_ARM64} ;; \
    esac; \
    curl -fsSL -o /tmp/firefox.tar.xz \
      "https://archive.mozilla.org/pub/firefox/releases/${FIREFOX_VERSION}/linux-${ffarch}/en-US/firefox-${FIREFOX_VERSION}.tar.xz"; \
    echo "${ffsum}  /tmp/firefox.tar.xz" | sha256sum -c -; \
    tar -xJf /tmp/firefox.tar.xz -C /opt; \
    ln -s /opt/firefox/firefox /usr/local/bin/firefox; \
    rm -f /tmp/firefox.tar.xz

# Optional VS Code (the room-desktop variant).
RUN --mount=type=cache,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,target=/var/lib/apt/lists,sharing=locked \
    set -eu; \
    [ -n "${VSCODE_VERSION}" ] || exit 0; \
    case "${TARGETARCH}" in amd64) arch=x64 ;; arm64) arch=arm64 ;; esac; \
    curl -fsSL -o /tmp/code.deb "https://update.code.visualstudio.com/${VSCODE_VERSION}/linux-deb-${arch}/stable"; \
    apt-get install -y --no-install-recommends /tmp/code.deb; \
    rm -f /tmp/code.deb /etc/apt/sources.list.d/vscode.*

# Node through nvm at /opt/nvm, owned by the agent user (uid 1000) so agents
# can `npm install -g` and `nvm install` without sudo. /opt/nvm/current is the
# default version and is on PATH, so node, claude and ore work without nvm.
# Agent CLIs are pinned; auto-update is off (DISABLE_AUTOUPDATER below).
ENV NVM_DIR=/opt/nvm
RUN set -eu; \
    mkdir -p "$NVM_DIR"; \
    for f in nvm.sh nvm-exec bash_completion; do \
      curl -fsSL -o "$NVM_DIR/$f" "https://raw.githubusercontent.com/nvm-sh/nvm/${NVM_VERSION}/$f"; \
    done; \
    chmod 0755 "$NVM_DIR/nvm-exec"; \
    export NODE_EXTRA_CA_CERTS=/etc/ssl/certs/ca-certificates.crt; \
    . "$NVM_DIR/nvm.sh"; \
    nvm install "${NODE_VERSION}"; \
    nvm alias default "${NODE_VERSION}"; \
    ln -sfn "versions/node/$(nvm version default)" "$NVM_DIR/current"; \
    npm install -g --no-fund --no-audit \
      "@anthropic-ai/claude-code@${CLAUDE_CODE_VERSION}" \
      "@ore-cli/ore@${ORE_VERSION}" \
      "pnpm@${PNPM_VERSION}"; \
    nvm cache clear; npm cache clean --force; rm -rf /root/.npm /tmp/*; \
    chown -R 1000:1000 "$NVM_DIR"
ENV PATH=/opt/nvm/current/bin:$PATH \
    DISABLE_AUTOUPDATER=1

COPY images/room/rootfs/ /
COPY --from=gobuild /out/supervisor /out/swarm /usr/local/bin/

# Podman runs as root inside the pod's user namespace (ADR 0001); the docker
# CLI finds it on the default /var/run/docker.sock, and the supervisor runs
# /usr/local/libexec/podman-ulimits before starting it. DOCKER_BUILDKIT=0:
# Podman's Docker API has no BuildKit, so `docker build` uses the classic
# builder. The build CA, if any, leaves the trust store here.
RUN set -eu; \
    chmod 0644 /etc/containers/storage.conf /etc/containers/containers.conf \
      /etc/containers/registries.conf.d/50-swarmageddon.conf /etc/ssh/ssh_config.d/50-swarmageddon.conf; \
    chmod 0755 /usr/local/libexec/podman-ulimits; \
    mkdir -p /var/lib/containers /run/swarm /etc/swarm; \
    rm -f /usr/local/share/ca-certificates/zz-build-ca.crt; \
    update-ca-certificates --fresh >/dev/null

ENV DISPLAY=:99 \
    DOCKER_HOST=unix:///var/run/docker.sock \
    DOCKER_BUILDKIT=0
STOPSIGNAL SIGTERM
ENTRYPOINT ["tini", "--", "/usr/local/bin/supervisor"]
