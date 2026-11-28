SHELL := /usr/bin/env bash

CLUSTER_NAME   ?= llm-platform
NAMESPACE      ?= llm-platform
IMAGE          ?= fakebackend:dev
OPERATOR_IMAGE ?= llm-platform-operator:dev
KUBE_CONTEXT   := kind-$(CLUSTER_NAME)
RUNS           ?= 5
RUN            ?= .

# Pinned tool versions. Bump them deliberately, together with go.mod.
CONTROLLER_RUNTIME_VERSION ?= v0.24.1
CONTROLLER_TOOLS_VERSION   ?= v0.22.0
ENVTEST_VERSION            ?= v0.24.1
# Empty means "latest available" for setup-envtest; set e.g. 1.35.0 to pin.
ENVTEST_K8S_VERSION        ?=

LOCALBIN        := $(CURDIR)/bin
CONTROLLER_GEN  := $(LOCALBIN)/controller-gen
SETUP_ENVTEST   := $(LOCALBIN)/setup-envtest
KIND_KUBECONFIG := $(LOCALBIN)/kind.kubeconfig
ENVTEST_ASSETS   = KUBEBUILDER_ASSETS="$$($(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION) -p path)"

.DEFAULT_GOAL := help
.PHONY: help deps build test test-unit test-run cover vet fmt fmt-check lint check generate manifests verify-generated \
	run-fake image cluster load-image deploy smoke undeploy install-crds uninstall-crds run-operator \
	operator-image operator-deploy operator-undeploy e2e measure-ready clean

help: ## Show this help
	@grep -E '^[a-zA-Z0-9_-]+:.*## ' $(MAKEFILE_LIST) | awk -F':.*## ' '{printf "  %-18s %s\n", $$1, $$2}'

deps: ## First-time M1 setup: add Kubernetes dependencies at pinned versions
	go get sigs.k8s.io/controller-runtime@$(CONTROLLER_RUNTIME_VERSION)
	go mod tidy

build: ## Build binaries into ./bin
	go build -o bin/fakebackend ./cmd/fakebackend
	go build -o bin/ttftprobe ./cmd/ttftprobe
	go build -o bin/operator ./cmd/operator

$(CONTROLLER_GEN):
	@mkdir -p $(LOCALBIN)
	GOBIN=$(LOCALBIN) go install sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_TOOLS_VERSION)

$(SETUP_ENVTEST):
	@mkdir -p $(LOCALBIN)
	GOBIN=$(LOCALBIN) go install sigs.k8s.io/controller-runtime/tools/setup-envtest@$(ENVTEST_VERSION)

generate: $(CONTROLLER_GEN) ## Regenerate deepcopy code from the API types
	$(CONTROLLER_GEN) object:headerFile=hack/boilerplate.go.txt paths=./api/...

manifests: $(CONTROLLER_GEN) ## Regenerate the CRD and RBAC from Go markers
	$(CONTROLLER_GEN) rbac:roleName=llm-platform-operator-role crd paths=./... output:crd:artifacts:config=config/crd/bases output:rbac:artifacts:config=config/rbac

verify-generated: generate manifests ## Fail if generated files are out of date (CI runs this)
	@git diff --exit-code -- api config/crd config/rbac || { echo "generated files are out of date: run 'make generate manifests' and commit"; exit 1; }

test: $(SETUP_ENVTEST) ## Run all tests, including envtest integration tests, with the race detector
	$(ENVTEST_ASSETS) go test -race -count=1 ./...

test-unit: ## Run only tests that need no cluster (integration tests are skipped)
	go test -race -count=1 ./...

test-run: $(SETUP_ENVTEST) ## Run matching tests verbosely: make test-run RUN=TestAdmission
	$(ENVTEST_ASSETS) go test -count=1 -v -run '$(RUN)' ./internal/controller

