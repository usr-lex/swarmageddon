# swarmageddon: see docs/design.md §13.
SHELL := /bin/bash
export GOTOOLCHAIN ?= go1.26.9
GOBIN := $(shell go env GOPATH)/bin
export PATH := $(GOBIN):$(PATH)
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
REGISTRY ?= ghcr.io/usr-lex/swarmageddon
BINARIES := operator control-center messaging supervisor swarm mockmodel

.PHONY: all generate manifests envtest build test lint
all: generate build

## generate: protobuf, deepcopy and CRDs
generate:
	buf generate
	controller-gen object paths=./api/...
	controller-gen crd paths=./api/... output:crd:artifacts:config=config/crd
	$(MAKE) -s chart-crds

## manifests: the operator's ClusterRole from the +kubebuilder:rbac markers
manifests:
	controller-gen rbac:roleName=swarmageddon-operator paths=./internal/operator/... output:rbac:artifacts:config=config/rbac

## envtest: download the API server and etcd for controller tests
ENVTEST_K8S ?= 1.36.x
ENVTEST_DIR ?= $(CURDIR)/bin/envtest
envtest:
	@go run sigs.k8s.io/controller-runtime/tools/setup-envtest@latest use $(ENVTEST_K8S) --bin-dir $(ENVTEST_DIR) -p path

## build: every Go binary into bin/
build:
	@mkdir -p bin
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X github.com/usr-lex/swarmageddon/internal/version.Version=$(VERSION)" -o bin/ $(addprefix ./cmd/,$(BINARIES))

## test: unit and envtest
test:
	KUBEBUILDER_ASSETS="$${KUBEBUILDER_ASSETS:-$$($(MAKE) -s envtest)}" go test ./...

lint:
	go vet ./...
	buf lint

## ---- images, chart, install.yaml, dev cluster (docs/design.md §13) ----

TOOLS := $(CURDIR)/bin/tools
HELM_VERSION ?= v3.22.1
K3D_VERSION ?= v5.8.3
KUBECTL_VERSION ?= v1.36.2
HELM := $(TOOLS)/helm
K3D := $(TOOLS)/k3d
KUBECTL := $(TOOLS)/kubectl
HOST_ARCH := $(shell go env GOARCH)

# Images are $(REGISTRY)/<name>:$(VERSION). Locally: the host's platform,
# loaded into docker. CI: PLATFORMS=linux/amd64,linux/arm64 IMAGE_OUTPUT=--push.
SERVICE_IMAGES := operator control-center messaging mockmodel
IMAGES := room runner $(SERVICE_IMAGES)
PLATFORMS ?= linux/$(HOST_ARCH)
IMAGE_OUTPUT ?= --load
# Behind a TLS-intercepting proxy, set BUILD_CA to its CA bundle: builds then
# use the host network and $HTTPS_PROXY, and trust that CA while building only.
BUILD_CA ?=
BUILDX_FLAGS ?=
ifneq ($(BUILD_CA),)
BUILDX_FLAGS += --network host --secret id=build_ca,src=$(BUILD_CA) \
	--build-arg HTTPS_PROXY --build-arg https_proxy --build-arg NO_PROXY --build-arg no_proxy
endif
BUILDX = docker buildx build --platform $(PLATFORMS) $(IMAGE_OUTPUT) $(BUILDX_FLAGS) \
	--build-arg VERSION=$(VERSION)

.PHONY: images image-room image-runner $(addprefix image-,$(SERVICE_IMAGES))
## images: every image, tagged $(REGISTRY)/<name>:$(VERSION)
images: $(addprefix image-,$(IMAGES))

image-room:
	$(BUILDX) -f images/room/Dockerfile -t $(REGISTRY)/room:$(VERSION) .

image-runner: image-room
	$(BUILDX) -f images/runner/Dockerfile --build-arg ROOM_IMAGE=$(REGISTRY)/room:$(VERSION) \
		-t $(REGISTRY)/runner:$(VERSION) .

