SHELL := /usr/bin/env bash

CLUSTER_NAME ?= llm-platform
NAMESPACE    ?= llm-platform
IMAGE        ?= fakebackend:dev
KUBE_CONTEXT := kind-$(CLUSTER_NAME)

.DEFAULT_GOAL := help
.PHONY: help build test vet fmt fmt-check lint check run-fake image cluster load-image deploy smoke undeploy clean

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*## ' $(MAKEFILE_LIST) | awk -F':.*## ' '{printf "  %-12s %s\n", $$1, $$2}'

build: ## Build binaries into ./bin
	go build -o bin/fakebackend ./cmd/fakebackend
	go build -o bin/ttftprobe ./cmd/ttftprobe

test: ## Run unit tests with the race detector
	go test -race -count=1 ./...

vet: ## Run go vet
	go vet ./...

fmt: ## Format all Go files
	gofmt -w .

fmt-check: ## Fail if any Go file is not gofmt-formatted
	@test -z "$$(gofmt -l .)" || { echo "needs gofmt:"; gofmt -l .; exit 1; }

lint: ## Run golangci-lint (must be installed)
	golangci-lint run ./...

check: fmt-check vet test ## Everything CI runs, minus lint and docker

run-fake: ## Run the fake backend locally on :8000
	go run ./cmd/fakebackend

image: ## Build the fake backend container image
	docker build -t $(IMAGE) .

cluster: ## Create the kind cluster (no-op if it exists)
	@if kind get clusters 2>/dev/null | grep -qx "$(CLUSTER_NAME)"; then \
		echo "cluster $(CLUSTER_NAME) already exists"; \
	else \
		kind create cluster --name $(CLUSTER_NAME) --config hack/kind-config.yaml --wait 120s; \
	fi

load-image: image ## Load the image into the kind cluster
	kind load docker-image $(IMAGE) --name $(CLUSTER_NAME)

deploy: load-image ## Deploy the fake backend into the cluster
	kubectl --context $(KUBE_CONTEXT) apply -f deploy/fakebackend.yaml
	kubectl --context $(KUBE_CONTEXT) -n $(NAMESPACE) rollout restart deployment/fakebackend
	kubectl --context $(KUBE_CONTEXT) -n $(NAMESPACE) rollout status deployment/fakebackend --timeout=120s

smoke: ## Smoke-test the deployed backend through a port-forward
	CLUSTER_NAME=$(CLUSTER_NAME) NAMESPACE=$(NAMESPACE) ./hack/smoke.sh

undeploy: ## Remove the fake backend from the cluster
	kubectl --context $(KUBE_CONTEXT) delete -f deploy/fakebackend.yaml --ignore-not-found

clean: ## Delete the kind cluster and local build output
	-kind delete cluster --name $(CLUSTER_NAME)
	rm -rf bin
