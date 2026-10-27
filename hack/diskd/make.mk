##@ diskd

.PHONY: diskd diskd-lab-up diskd-lab-down diskd-lab diskd-lab-drill diskd-mkscratch diskd-bench-store

diskd: ## Release build of diskd and diskctl, static: target/x86_64-unknown-linux-musl/release/.
	cargo build --release --target=$(MUSL_TARGET) -p diskd
	cargo build --release --target=$(MUSL_TARGET) -p diskctl
	@for b in diskd diskctl; do \
		file target/$(MUSL_TARGET)/release/$$b | grep -qE 'static(-pie)? linked' \
			|| { echo "$$b is not statically linked"; exit 1; }; \
	done

diskd-lab-up: ## Start a lab diskd under sudo (state in target/diskd/state; S3 from the env, if set).
	cargo build --release -p diskd
	cargo build --release -p diskctl
	hack/diskd/lab-up

diskd-lab-down: ## Stop the lab diskd (LAB_FLAGS=--clean stops its devices first).
	hack/diskd/lab-down $(LAB_FLAGS)

# The lab tests are #[ignore]d; this runs them against the lab daemon, as
# root through cargo's runner, so the build stays the user's. The ones that
# need S3 skip without S3_BUCKET and AWS_* in the environment.
diskd-lab: diskd-lab-up ## Run diskd's lab tests: ublk, recovery, writes, flatten, S3 tiering; needs sudo and fio.
	DISKD_STATE="$$(readlink -f target/diskd/state)" CARGO_TARGET_X86_64_UNKNOWN_LINUX_GNU_RUNNER="sudo -E" \
		cargo test --release -p diskd --test 'lab_*' -- --ignored --test-threads=1 --nocapture

diskd-lab-drill: diskd-lab-up ## SIGKILL diskd under fio and check that it recovers its device.
	hack/diskd/lab-drill-recovery lab-drill

diskd-mkscratch: ## A loop-backed XFS volume at target/diskd/xfs for the benchmarks (sudo).
	sudo hack/diskd/mkscratch

diskd-bench-store: ## The chunk store's benchmark; refuses anything but XFS (make diskd-mkscratch).
	@stat -f -c %T target/diskd/xfs 2>/dev/null | grep -q '^xfs$$' \
		|| { echo "target/diskd/xfs is not XFS: make diskd-mkscratch"; exit 1; }
	cargo run --release -p disk-store --example bench-store -- \
		--out target/diskd/bench --state target/diskd/xfs/bench-store
