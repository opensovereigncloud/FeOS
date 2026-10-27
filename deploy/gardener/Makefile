# SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
# SPDX-License-Identifier: Apache-2.0

# Images, manifests and rollouts of the Gardener shoot's deployment
# (README.md). Run from the repository's root: make -C deploy/gardener
# <target>, with KUBECONFIG at the shoot. The parameters come from
# config.env (config.env.example). Pushing needs `docker login` at
# REGISTRY. ferund's image and FeChat's manifests are deploy/k3s's, built
# and filled in with this directory's parameters.

REPO_ROOT := $(abspath ../..)
-include config.env
REGISTRY ?= $(if $(DOMAIN),oci.$(DOMAIN))
export DOMAIN REGISTRY OIDC_ISSUER WORKER_POOL NODE_NAME_PREFIX FERUN_NODES_IP FECHAT_MODEL
DOCKER ?= docker
TAG ?= dev
PACKAGES_DIR ?= $(REPO_ROOT)/target/packages
CONTEXT := $(REPO_ROOT)/target/gardener/feos-node
MANIFESTS := $(REPO_ROOT)/target/gardener/manifests

# need(names): stops unless every variable named is set.
need = $(foreach v,$(1),$(if $($(v)),,$(error $(v) is not set: see config.env.example)))

# render(names, command): writes $@, the command's output with ${NAME} of
# the variables named filled in; each must be set, and none may be left.
define render
$(call need,$(1))mkdir -p $(@D)
$(2) | envsubst '$(foreach v,$(1),$${$(v)})' >$@.tmp
@if grep -n '\$${[A-Z_]*}' $@.tmp >&2; then echo "$@: a parameter is left" >&2; rm -f $@.tmp; exit 1; fi
mv $@.tmp $@
endef

# The files of a directory as one stream of documents, in this order.
cat-yaml = awk 'FNR==1{print "---"}1' $(1)

.PHONY: feos-node push-feos-node deploy-feos feos-k8s push-feos-k8s deploy-feos-k8s
.PHONY: ferund push-ferund deploy-ferund
.PHONY: manifests apply-feos apply-ferun apply-fechat apply-registry FORCE

# feosd, diskd, feosctl, Firecracker and the guest from the packages, for
# the feos-node pod (ADR 0020).
feos-node: ## Build the feos-node image from target/packages.
	$(call need,REGISTRY)
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
	$(call need,REGISTRY)
	$(DOCKER) build -f feos-k8s/Dockerfile -t $(REGISTRY)/feos-k8s:$(TAG) $(REPO_ROOT)

push-feos-k8s: feos-k8s ## Build and push the feos-k8s image.
	$(DOCKER) push $(REGISTRY)/feos-k8s:$(TAG)

deploy-feos-k8s: $(MANIFESTS)/feos-k8s.yaml push-feos-k8s ## Push, apply and roll feos-k8s.
	kubectl apply -f $(MANIFESTS)/feos-k8s.yaml
	kubectl -n feos rollout restart deployment/feos-k8s
	kubectl -n feos rollout status deployment/feos-k8s --timeout=5m

ferund push-ferund deploy-ferund: ## ferund's image and rollout, as deploy/k3s's.
	$(MAKE) -C ../k3s $@ REGISTRY='$(REGISTRY)' TAG='$(TAG)' DOCKER='$(DOCKER)'

manifests: $(MANIFESTS)/feos.yaml $(MANIFESTS)/feos-k8s.yaml $(MANIFESTS)/ferun.yaml $(MANIFESTS)/fechat.yaml $(MANIFESTS)/registry.yaml ## Write every part's manifests, with the parameters, to target/gardener/manifests.

$(MANIFESTS)/feos.yaml: FORCE
	$(call render,DOMAIN REGISTRY WORKER_POOL FERUN_NODES_IP,cat feos/feos.yaml)

$(MANIFESTS)/feos-k8s.yaml: FORCE
	$(call render,REGISTRY WORKER_POOL NODE_NAME_PREFIX,cat feos/feos-k8s.yaml)

# The k3s manifests live outside this directory.
$(MANIFESTS)/ferun.yaml: FORCE
	$(call render,DOMAIN REGISTRY OIDC_ISSUER FERUN_NODES_IP,kubectl kustomize --load-restrictor LoadRestrictionsNone ferun)

$(MANIFESTS)/fechat.yaml: FORCE
	$(call render,DOMAIN REGISTRY OIDC_ISSUER FECHAT_MODEL,$(call cat-yaml,$(addprefix ../k3s/fechat/,namespace.yaml config.yaml app.yaml backend.yaml ingress.yaml)))

$(MANIFESTS)/registry.yaml: FORCE
	$(call render,DOMAIN,cat registry/registry.yaml)

apply-feos: $(MANIFESTS)/feos.yaml ## Apply the feos-node DaemonSet and its configuration.
	kubectl apply -f $<

apply-ferun: $(MANIFESTS)/ferun.yaml ## Apply ferund's manifests, the k3s ones for the shoot.
	kubectl apply -f $<

apply-fechat: $(MANIFESTS)/fechat.yaml ## Apply FeChat, the k3s manifests.
	kubectl apply -f $<

apply-registry: $(MANIFESTS)/registry.yaml ## Apply the image registry.
	kubectl apply -f $<

FORCE:
