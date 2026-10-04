# Copyright 2026 Raghav Gade
# SPDX-License-Identifier: Apache-2.0
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
IMG ?= ghcr.io/gade-raghav/apva:$(VERSION)
PROMETHEUS_URL ?= http://localhost:9090

.PHONY: all build test lint run demo demo-cluster demo-down image e2e e2e-aws clean
all: lint test build

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "-X main.version=$(VERSION)" -o bin/apva ./cmd/apva

test:
	go test -race -cover ./...

lint:
	@test -z "$$(gofmt -l .)" || (echo "gofmt needed:"; gofmt -l .; exit 1)
	go vet ./...

run: build
	./bin/apva --prometheus-url=$(PROMETHEUS_URL)

demo: build
	./bin/apva --demo

demo-cluster:
	./test/demo/up.sh

demo-down:
	./test/demo/down.sh

image:
	docker build --build-arg VERSION=$(VERSION) -t $(IMG) .

e2e:
	./test/e2e/run.sh

# Needs a cluster in $KUBECONFIG with KWOK running; see .github/workflows/e2e-aws.yml.
e2e-aws:
	./test/e2e-aws/run.sh

clean:
	rm -rf bin dist
