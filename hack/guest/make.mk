GUEST_KERNEL_VERSION = $(shell sed -n 's/^export KERNEL_VERSION=//p' hack/guest/kernel/config.sh)

.PHONY: guest guest-kernel guest-kernel-config guest-rootfs test-guest

guest: guest-kernel guest-rootfs ## Build the guest kernel and initramfs into target/guest/.

guest-kernel: ## Build the guest kernel: target/guest/vmlinuz.
	hack/guest/kernel/mk-kernel

guest-kernel-config: ## Regenerate the guest kernel config from Firecracker's (after make guest-kernel).
	hack/guest/kernel/mk-config target/guest/kernel-cache/linux-$(GUEST_KERNEL_VERSION)

guest-rootfs: ## Build the guest initramfs: target/guest/initramfs.cpio.zst.
	hack/guest/rootfs/mk-rootfs

test-guest: firecracker ## Boot the guest under Firecracker and talk to it over vsock.
	hack/guest/test-vsock
