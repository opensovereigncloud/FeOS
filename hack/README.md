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
To test feos locally, you can execute `make run` to compile and run feos locally as a non-PID 1 process. The VM service needs `firecracker` and its `jailer` in your path (`make firecracker` downloads them to `target/firecracker/`), and root.

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

`make test-vm` runs the integration tests of the `vm-service` against Firecracker, with the guest profile as the VM image. To run FeOS within a virtual machine that itself runs VMs, as on a real host, you need a special host setup involving nested virtualization and a set of network services to support the FeOS guest environment.

For detailed instructions, please see the **[Nested Virtualization Test Environment Setup Guide](../docs/nested_virt_test_env.md)**.

## Firecracker
To boot the host image under [Firecracker](https://firecracker-microvm.github.io/) (`make firecracker`), give it a configuration like this one. Firecracker has no nested virtualization, so FeOS cannot run VMs in there. FeOS takes 2 GiB for huge pages at boot, so the VM needs more memory than that.

    sudo ip tuntap add mode tap name tap0
    sudo ip link set tap0 up
    sudo ip link set tap0 master vm-br0

    cat > target/run-vm.json <<JSON
    {
      "boot-source": {
        "kernel_image_path": "target/kernel/vmlinuz",
        "initrd_path": "target/initramfs.zst",
        "boot_args": "console=ttyS0 reboot=k panic=1"
      },
      "drives": [],
      "machine-config": {"vcpu_count": 4, "mem_size_mib": 4096},
      "network-interfaces": [{"iface_id": "eth0", "host_dev_name": "tap0", "guest_mac": "06:00:ac:10:00:02"}]
    }
    JSON
    sudo target/firecracker/firecracker --no-api --config-file target/run-vm.json

Without a DHCPv6 server on the bridge, FeOS has only the link-local address of that MAC, `fe80::400:acff:fe10:2`. feosctl reaches its API through a forward:

    socat 'TCP6-LISTEN:1338,bind=[::1],fork' 'TCP6:[fe80::400:acff:fe10:2%vm-br0]:1337' &
    feosctl host version-info --address 'http://[::1]:1338'
