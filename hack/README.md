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
To test feosd locally, you can execute `make run` to compile and run feosd as root. Pods need `firecracker` and its `jailer` in your path (`make firecracker` downloads them to `target/firecracker/`).

    make run

Pods with a network need `--pod-prefix` (`sudo ./target/x86_64-unknown-linux-musl/debug/feosd --pod-prefix fd00:fe05:1::/64` in a lab), and feosd then sets the machine's network up for them: forwarding, routes, the device `feos-dns` with the resolver's `169.254.0.53`, proxy ARP for `169.254.0.1` on the pods' TAP devices, and the table `inet feos` ([docs/architecture/node.md](../docs/architecture/node.md#the-pods-network)). Run it in a VM or a privileged container rather than on a machine whose network matters.


## Guest image (Firecracker)

feos-guest runs as PID 1 in a Firecracker microVM, booted
directly from a kernel and an initramfs, without a root disk:

    make guest          # target/guest/vmlinuz, initramfs.cpio.zst and data.img.zst
    make test-guest     # downloads Firecracker, boots the guest, talks to it over vsock

The guest kernel (`hack/guest/kernel`) starts from Firecracker's minimal
microVM config with FeOS's changes on top: virtio-mmio devices, balloon and
vsock, IPv6 and IPv4, no modules, no RTC sync. `make guest-kernel-config`
regenerates it. The initramfs (`hack/guest/rootfs/mk-rootfs`) holds
feos-guest as `/init`, `mke2fs` for the pods' new volumes, built from
e2fsprogs with `musl-gcc` and the kernel's headers (Debian: `musl-tools`,
`linux-libc-dev`), and a static uutils coreutils that containers get as a
toolbox. Kernel, Firecracker, e2fsprogs and coreutils downloads are pinned
by sha256. Builds run natively, without the build container and without
root.

`make test-guest` attaches a data volume, which FeOS mounts at
/var/lib/feos, and checks the vsock API, the clock, filesystem freezing and
the lifecycle uplink with `feosctl --address vsock:target/guest/run/v.sock`.
It needs /dev/kvm and runs Firecracker with sudo when /dev/kvm is not
writable.

`make test-guest-container` also gives the guest a pod's network: a TAP
device in a network namespace that stands for the node and, as feosd
does, answers ARP for the guest's gateway `169.254.0.1` by proxy, and NAT
on this machine (with sudo, removed when the test ends). It pulls a busybox image
in the guest and runs a container: exec, the tools, watches, port
forwarding, kube and SSH offloading, the network pushes of both families,
and the adoption of the container after a reboot.

## Packages

The `.deb` packages of ADR 0009 build with nfpm, which `make packages`
downloads, pinned by sha256 (`hack/packages/`):

    make packages        # target/packages/*.deb: feosd, feosctl, diskd, Firecracker, the guest
    make test-packages   # installs them on Debian 13 with systemd, in docker, and checks feosd
    make publish-apt     # builds HEAD as a dev version and publishes it to apt.fecode.dev

`make test-packages` boots a pod from the installed guest, restarts feosd
under it, gives feosd an `fd00:` pod prefix and a denied prefix in
`/etc/default/feosd`, and runs pods on its network (busybox and curl from
Docker Hub, so it needs IPv4 internet access, IPv6 not): they have an
IPv6 address of the prefix and an IPv4 address of `169.254.16.0/20`
behind proxy ARP for `169.254.0.1`, resolve through `169.254.0.53` and
reach the internet over IPv4, by name, by literal and with ICMP; they
reach neither each other nor the node, `169.254.169.254` or a denied
prefix, and one pod the other only over IPv6 on the port a security
group opens; `feosctl traffic watch` records their DNS queries, flows of
both families, drops and a TLS server name. A deleted pod takes its TAP
device and its connections in connection tracking, and the next pod gets
an IPv4 address never used. It execs in a pod through feosd, suspends,
resumes, stops and starts it, restarts feosd under it with its network,
and powers the container off through `feosctl host shutdown`.
`make publish-apt` reads the bucket's credentials from
`~/.config/feos/apt/r2.env` and signs with the key in
`~/.config/feos/apt/gnupg`; both stay on the machine that publishes.

## diskd

The disk daemon ([docs/architecture/disk.md](../docs/architecture/disk.md))
has its labs in `hack/diskd/`. They need the ublk driver, sudo and fio;
the state is in `target/diskd/`:

    make diskd               # static diskd and diskctl
    make diskd-lab-up        # start a lab diskd under sudo
    make diskd-lab           # the lab tests against it
    make diskd-lab-drill     # SIGKILL diskd under fio; it must recover
    make diskd-lab-down      # stop it

S3 is used when `S3_BUCKET` and `AWS_*` are in the environment; the lab
tests that need it skip otherwise. Nothing reads credentials from files.

## Nested Virtualization Test Environment

`make test-pods` runs the pod integration tests against Firecracker, with feos-guest installed as its package would. To run FeOS within a virtual machine that itself runs microVMs, as on a real host, you need a special host setup involving nested virtualization and a set of network services to support the FeOS guest environment.

For detailed instructions, please see the **[Nested Virtualization Test Environment Setup Guide](../docs/guides/test-environment.md)**.

## Firecracker
To boot the host image under [Firecracker](https://firecracker-microvm.github.io/) (`make firecracker`), give it a configuration like this one. Firecracker has no nested virtualization, so FeOS cannot run pods in there.

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