# A component's own cmd/<name>/Dockerfile wins; otherwise the shared one.
$(addprefix image-,$(SERVICE_IMAGES)): image-%:
	$(BUILDX) -t $(REGISTRY)/$*:$(VERSION) --build-arg BINARY=$* \
		-f $(if $(wildcard cmd/$*/Dockerfile),cmd/$*/Dockerfile,images/service/Dockerfile) .

## tools: helm, k3d and kubectl into bin/tools
.PHONY: tools
tools: $(HELM) $(K3D) $(KUBECTL)

# get.helm.sh isn't reachable everywhere; the Go module proxy is.
$(HELM):
	@mkdir -p $(TOOLS)
	GOBIN=$(TOOLS) go install helm.sh/helm/v3/cmd/helm@$(HELM_VERSION)

$(K3D):
	@mkdir -p $(TOOLS)
	curl -fsSL -o $@ https://github.com/k3d-io/k3d/releases/download/$(K3D_VERSION)/k3d-linux-$(HOST_ARCH)
	chmod +x $@

$(KUBECTL):
	@mkdir -p $(TOOLS)
	curl -fsSL -o $@ https://dl.k8s.io/release/$(KUBECTL_VERSION)/bin/linux/$(HOST_ARCH)/kubectl
	chmod +x $@

CHART := charts/swarmageddon
CHART_VERSION ?= $(shell sed -n 's/^version: //p' $(CHART)/Chart.yaml)
CHART_APP_VERSION ?= $(shell sed -n 's/^appVersion: "\(.*\)"/\1/p' $(CHART)/Chart.yaml)

.PHONY: chart-crds chart-lint chart install-yaml install-yaml-check
## chart-crds: copy the generated CRDs into the chart
chart-crds:
	@mkdir -p $(CHART)/crds
	rm -f $(CHART)/crds/*.yaml
	cp config/crd/*.yaml $(CHART)/crds/

chart-lint: $(HELM) chart-crds
	$(HELM) lint --kube-version 1.36.0 $(CHART)

## chart: package the chart into dist/
chart: $(HELM) chart-crds
	@mkdir -p dist
	$(HELM) package $(CHART) -d dist --version $(CHART_VERSION) --app-version $(CHART_APP_VERSION)

## install-yaml: render deploy/install.yaml from the chart, with the Namespace
install-yaml: $(HELM) chart-crds
	@mkdir -p deploy
	{ echo "# swarmageddon $(CHART_APP_VERSION): rendered from charts/swarmageddon by 'make install-yaml'. Do not edit."; \
	  $(HELM) template swarmageddon $(CHART) --namespace swarmageddon-system \
		--kube-version 1.36.0 --include-crds --set namespace.create=true \
		--set image.tag=$(CHART_APP_VERSION); } > deploy/install.yaml

## install-yaml-check: fail if the committed install.yaml or chart CRDs are stale
install-yaml-check: install-yaml
	git diff --exit-code -- deploy/install.yaml $(CHART)/crds

## dev: k3d cluster with a local registry, every image, the chart with dev values
.PHONY: dev dev-down dev-images
dev: tools
	K3D=$(K3D) KUBECTL=$(KUBECTL) HELM=$(HELM) hack/dev/up.sh

DEV_REGISTRY ?= localhost:5005
# Leave out room and runner (large) with DEV_IMAGES="operator control-center messaging mockmodel".
DEV_IMAGES ?= $(IMAGES)
# Called by hack/dev/up.sh: build every image and push it to the dev registry.
dev-images:
	$(MAKE) $(addprefix image-,$(DEV_IMAGES)) REGISTRY=$(DEV_REGISTRY) VERSION=dev PLATFORMS=linux/$(HOST_ARCH) IMAGE_OUTPUT=--load
	for i in $(DEV_IMAGES); do docker push $(DEV_REGISTRY)/$$i:dev; done

dev-down: $(K3D)
	K3D=$(K3D) hack/dev/down.sh

## e2e: the k3d end-to-end suite. canary: CLI protocol checks.
.PHONY: e2e canary
e2e:
	go test -tags e2e -count=1 -timeout 60m ./test/e2e/...

canary:
	go test -tags canary -count=1 -timeout 30m ./test/canary/...
