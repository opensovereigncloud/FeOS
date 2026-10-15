# ADR 0002: One repository, laid out by language

**Status:** Accepted · **Date:** 2026-10-13

## Context

The platform (ADR 0001) consists of programs that run in different places:
`feos-guest` in every pod, `feosd` and what joins it on every node,
`feos-host-init` where FeOS is the node's operating system, the control
plane's services, and clients. Most features change more than one of them
at once: a capability of pods is an API in the guest, a call in `feosd`
and a route in the control plane.

Many building blocks cut across the programs: the proto definitions, the
vsock transport between host and guest, sandboxing helpers (namespaces,
capabilities, seccomp), netlink configuration, OCI image handling.

FeOS and the agent sandbox system both carry work of several contributors.
That work should stay in the codebase and in `git blame`, not only in the
history.

## Decision

1. **One repository** for the node, the control plane and the clients.
2. **Laid out by language, not by component.** Rust crates live flat under
   `crates/`, the service crates under `crates/services/`. Go and web code
   get a top-level directory each when they arrive. All APIs live under
   `proto/feos/<api>/v1/` with the package `feos.<api>.v1`.
3. **One Cargo workspace**, with four rules:
   - Release artifacts are built per package (`cargo build -p <crate>`),
     never with `--workspace`, which would unify the features of all crates
     into every binary.
   - `[workspace.dependencies]` declare minimal features; each crate adds
     what it needs, since a crate cannot turn off a feature the workspace
     enables.
   - One `release` profile. A binary that needs something else gets a
     custom profile that inherits from it.
   - Crates that need another toolchain, such as eBPF programs on nightly,
     stay outside the workspace.
4. **Node binaries are static musl** (`x86_64-unknown-linux-musl`). They
   depend on no library of the system they run on.
5. **FeOS evolves; it is not replaced.** The FeOS crates are the base, and
   the sandbox system's functionality is ported into them. Where its
   implementation is clearly better, for example image unpacking, it
   replaces the internals; the crate, its API and its structure stay.
   Files move with `git mv`.

## Alternatives considered

- **A repository for FeOS and one for the platform**, the platform taking
  released FeOS artifacts. A feature across the API would become a change,
  a release and a version bump on two sides.
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

## Consequences

- `rustfmt.toml` keeps the 2021 style edition, so that code nobody changed
  is not reformatted.
- CI lints and tests every binary's feature set.
- The shared building blocks (vsock transport, sandboxing helpers, the
  helper control protocol) are to be extracted into crates of their own.
