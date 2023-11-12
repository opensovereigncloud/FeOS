SHELL := /bin/bash

empty:

build-container:
	cd hack/build-container && ./mk-build-container
	mkdir -p target
	touch hack/build-container

container-release:
	docker run -it --rm -u $${UID} -v "`pwd`:/feos" aurae-builder bash -c "cd /feos && make release"

kernel:
	mkdir -p target/rootfs/boot
	docker run -it --rm -u $${UID} -v "`pwd`:/feos" feos-builder bash -c "cd hack/kernel && ./mk-kernel"

menuconfig:
	docker run -it --rm -u $${UID} -v "`pwd`:/feos" feos-builder bash -c "cd hack/kernel && ./mk-menuconfig"

initramfs: container-release
	mkdir -p target/rootfs/bin
	mkdir -p target/rootfs/etc/feos
	cp target/release/feos target/rootfs/bin/feos
	sudo chown -R `whoami` target/rootfs/etc/feos/
	cd target/rootfs && rm -f init && ln -s bin/feos init
	docker run -it --rm -u $${UID} -v "`pwd`:/feos" feos-builder bash -c "cd hack/initramfs && ./mk-initramfs"

virsh-start:
	./hack/libvirt/init.sh libvirt-kvm.xml
	virsh --connect qemu:///system create target/libvirt.xml

virsh-stop:
	virsh --connect qemu:///system destroy feos

virsh-console:
	virsh --connect qemu:///system console feos

virsh-shutdown:
	virsh --connect qemu:///system shutdown feos --mode acpi

network:
	sudo brctl addbr vm-br0
	sudo ip link set up dev vm-br0
	sudo ip addr add fe80::1/64 dev vm-br0
	sudo ip addr add 169.254.42.1/24 dev vm-br0