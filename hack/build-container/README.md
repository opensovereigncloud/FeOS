FeOS Build Container
---------------------

The Build Container is used to provide a build environment, that is consistent across different build hosts.
Currently the Build Container is derived from the official [Rust 1-bookworm Docker container](https://hub.docker.com/_/rust). It is used to build the Linux kernel, the FeOS rust application and the initramfs.
It installs the Rust toolchain that `rust-toolchain.toml` pins.

FeOS links statically against musl, so no library from this container ends up in the initramfs.
The CA certificates in the initramfs do come from it, though.

You should care about keeping this container up to date!
Rebuild it from time to time, and whenever `rust-toolchain.toml` changes, using `make build-container`!
