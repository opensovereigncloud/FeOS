build-container:
	-docker rmi feos-builder
	docker pull rust:1-bookworm
	docker build hack/build-container -t feos-builder \
		--build-arg RUST_TOOLCHAIN=$(shell sed -n 's/^channel = "\(.*\)"/\1/p' rust-toolchain.toml)
