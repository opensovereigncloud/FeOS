##@ NAT64

.PHONY: nat64-bpf test-nat64

# The program builds on the nightly that crates/nat64-ebpf/rust-toolchain.toml
# names, with bpf-linker (cargo install bpf-linker). The object is committed:
# no other build needs either.
nat64-bpf: ## Rebuild the NAT64 eBPF program: crates/nat64/bpf/nat64.bpf.o (nightly, bpf-linker).
	cd crates/nat64-ebpf && cargo build --release --target-dir ../../target/nat64-ebpf
	cp target/nat64-ebpf/bpfel-unknown-none/release/nat64 crates/nat64/bpf/nat64.bpf.o
	sha256sum crates/nat64/bpf/nat64.bpf.o

test-nat64: ## Test NAT64 in network namespaces: echo, TCP, UDP, ICMP errors, restart; needs sudo.
	hack/nat64/test
