all:
	cargo build

release:
	cargo build --release

run: all
	./target/debug/feos

clean:
	rm -rf target

test:
	cargo test

include hack/hack.mk