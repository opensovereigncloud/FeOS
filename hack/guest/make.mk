GUEST_KERNEL_VERSION = $(shell sed -n 's/^export KERNEL_VERSION=//p' hack/guest/kernel/config.sh)
# The guest's version, where it is installed: /usr/lib/feos/guest/<version>/.
GUEST_VERSION ?= $(FEOS_VERSION)

.PHONY: guest guest-kernel guest-kernel-config guest-rootfs guest-data test-guest test-guest-container test-pods

guest: guest-kernel guest-rootfs guest-data ## Build the guest kernel, initramfs and blank data volume into target/guest/.

guest-kernel: ## Build the guest kernel: target/guest/vmlinuz.
	hack/guest/kernel/mk-kernel

guest-kernel-config: ## Regenerate the guest kernel config from Firecracker's (after make guest-kernel).
	hack/guest/kernel/mk-config target/guest/kernel-cache/linux-$(GUEST_KERNEL_VERSION)

guest-rootfs: ## Build the guest initramfs: target/guest/initramfs.cpio.zst.
	hack/guest/rootfs/mk-rootfs

# The size of the pods' disks: every pod's starts from this volume.
GUEST_DATA_SIZE ?= 8G

guest-data: ## Build the guest's blank data volume, pods' disks start from: target/guest/data.img.zst.
	hack/guest/data/mk-data $(GUEST_DATA_SIZE)

test-guest: firecracker ## Boot the guest under Firecracker and talk to it over vsock.
	hack/guest/test-vsock

test-guest-container: firecracker ## Run containers in the guest; needs sudo for a network namespace and NAT.
	hack/guest/test-container

test-pods: firecracker ## Run pods against Firecracker, with the guest installed; needs docker.
	hack/guest/test-pods
