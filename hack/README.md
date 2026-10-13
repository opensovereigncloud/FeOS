FeOS VM hacking tools
=======================

This directory includes scripts to run FeOS as the pid 1 process within a VM.

    make build-container
    make kernel
    make initramfs
    make uki

    # create `vm-br0` bridge on your machine:
    make network

    # run FeOS in a VM as pid 1:
    make virsh-start virsh-console virsh-stop

    # exit VM console with Ctrl+]


FeOS is a statically linked musl binary, so the initramfs needs no shared libraries. To get a consistent result across different build machines, the kernel, FeOS and the initramfs are built in a Debian-based build container (`make build-container`) that carries the Rust toolchain pinned in `rust-toolchain.toml`.

The Linux kernel is built from source (`make kernel`) with a custom kernel config. You can specify the used kernel version in `hack/kernel/config.sh` and modify the kernel config using `make menuconfig`.

The virtual machine has a virtio-net NIC attached which will be connected to a Linux bridge on the host system. You can create and configure this bridge using `make network`. The NIC will appear as `eth0` within the VM.

With the make target `virsh-start` the VM will be created and started. `virsh-console` brings you into the serial console of the VM (you can exit it with `Ctrl+]`). To stop and destroy the VM call `make virsh-stop` - you'll probably want to concatenate those commands to `make virsh-start virsh-console virsh-stop`. This will start the VM, opens the serial console and waits for you to hit `Ctrl+]` to exit the serial console and destroy the VM.

### make run
To test feos locally, you can execute `make run` to compile and run feos locally as a non-PID 1 process. You will still need cloud-hypervisor in your path.

    make run


## Guest image (Firecracker)

The guest profile of FeOS runs as PID 1 in a Firecracker microVM, booted
directly from a kernel and an initramfs, without a root disk:

    make guest          # target/guest/vmlinuz and target/guest/initramfs.cpio.zst
    make test-guest     # downloads Firecracker, boots the guest, talks to it over vsock

The guest kernel (`hack/guest/kernel`) starts from Firecracker's minimal
microVM config with FeOS's changes on top: virtio-mmio devices, balloon and
vsock, IPv4 only, no modules, no RTC sync. `make guest-kernel-config`
regenerates it. The initramfs (`hack/guest/rootfs/mk-rootfs`) holds FeOS in
the guest profile as `/init` and a static uutils coreutils that containers
get as a toolbox. Kernel, Firecracker and coreutils downloads are pinned by
sha256. Builds run natively, without the build container and without root.

`make test-guest` attaches a data volume, which FeOS mounts at
/var/lib/feos, and checks the vsock API, the clock, filesystem freezing and
the lifecycle uplink with `feosctl --address vsock:target/guest/run/v.sock`.
It needs /dev/kvm and runs Firecracker with sudo when /dev/kvm is not
writable.

`make test-guest-container` also gives the guest a network: a bridge, a TAP
device and NAT on this machine (with sudo, removed when the test ends). It
pulls a busybox image in the guest and runs a container: exec, the tools,
watches, port forwarding, kube and SSH offloading, and the adoption of the
container after a reboot.

## Nested Virtualization Test Environment

For integration tests of the `vm-service`, FeOS needs to run within a virtual machine that itself can run other VMs (e.g., cloud-hypervisor). This requires a special host setup involving nested virtualization and a set of network services to support the FeOS guest environment.

For detailed instructions, please see the **[Nested Virtualization Test Environment Setup Guide](../docs/nested_virt_test_env.md)**.

## Cloud-Hypervisor
If you want to run FeOS within a [cloud-hypervisor](https://www.cloudhypervisor.org/) VM get some inspiration from this bash snippet:

    sudo ip tuntap add mode tap name tap0
    sudo ip link set tap0 up
    sudo ip link set tap0 master vm-br0
    
    cloud-hypervisor \
        --cpus boot=4 \
        --memory size=1024M \
        --net tap=tap0 \
        --serial tty \
        --kernel target/kernel/vmlinuz \
        --initramfs target/initramfs.zst \
        --cmdline "`cat target/cmdline`"


