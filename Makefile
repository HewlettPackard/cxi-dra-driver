IMAGE ?= ghcr.io/hewlettpackard/cxi-dra-driver
TAG   ?= latest
KIND_CLUSTER ?= cxi-dra-dev
KIND_IMAGE   ?= localhost/cxi-dra-driver:dev
CONTAINER_CLI ?= $(shell command -v docker 2>/dev/null || command -v podman-hpc 2>/dev/null || command -v podman)

# BUILD_ARGS is passed through to the container build. Use it to redirect the
# base images at an internal mirror on air-gapped sites, or on a machine
# without Docker Hub credentials:
#   make image BUILD_ARGS='--build-arg GO_IMAGE=mirror.gcr.io/library/golang:1.24-bookworm \
#                          --build-arg RUNTIME_IMAGE=mirror.gcr.io/library/debian:bookworm-slim'
BUILD_ARGS ?=

BINARY      = cxi-dra-driver
CMD_PATH    = ./cmd/cxi-dra-driver
HOOK_BINARY = cxi-cdi-hook
HOOK_CMD_PATH = ./cmd/cxi-cdi-hook
CNI_BINARY  = slingshot-cni
CNI_CMD_PATH = ./cmd/slingshot-cni
# Target architecture for the CGO-free host binaries. Defaults to the build
# host's arch; set GOARCH=amd64 when building on an arm64 workstation for
# x86-64 nodes (or vice versa).
GOARCH ?= $(shell go env GOARCH)

.PHONY: build build-mock build-hook build-cni image image-mock push deploy clean kind-load kind-reload lint

# -buildvcs=false: the production binary is often built as root on a Slingshot
# node from a tree owned by a different user, which makes git refuse VCS queries
# ("dubious ownership", exit 128) and aborts the build. We don't need a VCS
# stamp in the binary, so skip it.
build: build-hook
	CGO_ENABLED=1 GOOS=linux go build -tags cxi -buildvcs=false -o bin/$(BINARY) $(CMD_PATH)

build-mock: build-hook
	CGO_ENABLED=0 GOOS=linux go build -buildvcs=false -o bin/$(BINARY) $(CMD_PATH)

# cxi-cdi-hook runs on the bare host (see internal/cdi.InstallHook), so it must
# stay CGO-free regardless of how the driver binary itself is built.
build-hook:
	CGO_ENABLED=0 GOOS=linux go build -buildvcs=false -o bin/$(HOOK_BINARY) $(HOOK_CMD_PATH)

# slingshot-cni is invoked directly by containerd/Multus on the host, so it
# must also stay CGO-free. The image build compiles it itself (see Dockerfile);
# this target is for building it standalone.
build-cni:
	CGO_ENABLED=0 GOOS=linux GOARCH=$(GOARCH) go build -buildvcs=false -o bin/$(CNI_BINARY) $(CNI_CMD_PATH)

# image: production image. Builds libcxi, the driver, cxi-cdi-hook and
# slingshot-cni inside the container, so it needs no host libraries or staged
# bin/ artifacts and works on any machine and either architecture. Layer caching
# is safe here, so --no-cache is deliberately not used.
image:
	$(CONTAINER_CLI) build $(BUILD_ARGS) -t $(IMAGE):$(TAG) -f Dockerfile .

# image-mock: no-hardware image for kind. See Dockerfile.mock.
image-mock:
	$(CONTAINER_CLI) build $(BUILD_ARGS) -t $(IMAGE):$(TAG)-mock -f Dockerfile.mock .

push: image
	$(CONTAINER_CLI) push $(IMAGE):$(TAG)

tidy:
	go mod tidy

vet:
	go vet ./...

# Pinned so CI (.github/workflows/build.yaml and pr-checks.yaml) and local runs
# use the same linter.
GOLANGCI_LINT_VERSION ?= v2.15.0

# golangci-lint needs a newer Go than go.mod asks for, so the toolchain download
# is allowed explicitly: actions/setup-go v6+ and the golang images export
# GOTOOLCHAIN=local. The linter is built for the host (GOOS/GOARCH cleared) but
# always analyses the Linux build, the only one the driver ships; elsewhere the
# *_linux.go files (the real netlink path among them) would go unlinted.
lint:
	GOOS= GOARCH= GOTOOLCHAIN=auto go run -exec 'env GOOS=linux' \
		github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION) run ./...

# kind-load: build dev image and load it into the kind cluster.
# Uses the mock Dockerfile: kind nodes have no CXI hardware, so the driver runs
# with --mock-devices and never reaches libcxi.
# Removes the old image from the node's containerd first to prevent dedup no-ops.
kind-load:
	$(CONTAINER_CLI) build --no-cache $(BUILD_ARGS) -t $(KIND_IMAGE) -f Dockerfile.mock .
	-$(CONTAINER_CLI) exec $(KIND_CLUSTER)-worker ctr -n k8s.io images rm $(KIND_IMAGE) 2>/dev/null
	$(CONTAINER_CLI) save $(KIND_IMAGE) | $(CONTAINER_CLI) exec -i $(KIND_CLUSTER)-worker ctr -n k8s.io images import -

# kind-reload: load new image and restart the DaemonSet.
kind-reload: kind-load
	kubectl -n kube-system rollout restart daemonset/cxi-dra-driver
	kubectl -n kube-system rollout status daemonset/cxi-dra-driver --timeout=90s

deploy:
	kubectl apply -f deploy/kubernetes/clusterrole.yaml
	kubectl apply -f deploy/kubernetes/deviceclass.yaml
	kubectl apply -f deploy/kubernetes/admission-policy.yaml
	kubectl apply -f deploy/kubernetes/daemonset.yaml

undeploy:
	kubectl delete -f deploy/kubernetes/daemonset.yaml --ignore-not-found
	kubectl delete -f deploy/kubernetes/admission-policy.yaml --ignore-not-found
	kubectl delete -f deploy/kubernetes/deviceclass.yaml --ignore-not-found
	kubectl delete -f deploy/kubernetes/clusterrole.yaml --ignore-not-found

test-deploy:
	kubectl apply -f deploy/kubernetes/test/resourceclaim.yaml
	kubectl apply -f deploy/kubernetes/test/pod.yaml

test-clean:
	kubectl delete -f deploy/kubernetes/test/pod.yaml --ignore-not-found
	kubectl delete -f deploy/kubernetes/test/resourceclaim.yaml --ignore-not-found

clean:
	rm -f bin/$(BINARY) bin/$(HOOK_BINARY) bin/$(CNI_BINARY)
