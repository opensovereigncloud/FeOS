initramfs: target/firecracker/firecracker container-release
	mkdir -p target/rootfs/bin
	mkdir -p target/rootfs/etc/feos
	mkdir -p target/rootfs/usr/share/feos
	cp target/firecracker/firecracker target/rootfs/bin/firecracker
	cp target/kernel/vmlinuz target/rootfs/usr/share/feos/vmlinuz
	cp target/x86_64-unknown-linux-musl/release/feos target/rootfs/bin/feos
	sudo chown -R `whoami` target/rootfs/etc/feos/
	cd target/rootfs && rm -f init && ln -s bin/feos init
	docker run --rm -u $${UID} -v "`pwd`:/feos" feos-builder bash -c "cd hack/initramfs && ./mk-initramfs"
