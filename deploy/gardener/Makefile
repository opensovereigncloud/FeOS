# SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
# SPDX-License-Identifier: Apache-2.0

# Images and rollouts of the Gardener shoot's deployment (README.md). Run
# from the repository's root: make -C deploy/gardener <target>, with
# KUBECONFIG at the shoot. Pushing needs `docker login oci.fecore.dev`.
# ferund's image is the k3s lab's: make -C deploy/k3s push-ferund.

REPO_ROOT := $(abspath ../..)
REGISTRY ?= oci.fecore.dev
DOCKER ?= docker
TAG ?= dev
PACKAGES_DIR ?= $(REPO_ROOT)/target/packages
CONTEXT := $(REPO_ROOT)/target/gardener/feos-node

.PHONY: feos-node push-feos-node deploy-feos apply-ferun

# The k3s image's Dockerfile, with this directory's install script: the
# same packages, laid over /usr as a system extension instead of apt's.
feos-node: ## Build the feos DaemonSet's image from target/packages.
	@ls $(PACKAGES_DIR)/feosd_*.deb >/dev/null 2>&1 || { echo "no packages; run make packages"; exit 1; }
	rm -rf $(CONTEXT) && mkdir -p $(CONTEXT)
	cp $(PACKAGES_DIR)/*.deb ../k3s/feos/Dockerfile feos/install $(CONTEXT)/
	dpkg-deb -f $$(ls $(PACKAGES_DIR)/feosd_*.deb | head -1) Version > $(CONTEXT)/version
	$(DOCKER) build -t $(REGISTRY)/feos-node-gardenlinux:$(TAG) $(CONTEXT)

push-feos-node: feos-node ## Build and push the feos DaemonSet's image.
	$(DOCKER) push $(REGISTRY)/feos-node-gardenlinux:$(TAG)

deploy-feos: push-feos-node ## Push and roll the feos DaemonSet, a node at a time.
	kubectl -n feos rollout restart daemonset/feos-node
	kubectl -n feos rollout status daemonset/feos-node --timeout=15m

apply-ferun: ## Apply ferund's manifests, the k3s ones with the shoot's address.
	kubectl kustomize --load-restrictor LoadRestrictionsNone ferun | kubectl apply -f -
