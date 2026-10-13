# ADR 0001: FeOS becomes a monorepo

**Status:** Accepted · **Date:** 2026-10-13

## Context

FeOS is a Rust PID 1 for bare-metal hypervisors: it configures itself from the
network and runs VMs and containers. An agent sandbox system is built the
same way: a Rust PID 1 for Firecracker microVMs that run agent sandboxes,
with host-side components around it. The two share their lineage (Rust PID 1,
tonic gRPC, `nix`, youki, the proto layout, the kernel build), and the
sandbox system is to be merged into FeOS.

After the merge, the repository holds several programs that run in different
places:

- in the microVM, the PID 1 that runs containers;
- on each hypervisor node, a manager that starts Firecracker microVMs, a disk
  daemon, and per-sandbox proxies that keep credentials out of the VM;
- a control plane with the sandbox API, and clients.

Many building blocks cut across these programs: the proto definitions, the
vsock transport between host and guest, sandboxing helpers (namespaces,
capabilities, seccomp), netlink configuration, OCI image handling.

FeOS carries years of work from several contributors. That work should stay in
the codebase and in `git blame`, not only in the history.

## Decision

1. **One repository** for all of these components.
2. **Laid out by language, not by component.** Rust crates live flat under
   `crates/`, the service crates under `crates/services/`. Go and web code get
   their own top-level directory when they arrive. All APIs live under
   `proto/feos/<api>/v1/` with the package `feos.<api>.v1`.
3. **One Cargo workspace**, with four rules:
   - Release artifacts are built per package (`cargo build -p <crate>`), never
     with `--workspace`, which would unify the features of all crates into
     every binary.
   - `[workspace.dependencies]` declare minimal features; each crate adds what
     it needs, since a crate cannot turn off a feature the workspace enables.
   - One `release` profile. Binaries that need something else get a custom
     profile that inherits from it, as `release-guest` does.
   - Crates that need another toolchain, such as eBPF programs on nightly, stay
     outside the workspace.
4. **FeOS evolves; it is not replaced.** The FeOS crates are the base, and
   the sandbox system's functionality is ported into them. Where its
   implementation is clearly better, for example image unpacking, it
   replaces the internals; the crate, its API and its structure stay.
5. **Two profiles of `feos`, selected with cargo features.** `host` (the
   default) is FeOS as a bare-metal hypervisor OS; `guest` is PID 1 in a
   microVM that its host configures. Features stay additive.

## Alternatives considered

- **A directory per component**, as the sandbox system's repository has.
  The shared building blocks would need a home outside the components
  anyway, and each component directory would end up wrapping a single
  binary crate.
- **A workspace per component.** Shared crates would be built once per
  workspace, the lockfiles would drift apart, and there would be no single
  lint and test run. What separate workspaces give (isolated features and
  profiles, other toolchains) the rules in point 3 provide too.
- **Replacing FeOS with the sandbox system's code.** Less work, since that
  code is newer in most overlapping areas, but it would remove the FeOS
  contributors' code from the project.
- **Selecting the profile at runtime.** The guest binary would then carry the
  host-only code and dependencies (DHCPv6, raw sockets, NTP, the VM service).

## Consequences

- Files move with `git mv`, so `git blame` follows them. `rustfmt.toml` keeps
  the 2021 style edition to avoid reformatting code nobody changed.
- CI lints both profiles.
- Next: port the sandbox system's guest functionality into the FeOS crates,
  bring over the host-side components, and extract the shared building
  blocks (vsock transport, sandboxing helpers, the helper control protocol)
  into crates.
