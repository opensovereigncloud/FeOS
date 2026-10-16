SHELL := /bin/bash

include hack/build-container/make.mk
include hack/kernel/make.mk
include hack/initramfs/make.mk
include hack/uki/make.mk
include hack/firecracker/make.mk
include hack/guest/make.mk
include hack/packages/make.mk
include hack/diskd/make.mk
include hack/go/make.mk

# The build container builds into target/container: build scripts that the
# host built would not run on the container's older glibc.
container-release:
#	make cargo index cache
	mkdir -p target/cargo
	docker run --rm -u $${UID} -v "`pwd`/target/cargo:/.cargo" -v "`pwd`:/feos" feos-builder bash -c "cd /feos && CARGO_HOME=/.cargo CARGO_TARGET_DIR=/feos/target/container make release"

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
	sudo ip link add name vm-br0 type bridge
	sudo ip link set up dev vm-br0
	sudo ip addr add fe80::1/64 dev vm-br0
	sudo ip addr add 169.254.42.1/24 dev vm-br0
