FeOS Developer Documentation
============================

See [overview.md](overview.md) for what FeOS is, [architecture/](architecture/)
for how it works, [/hack/README.md](/hack/README.md) for building and booting
FeOS images, and [/docs/adr](/docs/adr) for the decisions behind it.

## Repository layout

FeOS is a monorepo. Rust crates live under `crates/` in one Cargo workspace;
other languages get their own top-level directory when they arrive.

```
crates/
  feos/                 feosd and feos-guest: their startup, the wiring of
                        the services, the task supervisor
  feos-host-init/       PID 1 of a node whose operating system is FeOS:
                        machine setup (network, NTP, hugepages, SR-IOV)
  feos-proto/           generated gRPC code for proto/
  feos-utils/           shared building blocks: logger, mounts, reaper, vsock,
                        resolvers, confined container file access, dispatch
  feosctl/              command-line client
  services/
    container-service/  containers: overlay roots, exec, adoption, persistence
    host-service/       host information, logs, power, upgrade, time, network
                        push, filesystem freezing, memory
    image-service/      OCI image pulls and the image store
    pod-service/        pods on top of the VM service, and the forwarding
                        of a pod's services to its guest
    task-service/       the container runtime (libcontainer), its own process
    vm-service/         VMs through a Hypervisor trait (Firecracker, jailed):
                        snapshots, balloon, the guest's uplink and pushes
    tool-service/       guest: agent file and web tools on a container's files
    watch-service/      guest: inotify subscriptions on a container's files
    forward-service/    guest: port forwarding, kube and SSH credential offloading
proto/feos/<api>/v1/    the gRPC APIs, package feos.<api>.v1
hack/                   kernel, initramfs, UKI, guest image and VM tooling
docs/                   documentation and ADRs
```

## Binaries

Each binary is named after where it runs ([ADR 0003](adr/0003-programs.md)):

- **`feosd`** (`crates/feos`, feature `host`, the default): the node agent,
  under systemd or `feos-host-init`, never PID 1. It runs the VM and pod
  services and serves the public API on `/run/feos/api.sock`, and on TCP
  with `--tcp`. VMs run in Firecracker, in its jailer
  ([ADR 0005](adr/0005-firecracker.md),
  [ADR 0008](adr/0008-suspend-and-restore.md)).
- **`feos-guest`** (`crates/feos`, `--no-default-features --features
  guest`): PID 1 in a microVM whose host configures it. Its API listens on
  vsock port 10000, not on TCP, and once it is ready it dials the host
  (vsock port 10001) and holds a `LifecycleService.Attach` call open. A data
  volume, if the VM has one, holds images and containers. It also serves
  the tool, watch, port forward, kube and SSH services for the agent in its
  containers. None of the node's code or dependencies are in this binary.
- **`feos-host-init`** (`crates/feos-host-init`): PID 1 when FeOS is the
  node's operating system. It sets the machine up and runs `feosd`.
- **`feosctl`** (`crates/feosctl`): the client.

`feosd` and `feos-guest` run the container runtime in a process of their
own, `--task-service`, and start it again when it exits. See
[ADR 0007](adr/0007-guest.md) for the guest's API.

Both serve the public API on `/run/feos/api.sock`. Features are additive: code
that only `feosd` needs sits behind `#[cfg(feature = "host")]`; code that only
the guest needs behind `#[cfg(feature = "guest")]`. With both features,
`crates/feos` builds both binaries and its library does both, which is only
useful for development.

## Building, linting, testing

You need the toolchain from `rust-toolchain.toml` (rustup installs it on first
use) and `protoc`.

    make clippy          # lint feosd's and feos-guest's features, -D warnings
    make fmt-check
    make test-unit       # unit tests, no root needed
    make test            # all tests; the integration tests need root
    make release         # target/x86_64-unknown-linux-musl/release/{feosd,feos-host-init}
    make release-guest   # target/x86_64-unknown-linux-musl/release/feos-guest
    make cli             # feosctl

    make guest                 # guest kernel and initramfs: target/guest/
    make test-guest            # boot the guest under Firecracker and talk to it
    make test-guest-container  # run containers in the guest; needs sudo for a
                               # bridge, a TAP device and NAT
    make test-vm               # the VM service against Firecracker, with the
                               # guest as VM image; needs docker

Build release artifacts per package (`cargo build -p <crate>`), never with
`--workspace`: a workspace build unifies the features of all crates into every
binary.

## Rules

- **Static musl.** FeOS binaries are built for `x86_64-unknown-linux-musl` and
  linked statically. Check new binaries with `file`; they must say
  "statically linked" or "static-pie linked".
- **Syscalls first.** Talk to the kernel through syscalls, using
  [`nix`](https://docs.rs/nix) (or `libc` where nix has no wrapper). Do not
  shell out to userspace tools such as `ip` or `mount`, and do not depend on
  glibc-specific behaviour.
- **PID 1 must not die.** When PID 1 exits, the kernel panics. Code on the
  PID 1 path returns errors instead of panicking, and `main` idles rather
  than exit. It must keep working when filesystems are read-only.
- **Keep authorship.** FeOS has history worth keeping in `git blame`. Move
  files with `git mv`, and do not reformat code you are not changing:
  `rustfmt.toml` keeps the 2021 style edition for that reason.

## Protocol buffers

Each API has its own package `feos.<api>.v1` in
`proto/feos/<api>/v1/<api>.proto`. `crates/feos-proto` compiles all of them;
a new file needs an entry in its `build.rs` and a module in its `lib.rs`.