cover: $(SETUP_ENVTEST) ## Test with coverage and print the total
	$(ENVTEST_ASSETS) go test -race -count=1 -coverprofile=cover.out ./api/... ./internal/...
	go tool cover -func=cover.out | tail -n 1

vet: ## Run go vet
	go vet ./...

fmt: ## Format all Go files
	gofmt -w .

fmt-check: ## Fail if any Go file is not gofmt-formatted
	@test -z "$$(gofmt -l .)" || { echo "needs gofmt:"; gofmt -l .; exit 1; }

lint: ## Run golangci-lint (must be installed)
	golangci-lint run ./...

check: fmt-check vet test ## Everything CI runs except lint and docker

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

load-image: image ## Load the fake backend image into the kind cluster
	kind load docker-image $(IMAGE) --name $(CLUSTER_NAME)

deploy: load-image ## Deploy the standalone fake backend (M0) into the cluster
	kubectl --context $(KUBE_CONTEXT) apply -f deploy/fakebackend.yaml
	kubectl --context $(KUBE_CONTEXT) -n $(NAMESPACE) rollout restart deployment/fakebackend
	kubectl --context $(KUBE_CONTEXT) -n $(NAMESPACE) rollout status deployment/fakebackend --timeout=120s

smoke: ## Smoke-test the standalone fake backend through a port-forward (M0)
	CLUSTER_NAME=$(CLUSTER_NAME) NAMESPACE=$(NAMESPACE) ./hack/smoke.sh

undeploy: ## Remove the standalone fake backend (M0)
	kubectl --context $(KUBE_CONTEXT) delete -f deploy/fakebackend.yaml --ignore-not-found

install-crds: ## Install the InferenceService CRD into the kind cluster
	kubectl --context $(KUBE_CONTEXT) apply --server-side -f config/crd/bases

uninstall-crds: ## Remove the CRD (deletes every InferenceService!)
	kubectl --context $(KUBE_CONTEXT) delete -f config/crd/bases --ignore-not-found

run-operator: cluster install-crds ## Run the operator on this machine against the kind cluster
	@mkdir -p $(LOCALBIN)
	kind get kubeconfig --name $(CLUSTER_NAME) > $(KIND_KUBECONFIG)
	KUBECONFIG=$(KIND_KUBECONFIG) go run ./cmd/operator

operator-image: ## Build the operator container image
	docker build -f Dockerfile.operator -t $(OPERATOR_IMAGE) .

operator-deploy: operator-image install-crds ## Deploy the operator into the kind cluster
	kind load docker-image $(OPERATOR_IMAGE) --name $(CLUSTER_NAME)
	kubectl --context $(KUBE_CONTEXT) apply -f config/manager/namespace.yaml
	kubectl --context $(KUBE_CONTEXT) apply -f config/rbac
	kubectl --context $(KUBE_CONTEXT) apply -f config/manager/manager.yaml
	kubectl --context $(KUBE_CONTEXT) -n llm-platform-system rollout restart deployment/llm-platform-operator
	kubectl --context $(KUBE_CONTEXT) -n llm-platform-system rollout status deployment/llm-platform-operator --timeout=120s

operator-undeploy: ## Remove the in-cluster operator
	kubectl --context $(KUBE_CONTEXT) delete -f config/manager/manager.yaml --ignore-not-found
	kubectl --context $(KUBE_CONTEXT) delete -f config/rbac --ignore-not-found

e2e: ## End-to-end check against the kind cluster (operator must be running)
	CLUSTER_NAME=$(CLUSTER_NAME) ./hack/e2e-m1.sh

measure-ready: ## Time apply -> Ready on the kind cluster: make measure-ready RUNS=5
	CLUSTER_NAME=$(CLUSTER_NAME) ./hack/measure-ready.sh $(RUNS)

clean: ## Delete the kind cluster and local build output
	-kind delete cluster --name $(CLUSTER_NAME)
	rm -rf bin cover.out
