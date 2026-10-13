# FeOS — agent instructions

FeOS is a Rust PID 1 for bare-metal hypervisors and microVMs, in one monorepo.
Read `docs/development.md` (layout, profiles, commands, rules) and
`docs/adr/` before larger changes.

Hard rules:

- **Static musl.** Binaries build for `x86_64-unknown-linux-musl`, statically
  linked. Check new binaries with `file`.
- **Syscalls first**, through `nix` (or `libc` where nix lacks a wrapper). No
  shelling out to userspace tools, no glibc-specific behaviour.
- **PID 1 must not die.** No panics on the PID 1 path; return errors, log,
  keep serving.
- **Both profiles must build.** `make clippy` lints the host profile, the
  guest profile (`-p feos --no-default-features --features guest`) and both
  together. Host-only code goes behind `#[cfg(feature = "host")]`, guest-only
  code behind `#[cfg(feature = "guest")]`; features stay additive.
- **Release builds per package** (`-p`), never `--workspace`.
- **Keep authorship.** Use `git mv` to move files, and do not reformat code you
  are not otherwise changing.

Before you commit: `make fmt-check clippy test-unit`.
