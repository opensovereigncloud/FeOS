NFPM_VERSION = $(shell cat hack/packages/nfpm.version)
NFPM_TARBALL = nfpm_$(NFPM_VERSION)_Linux_x86_64.tar.gz
NFPM = target/nfpm/nfpm
PACKAGES_DIR = target/packages
# The guest's version in a package name, which takes no ~.
GUEST_NAME = $(subst ~,-,$(GUEST_VERSION))

.PHONY: packages package-binaries

packages: firecracker guest package-binaries $(NFPM) ## Build the .deb packages into target/packages/ (ADR 0009).
	mkdir -p $(PACKAGES_DIR)
	for package in feosd feosctl feos-firecracker feos-firecracker-meta feos-guest feos-guest-meta; do \
		FEOS_VERSION=$(FEOS_VERSION) GUEST_VERSION=$(GUEST_VERSION) GUEST_NAME=$(GUEST_NAME) \
		FIRECRACKER_VERSION=$(FIRECRACKER_VERSION) \
		MUSL_RELEASE=target/$(MUSL_TARGET)/release \
		$(NFPM) package --config hack/packages/$$package.yaml \
			--packager deb --target $(PACKAGES_DIR)/ || exit 1; \
	done

# Per package, so that no binary carries another's features.
package-binaries:
	cargo build --release --features git-version --target=$(MUSL_TARGET) -p feos --bin feosd
	cargo build --release --target=$(MUSL_TARGET) -p feosctl

$(NFPM): hack/packages/nfpm.version hack/packages/nfpm.sha256
	mkdir -p $(dir $(NFPM))
	curl -fsSL -o "$(dir $(NFPM))$(NFPM_TARBALL)" \
		"https://github.com/goreleaser/nfpm/releases/download/v$(NFPM_VERSION)/$(NFPM_TARBALL)"
	echo "$$(cat hack/packages/nfpm.sha256)  $(dir $(NFPM))$(NFPM_TARBALL)" | sha256sum -c -
	tar -xzf "$(dir $(NFPM))$(NFPM_TARBALL)" -C $(dir $(NFPM)) nfpm
	rm -f "$(dir $(NFPM))$(NFPM_TARBALL)"
	touch $@

.PHONY: test-packages
test-packages: ## Install the packages on Debian 13 with systemd and check feosd; needs docker.
	hack/packages/test-packages

# A build of a commit as a version below the release it leads to:
# <FeOS version>~dev.<UTC time>.<commit>.
DEV_VERSION := $(FEOS_VERSION)~dev.$(shell date -u +%Y%m%d%H%M).$(shell git rev-parse --short=7 HEAD 2>/dev/null)

.PHONY: publish-apt
publish-apt: ## Build the packages of HEAD as a dev version and publish them to apt.fecode.dev.
	@git diff --quiet HEAD -- crates proto hack Cargo.toml Cargo.lock || \
		{ echo "publish-apt: commit what the packages hold first"; exit 1; }
	rm -f $(PACKAGES_DIR)/*.deb
	$(MAKE) packages FEOS_VERSION=$(DEV_VERSION)
	hack/packages/publish-apt $(PACKAGES_DIR)/*.deb
