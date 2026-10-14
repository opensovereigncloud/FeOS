# FeOS as the node's operating system (ADR 0009): feos-host-init as /init,
# feosd, and Firecracker and the guest where their packages put them.
initramfs: target/firecracker/firecracker guest container-release
	mkdir -p target/rootfs/usr/sbin
	mkdir -p target/rootfs/etc/feos
	mkdir -p target/rootfs/usr/lib/feos/firecracker/$(FIRECRACKER_VERSION)
	mkdir -p target/rootfs/usr/lib/feos/guest/$(GUEST_VERSION)
	cp target/firecracker/firecracker target/firecracker/jailer target/rootfs/usr/lib/feos/firecracker/$(FIRECRACKER_VERSION)/
	cp target/guest/vmlinuz target/rootfs/usr/lib/feos/guest/$(GUEST_VERSION)/vmlinuz
	cp target/guest/initramfs.cpio.zst target/rootfs/usr/lib/feos/guest/$(GUEST_VERSION)/initramfs
	rm -f target/rootfs/bin/feos target/rootfs/bin/firecracker target/rootfs/bin/jailer
	rm -f target/rootfs/usr/share/feos/vmlinuz target/rootfs/usr/share/feos/initramfs
	cp target/container/x86_64-unknown-linux-musl/release/feos-host-init target/rootfs/usr/sbin/feos-host-init
	cp target/container/x86_64-unknown-linux-musl/release/feosd target/rootfs/usr/sbin/feosd
	sudo chown -R `whoami` target/rootfs/etc/feos/
	cd target/rootfs && rm -f init && ln -s usr/sbin/feos-host-init init
	docker run --rm -u $${UID} -v "`pwd`:/feos" feos-builder bash -c "cd hack/initramfs && ./mk-initramfs"
