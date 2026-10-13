FIRECRACKER_VERSION = $(shell cat hack/firecracker/version)
FIRECRACKER_TARBALL = firecracker-v$(FIRECRACKER_VERSION)-x86_64.tgz
FIRECRACKER_DIR = target/firecracker

.PHONY: firecracker
firecracker: $(FIRECRACKER_DIR)/firecracker ## Download Firecracker into target/firecracker/.

$(FIRECRACKER_DIR)/firecracker: hack/firecracker/version hack/firecracker/sha256
	mkdir -p $(FIRECRACKER_DIR)
	curl -fsSL -o "$(FIRECRACKER_DIR)/$(FIRECRACKER_TARBALL)" \
		"https://github.com/firecracker-microvm/firecracker/releases/download/v$(FIRECRACKER_VERSION)/$(FIRECRACKER_TARBALL)"
	echo "$$(cat hack/firecracker/sha256)  $(FIRECRACKER_DIR)/$(FIRECRACKER_TARBALL)" | sha256sum -c -
	tar -xzf "$(FIRECRACKER_DIR)/$(FIRECRACKER_TARBALL)" -C $(FIRECRACKER_DIR) --strip-components=1
	for bin in firecracker jailer snapshot-editor rebase-snap; do \
		mv -f "$(FIRECRACKER_DIR)/$$bin-v$(FIRECRACKER_VERSION)-x86_64" "$(FIRECRACKER_DIR)/$$bin"; \
	done
	rm -f "$(FIRECRACKER_DIR)/$(FIRECRACKER_TARBALL)" $(FIRECRACKER_DIR)/*.debug
	touch $@
