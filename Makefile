all:
	cargo build

release:
	cargo build --release

run: all
	./target/debug/feos

clean:
	rm -rf target

include hack/hack.mk