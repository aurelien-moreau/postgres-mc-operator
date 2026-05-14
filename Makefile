BINARY_NAME     := postgres-mc-operator
IMG             ?= $(BINARY_NAME):latest
CONTROLLER_GEN  := controller-gen
GO              := go

.PHONY: all build test generate manifests lint vet fmt run docker-build install uninstall deploy

all: generate manifests build

## Build

build: generate
	$(GO) build -o bin/$(BINARY_NAME) ./cmd/

run: generate
	$(GO) run ./cmd/main.go

## Test

test: generate
	$(GO) test ./... -coverprofile=coverage.out
	$(GO) tool cover -func=coverage.out

## Code generation

generate:
	$(CONTROLLER_GEN) object:headerFile="hack/boilerplate.go.txt" paths="./..."

manifests: generate
	$(CONTROLLER_GEN) rbac:roleName=$(BINARY_NAME)-role crd paths="./..." \
		output:crd:artifacts:config=config/crd/bases \
		output:rbac:artifacts:config=config/rbac

## Code quality

fmt:
	$(GO) fmt ./...

vet:
	$(GO) vet ./...

lint:
	golangci-lint run ./...

## Docker

docker-build:
	docker build -t $(IMG) .

docker-push:
	docker push $(IMG)

## CRD install / uninstall (requires kubectl and a running cluster)

install: manifests
	kubectl apply -f config/crd/bases/

uninstall:
	kubectl delete -f config/crd/bases/ --ignore-not-found

## Deploy

deploy: manifests
	kubectl apply -f config/crd/bases/
	kubectl apply -f config/rbac/role.yaml

.PHONY: help
help:
	@grep -E '^[a-zA-Z_-]+:.*?##' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-20s\033[0m %s\n", $$1, $$2}'
