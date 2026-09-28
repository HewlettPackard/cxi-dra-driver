IMAGE ?= ghcr.io/slingshot/cxi-dra-driver
TAG   ?= latest
KIND_CLUSTER ?= cxi-dra-dev
KIND_IMAGE   ?= localhost/cxi-dra-driver:dev
CONTAINER_CLI ?= $(shell command -v docker 2>/dev/null || command -v podman-hpc)

BINARY      = cxi-dra-driver
CMD_PATH    = ./cmd/cxi-dra-driver
HOOK_BINARY = cxi-cdi-hook
HOOK_CMD_PATH = ./cmd/cxi-cdi-hook
CNI_BINARY  = slingshot-cni
CNI_CMD_PATH = ./cmd/slingshot-cni

.PHONY: build build-hook build-cni image push deploy clean kind-load kind-reload

build: build-hook
	CGO_ENABLED=1 GOOS=linux go build -tags cxi -o bin/$(BINARY) $(CMD_PATH)

build-mock: build-hook
	CGO_ENABLED=0 GOOS=linux go build -o bin/$(BINARY) $(CMD_PATH)

# cxi-cdi-hook runs on the bare host (see internal/cdi.InstallHook), so it must
# stay CGO-free regardless of how the driver binary itself is built.
build-hook:
	CGO_ENABLED=0 GOOS=linux go build -o bin/$(HOOK_BINARY) $(HOOK_CMD_PATH)

# slingshot-cni is invoked directly by containerd/Multus on the host, so it
# must also stay CGO-free; it ships independently of the driver DaemonSet
# image built by `image` below.
build-cni:
	CGO_ENABLED=0 GOOS=linux go build -o bin/$(CNI_BINARY) $(CNI_CMD_PATH)

image:
	$(CONTAINER_CLI) build --no-cache -t $(IMAGE):$(TAG) .

push: image
	$(CONTAINER_CLI) push $(IMAGE):$(TAG)

tidy:
	go mod tidy

vet:
	go vet ./...

# kind-load: build dev image and load it into the kind cluster.
# Removes the old image from the node's containerd first to prevent dedup no-ops.
kind-load:
	$(CONTAINER_CLI) build --no-cache -t $(KIND_IMAGE) .
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
