# swarmageddon: see docs/design.md §13.
SHELL := /bin/bash
export GOTOOLCHAIN ?= go1.26.9
GOBIN := $(shell go env GOPATH)/bin
export PATH := $(GOBIN):$(PATH)
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
REGISTRY ?= ghcr.io/usr-lex/swarmageddon
BINARIES := operator control-center messaging supervisor swarm mockmodel

.PHONY: all generate build test lint
all: generate build

## generate: protobuf, deepcopy and CRDs
generate:
	buf generate
	controller-gen object paths=./api/...
	controller-gen crd paths=./api/... output:crd:artifacts:config=config/crd

## build: every Go binary into bin/
build:
	@mkdir -p bin
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X github.com/usr-lex/swarmageddon/internal/version.Version=$(VERSION)" -o bin/ $(addprefix ./cmd/,$(BINARIES))

## test: unit and envtest
test:
	go test ./...

lint:
	go vet ./...
	buf lint
