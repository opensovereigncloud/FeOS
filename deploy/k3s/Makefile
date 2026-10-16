# SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
# SPDX-License-Identifier: Apache-2.0

# Images and rollouts of the lab's k3s deployment (README.md). Run from the
# repository's root: make -C deploy/k3s <target>. Pushing needs
# `docker login oci.fecore.dev`.

REPO_ROOT := $(abspath ../..)
REGISTRY ?= oci.fecore.dev
DOCKER ?= docker
TAG ?= dev
PACKAGES_DIR ?= $(REPO_ROOT)/target/packages
CONTEXT := $(REPO_ROOT)/target/k3s/feos-node

.PHONY: feos-node push-feos-node deploy-feos

# The packages are make packages' output; their version is feosd's.
feos-node: ## Build the feos DaemonSet's image from target/packages.
	@ls $(PACKAGES_DIR)/feosd_*.deb >/dev/null 2>&1 || { echo "no packages; run make packages"; exit 1; }
	rm -rf $(CONTEXT) && mkdir -p $(CONTEXT)
	cp $(PACKAGES_DIR)/*.deb feos/Dockerfile feos/install $(CONTEXT)/
	dpkg-deb -f $$(ls $(PACKAGES_DIR)/feosd_*.deb | head -1) Version > $(CONTEXT)/version
	$(DOCKER) build -t $(REGISTRY)/feos-node:$(TAG) $(CONTEXT)

push-feos-node: feos-node ## Build and push the feos DaemonSet's image.
	$(DOCKER) push $(REGISTRY)/feos-node:$(TAG)

deploy-feos: push-feos-node ## Push and roll the feos DaemonSet, a node at a time.
	kubectl -n feos rollout restart daemonset/feos-node
	kubectl -n feos rollout status daemonset/feos-node --timeout=15m
