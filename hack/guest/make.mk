GUEST_KERNEL_VERSION = $(shell sed -n 's/^export KERNEL_VERSION=//p' hack/guest/kernel/config.sh)
# The guest's version, where it is installed: /usr/lib/feos/guest/<version>/.
GUEST_VERSION ?= $(FEOS_VERSION)

.PHONY: guest guest-kernel guest-kernel-config guest-rootfs test-guest test-guest-container test-vm

guest: guest-kernel guest-rootfs ## Build the guest kernel and initramfs into target/guest/.

guest-kernel: ## Build the guest kernel: target/guest/vmlinuz.
	hack/guest/kernel/mk-kernel

guest-kernel-config: ## Regenerate the guest kernel config from Firecracker's (after make guest-kernel).
	hack/guest/kernel/mk-config target/guest/kernel-cache/linux-$(GUEST_KERNEL_VERSION)

guest-rootfs: ## Build the guest initramfs: target/guest/initramfs.cpio.zst.
	hack/guest/rootfs/mk-rootfs

test-guest: firecracker ## Boot the guest under Firecracker and talk to it over vsock.
	hack/guest/test-vsock

test-guest-container: firecracker ## Run containers in the guest; needs sudo for a bridge, a TAP device and NAT.
	hack/guest/test-container

test-vm: firecracker ## Run the VM service against Firecracker, with the guest as VM image; needs docker.
	hack/guest/test-vm
