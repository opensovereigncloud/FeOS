// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! Command line (clap derive). `--flag value` and `--flag=value` are the
//! same flag; a hand-rolled parser that only took the first once kept the
//! daemon from starting.

use std::path::PathBuf;

use clap::Parser;

/// Local chunk cache budget: 8 GiB of reconstructible bytes per node.
const DEFAULT_BUDGET_BYTES: u64 = 8 << 30;

#[derive(Parser, Debug)]
#[command(
    name = "diskd",
    about = "The node's disk daemon: ublk devices over a chunk store (ADR 0011)",
    version
)]
pub struct Args {
    /// Daemon state: chunk cache, layer manifests, device table, live links.
    #[arg(long, default_value = "/var/lib/feos/disk")]
    pub state_dir: PathBuf,

    /// gRPC control socket [default: <state-dir>/diskd.sock].
    #[arg(long)]
    socket: Option<PathBuf>,

    /// Local chunk cache budget in bytes; eviction drops reconstructible
    /// chunks down to it.
    #[arg(long, default_value_t = DEFAULT_BUDGET_BYTES)]
    pub budget_bytes: u64,

    /// Bounded parallelism for S3 PUT/GET. A single stream is far too slow
    /// to hibernate a pod.
    #[arg(long, default_value_t = 16)]
    pub s3_concurrency: usize,

    /// Chunks the UFFD memory backend pulls, in parallel, beyond the one
    /// the guest is blocked on. A fault is a serial S3 round trip by
    /// construction — the vCPU is stopped — so 0 trades one big parallel
    /// download for a long chain of small sequential ones.
    #[arg(long, env = "DISKD_MEM_READAHEAD", default_value_t = 8)]
    pub mem_readahead: u32,

    /// Key prefix inside the bucket. "" in production; the labs give
    /// themselves an isolated prefix so a destructive drill (the sweeper)
    /// can never reach objects in use.
    #[arg(long, env = "DISKD_S3_PREFIX", default_value = "")]
    s3_prefix: String,
}

impl Args {
    /// The control socket, defaulted into the state dir.
    pub fn socket(&self) -> PathBuf {
        self.socket
            .clone()
            .unwrap_or_else(|| self.state_dir.join("diskd.sock"))
    }

    /// The bucket prefix, always either empty or slash-terminated (every
    /// key is built by concatenation).
    pub fn s3_prefix(&self) -> String {
        if self.s3_prefix.is_empty() || self.s3_prefix.ends_with('/') {
            self.s3_prefix.clone()
        } else {
            format!("{}/", self.s3_prefix)
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn both_spellings_of_a_flag_are_the_same_flag() {
        let a = Args::parse_from(["diskd", "--state-dir=/var/lib/feos/disk"]);
        let b = Args::parse_from(["diskd", "--state-dir", "/var/lib/feos/disk"]);
        assert_eq!(a.state_dir, b.state_dir);
        assert_eq!(a.socket(), b.socket());
        assert_eq!(a.socket(), PathBuf::from("/var/lib/feos/disk/diskd.sock"));
    }

    #[test]
    fn defaults() {
        let a = Args::parse_from(["diskd"]);
        assert_eq!(a.state_dir, PathBuf::from("/var/lib/feos/disk"));
        assert_eq!(a.socket(), PathBuf::from("/var/lib/feos/disk/diskd.sock"));
        assert_eq!(a.budget_bytes, 8 << 30);
        assert_eq!(a.s3_concurrency, 16);
        assert_eq!(a.mem_readahead, 8);
        assert_eq!(a.s3_prefix(), "");
    }

    #[test]
    fn an_explicit_socket_wins_over_the_state_dir() {
        let a = Args::parse_from(["diskd", "--state-dir", "/s", "--socket", "/run/d.sock"]);
        assert_eq!(a.socket(), PathBuf::from("/run/d.sock"));
    }

    #[test]
    fn the_bucket_prefix_always_ends_in_a_slash() {
        assert_eq!(
            Args::parse_from(["diskd", "--s3-prefix", "diskd-test/run7"]).s3_prefix(),
            "diskd-test/run7/"
        );
        assert_eq!(
            Args::parse_from(["diskd", "--s3-prefix", "already/"]).s3_prefix(),
            "already/"
        );
    }

    #[test]
    fn unknown_flags_are_refused_rather_than_ignored() {
        assert!(Args::try_parse_from(["diskd", "--nope"]).is_err());
    }
}
