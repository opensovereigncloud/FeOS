initramfs: target/firecracker/firecracker container-release
	mkdir -p target/rootfs/bin
	mkdir -p target/rootfs/usr/sbin
	mkdir -p target/rootfs/etc/feos
	mkdir -p target/rootfs/usr/share/feos
	cp target/firecracker/firecracker target/firecracker/jailer target/rootfs/bin/
	cp target/kernel/vmlinuz target/rootfs/usr/share/feos/vmlinuz
	rm -f target/rootfs/bin/feos
	cp target/x86_64-unknown-linux-musl/release/feos-host-init target/rootfs/usr/sbin/feos-host-init
	cp target/x86_64-unknown-linux-musl/release/feosd target/rootfs/usr/sbin/feosd
	sudo chown -R `whoami` target/rootfs/etc/feos/
	cd target/rootfs && rm -f init && ln -s usr/sbin/feos-host-init init
	docker run --rm -u $${UID} -v "`pwd`:/feos" feos-builder bash -c "cd hack/initramfs && ./mk-initramfs"
