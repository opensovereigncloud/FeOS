# FeOS — agent instructions

FeOS is the node layer for microVM pods, in one monorepo: `feosd` (the node
agent), `feos-guest` (PID 1 in a pod), `feos-host-init` (PID 1 of a node
whose operating system is FeOS) and `feosctl`. Read `docs/overview.md`,
`docs/development.md` (layout, binaries, commands, rules) and `docs/adr/`
before larger changes.

Hard rules:

- **Static musl.** Binaries build for `x86_64-unknown-linux-musl`, statically
  linked. Check new binaries with `file`.
- **Syscalls first**, through `nix` (or `libc` where nix lacks a wrapper). No
  shelling out to userspace tools, no glibc-specific behaviour. The one
  exception: `mke2fs` in the guest, to format a new volume (ADR 0016);
  nothing else.
- **PID 1 must not die.** No panics on the PID 1 path; return errors, log,
  keep serving.
- **Both feature sets must build.** `make clippy` lints `feosd`'s features
  (the default), `feos-guest`'s (`-p feos --no-default-features --features
  guest`) and both together. feosd-only code goes behind
  `#[cfg(feature = "host")]`, guest-only code behind
  `#[cfg(feature = "guest")]`; features stay additive.
- **Release builds per package** (`-p`), never `--workspace`.
- **Keep authorship.** Use `git mv` to move files, and do not reformat code you
  are not otherwise changing.

Before you commit: `make fmt-check clippy test-unit`. When you change what
runs in the guest, also `make guest test-guest test-guest-container` (needs
/dev/kvm, and sudo for the test network). When you change the pod or
microVM service, also `make test-pods` (needs docker).
