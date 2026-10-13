FROM rust:1-bookworm

RUN apt-get update && apt-get upgrade -y && apt-get install -y \
    flex bison bc libelf-dev \
    cpio zstd \
    gperf \
    ninja-build \
    protobuf-compiler \
    libprotobuf-dev \
    sbsigntool systemd-boot \
    musl-tools \
    ca-certificates \
    binutils

RUN cargo new xyz; cd xyz; cargo fetch; cd ..; rm -rf xyz
# The toolchain rust-toolchain.toml pins. It has to be in the image: the
# container runs as the calling user, who cannot write to RUSTUP_HOME.
ARG RUST_TOOLCHAIN
RUN test -n "$RUST_TOOLCHAIN" && rustup toolchain install "$RUST_TOOLCHAIN" \
    --profile minimal --component clippy,rustfmt --target x86_64-unknown-linux-musl
RUN mkdir /feos
RUN mkdir /target
WORKDIR /feos
