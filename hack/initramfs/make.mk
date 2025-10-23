initramfs: target/cloud-hypervisor target/cloud-hypervisor-firmware target/youki container-release
	mkdir -p target/rootfs/bin
	mkdir -p target/rootfs/etc/feos
	mkdir -p target/rootfs/usr/share/cloud-hypervisor
	mkdir -p target/rootfs/usr/share/youki
	mkdir -p target/rootfs/usr/share/feos
	cp target/cloud-hypervisor/target/cloud-hypervisor-static target/rootfs/bin/cloud-hypervisor
	cp target/cloud-hypervisor/target/hypervisor-fw target/rootfs/usr/share/cloud-hypervisor
	cp target/youki/target/youki target/rootfs/bin/youki
	cp target/kernel/vmlinuz target/rootfs/usr/share/feos/vmlinuz
	cp target/x86_64-unknown-linux-musl/release/feos target/rootfs/bin/feos
	sudo chown -R `whoami` target/rootfs/etc/feos/
	cd target/rootfs && rm -f init && ln -s bin/feos init
	docker run --rm -u $${UID} -v "`pwd`:/feos" feos-builder bash -c "cd hack/initramfs && ./mk-initramfs"
