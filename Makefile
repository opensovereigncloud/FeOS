clippy:
	cargo clippy

all: clippy
	cargo build

release: clippy
	cargo build --release

run: all
	./target/debug/feos

clean:
	rm -rf target

test: clippy
	cargo test

include hack/hack.mk