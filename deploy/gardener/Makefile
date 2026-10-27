# SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
# SPDX-License-Identifier: Apache-2.0

# Images and rollouts of the Gardener shoot's deployment (README.md). Run
# from the repository's root: make -C deploy/gardener <target>, with
# KUBECONFIG at the shoot. Pushing needs `docker login oci.fecode.dev`.
# ferund's image is the k3s lab's: make -C deploy/k3s push-ferund.

REPO_ROOT := $(abspath ../..)
REGISTRY ?= oci.fecode.dev
DOCKER ?= docker
TAG ?= dev
PACKAGES_DIR ?= $(REPO_ROOT)/target/packages
CONTEXT := $(REPO_ROOT)/target/gardener/feos-node

.PHONY: feos-node push-feos-node deploy-feos feos-k8s push-feos-k8s deploy-feos-k8s apply-ferun

# feosd, diskd, feosctl, Firecracker and the guest from the packages, for
# the feos-node pod (ADR 0020).
feos-node: ## Build the feos-node image from target/packages.
	@ls $(PACKAGES_DIR)/feosd_*.deb >/dev/null 2>&1 || { echo "no packages; run make packages"; exit 1; }
	rm -rf $(CONTEXT) && mkdir -p $(CONTEXT)
	cp $(PACKAGES_DIR)/*.deb feos/Dockerfile feos/run $(CONTEXT)/
	dpkg-deb -f $$(ls $(PACKAGES_DIR)/feosd_*.deb | head -1) Version > $(CONTEXT)/version
	$(DOCKER) build -t $(REGISTRY)/feos-node:$(TAG) $(CONTEXT)

push-feos-node: feos-node ## Build and push the feos-node image.
	$(DOCKER) push $(REGISTRY)/feos-node:$(TAG)

deploy-feos: push-feos-node ## Push and roll the feos-node DaemonSet, a node at a time.
	kubectl -n feos rollout restart daemonset/feos-node
	kubectl -n feos rollout status daemonset/feos-node --timeout=30m

feos-k8s: ## Build the feos-k8s image (ADR 0020).
	$(DOCKER) build -f feos-k8s/Dockerfile -t $(REGISTRY)/feos-k8s:$(TAG) $(REPO_ROOT)

push-feos-k8s: feos-k8s ## Build and push the feos-k8s image.
	$(DOCKER) push $(REGISTRY)/feos-k8s:$(TAG)

deploy-feos-k8s: push-feos-k8s ## Push and roll feos-k8s.
	kubectl apply -f feos/feos-k8s.yaml
	kubectl -n feos rollout restart deployment/feos-k8s
	kubectl -n feos rollout status deployment/feos-k8s --timeout=5m

apply-ferun: ## Apply ferund's manifests, the k3s ones with the shoot's address.
	kubectl kustomize --load-restrictor LoadRestrictionsNone ferun | kubectl apply -f -
