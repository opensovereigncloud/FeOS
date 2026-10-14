// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! Shared lab harness for the kernel-level drills (crash recovery, writes
//! through the device, cold S3 reads). The lab tests are #[ignore]d: they
//! need root, ublk and a running lab daemon, and `make diskd-lab` runs
//! them. Without the daemon or root they skip.
#![allow(dead_code)]

use std::path::{Path, PathBuf};
use std::process::{Command, Output};

pub struct Lab {
    pub root: PathBuf,  // workspace root
    pub diskd: PathBuf, // workspace root, where hack/diskd and target/ are
    pub state: PathBuf, // daemon state dir
    pub socket: PathBuf,
    pub ctl: PathBuf, // diskctl binary
}

pub fn ensure_lab_daemon() -> bool {
    let l = Lab::new();
    if !l.socket.exists() {
        println!("skipping lab test (no daemon — run `make diskd-lab-up` first)");
        return false;
    }
    if !nix::unistd::Uid::effective().is_root() {
        println!("skipping lab test (needs root: sudo -E ...)");
        return false;
    }
    true
}

/// A lab daemon as above, and S3 in the environment (S3_BUCKET, AWS_*):
/// the daemon and the test process take the bucket from there.
pub fn ensure_lab_s3() -> bool {
    if std::env::var("S3_BUCKET")
        .map(|b| b.is_empty())
        .unwrap_or(true)
    {
        println!("skipping lab test (no S3: set S3_BUCKET and AWS_*)");
        return false;
    }
    ensure_lab_daemon()
}

impl Lab {
    pub fn new() -> Self {
        let root = PathBuf::from(env!("CARGO_MANIFEST_DIR"))
            .parent()
            .unwrap() // crates
            .parent()
            .unwrap() // the workspace
            .to_path_buf();
        let diskd = root.clone();
        let state = std::env::var("DISKD_STATE")
            .map(PathBuf::from)
            .unwrap_or_else(|_| root.join("target/diskd/state"));
        // Prefer the build matching `cargo test --release` (make diskd-lab);
        // fall back to debug for plain `cargo test` runs.
        let pick = |name: &str| {
            let rel = diskd.join(format!("target/release/{name}"));
            if rel.exists() {
                rel
            } else {
                diskd.join(format!("target/debug/{name}"))
            }
        };
        Self {
            socket: state.join("diskd.sock"),
            state,
            ctl: pick("diskctl"),
            root,
            diskd,
        }
    }

    /// Run a diskd hack script; returns stdout+stderr. Err on nonzero exit.
    pub fn run_hack(&self, script: &str, args: &[&str]) -> Result<String, String> {
        let out = Command::new(self.diskd.join("hack/diskd").join(script))
            .args(args)
            .output()
            .map_err(|e| format!("spawn {script}: {e}"))?;
        let both = format!(
            "{}\n{}",
            String::from_utf8_lossy(&out.stdout),
            String::from_utf8_lossy(&out.stderr)
        );
        if out.status.success() {
            Ok(both)
        } else {
            Err(both)
        }
    }

    pub fn ctl(&self, args: &[&str]) -> Output {
        Command::new(&self.ctl)
            .args(["--socket".into(), self.socket.display().to_string()])
            .args(args)
            .output()
            .expect("diskctl spawn")
    }

    pub fn ctl_json(&self, args: &[&str]) -> Result<serde_json::Value, String> {
        let out = self.ctl(args);
        if !out.status.success() {
            return Err(format!(
                "diskctl {:?} failed: {}",
                args,
                String::from_utf8_lossy(&out.stderr)
            ));
        }
        serde_json::from_slice(&out.stdout).map_err(|e| format!("bad json: {e}"))
    }

    /// The layer manifest this daemon holds locally (tests compare
    /// entry tables: who owns which chunk).
    pub fn load_layer_manifest(&self, layer_id: &str) -> disk_store::Manifest {
        let p = self
            .state
            .join("layers")
            .join(format!("{layer_id}.manifest"));
        disk_store::Manifest::decode(&std::fs::read(&p).unwrap_or_else(|e| {
            panic!("read {}: {e}", p.display());
        }))
        .expect("manifest decode")
    }

    pub fn cleanup(&self, pod: &str) {
        let _ = self.ctl(&["stop", pod]);
        let _ = self.ctl(&["delete", pod]);
    }

    /// Spawn a second daemon on a distinct state dir (a "node B"; tests run as root under sudo -E, inheriting S3 creds).
    /// Returns the child Lab view; the caller stops devices and kills
    /// the returned process.
    pub fn spawn_daemon(&self, state: &Path, extra_args: &[&str]) -> (Lab, std::process::Child) {
        let _ = std::fs::remove_dir_all(state);
        std::fs::create_dir_all(state).unwrap();
        self.start_daemon(state, extra_args)
    }

    /// Start a daemon on an EXISTING state dir (restart with different
    /// flags, keeping layers/cache/markers).
    pub fn start_daemon(&self, state: &Path, extra_args: &[&str]) -> (Lab, std::process::Child) {
        let _ = std::fs::remove_file(state.join("diskd.sock"));
        let log = std::fs::File::create(state.join("diskd.log")).unwrap();
        // `make diskd-lab` runs cargo test --release (so the release daemon
        // exists); fall back to the debug build for plain `cargo test`.
        let release = self.diskd.join("target/release/diskd");
        let bin = if release.exists() {
            release
        } else {
            self.diskd.join("target/debug/diskd")
        };
        let mut args: Vec<String> = vec!["--state-dir".into(), state.display().to_string()];
        for a in extra_args {
            args.push(a.to_string());
        }
        let child = std::process::Command::new(bin)
            .args(&args)
            .stdout(std::process::Stdio::from(log.try_clone().unwrap()))
            .stderr(std::process::Stdio::from(log))
            .spawn()
            .expect("spawn daemon");
        let sock = state.join("diskd.sock");
        for _ in 0..300 {
            let o = std::process::Command::new(&self.ctl)
                .args([
                    "--socket".into(),
                    sock.display().to_string(),
                    "stats".into(),
                ])
                .output();
            if o.map(|o| o.status.success()).unwrap_or(false) {
                break;
            }
            std::thread::sleep(std::time::Duration::from_millis(100));
        }
        assert!(sock.exists(), "spawned daemon never created its socket");
        (
            Lab {
                root: self.root.clone(),
                diskd: self.diskd.clone(),
                state: state.to_path_buf(),
                socket: sock,
                ctl: self.ctl.clone(),
            },
            child,
        )
    }

    pub fn stop(&self, pod: &str) {
        // best-effort
        let _ = self.ctl(&["stop", pod]);
    }

    pub fn xfs_cache(&self) -> PathBuf {
        self.root.join("target/diskd/xfs/cache")
    }

    /// Deterministic local base image. Shared across runs (created once) so S3 objects are
    /// reused.
    pub fn base_image(&self) -> PathBuf {
        let img = self.root.join("target/diskd/imported/lab-base.raw");
        if !img.is_file() {
            std::fs::create_dir_all(img.parent().unwrap()).unwrap();
            let o = Command::new("sh")
                .arg("-c")
                .arg(format!(
                    "head -c 67108864 /dev/urandom > {p}",
                    p = img.display()
                ))
                .output()
                .expect("base image write");
            assert!(o.status.success());
        }
        img
    }

    pub fn is_root() -> bool {
        nix::unistd::Uid::effective().is_root()
    }
}
