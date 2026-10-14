// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! Per-device counters + latency histograms. Stats is part of the
//! interface: the benches and lab tests read them.
//!
//! Lock-free only: the device thread is the sole writer; the control
//! plane reads via atomics. This is the sanctioned exception to
//! "no shared mutable state" — it's a read-mostly telemetry sink.

use std::sync::atomic::{AtomicU64, Ordering};

pub const HIST_BUCKETS: usize = 32;

#[derive(Default)]
pub struct DevStats {
    pub reads: AtomicU64,
    pub writes: AtomicU64,
    pub flushes: AtomicU64,
    pub discards: AtomicU64,
    pub read_bytes: AtomicU64,
    pub write_bytes: AtomicU64,
    pub cache_hits: AtomicU64,
    pub cache_misses: AtomicU64,
    pub s3_fetches: AtomicU64,
    pub s3_fetch_bytes: AtomicU64,
    pub errors: AtomicU64,
    /// Log2-µs buckets: read / write / flush.
    pub read_hist: Hist,
    pub write_hist: Hist,
    pub flush_hist: Hist,
}

pub struct Hist(pub [AtomicU64; HIST_BUCKETS]);

impl Default for Hist {
    fn default() -> Self {
        Hist(std::array::from_fn(|_| AtomicU64::new(0)))
    }
}

impl Hist {
    pub fn record(&self, d: std::time::Duration) {
        let us = d.as_micros() as u64;
        // floor(log2(us)): bucket i holds [2^i, 2^(i+1)) µs.
        let bucket = if us <= 1 {
            0
        } else {
            (63 - (us - 1).leading_zeros()) as usize
        };
        self.0[bucket.min(HIST_BUCKETS - 1)].fetch_add(1, Ordering::Relaxed);
    }

    pub fn snapshot(&self) -> Vec<u64> {
        self.0.iter().map(|a| a.load(Ordering::Relaxed)).collect()
    }

    /// Percentile in µs.
    pub fn percentile(&self, p: f64) -> f64 {
        let b: Vec<u64> = self.snapshot();
        let total: u64 = b.iter().sum();
        if total == 0 {
            return 0.0;
        }
        let want = (total as f64 * p) as u64;
        let mut acc = 0u64;
        for (i, &n) in b.iter().enumerate() {
            acc += n;
            if acc > want {
                return (1u64 << i.min(30)) as f64; // lower edge of bucket, µs
            }
        }
        (1u64 << 30) as f64
    }
}

impl DevStats {
    pub fn bump(c: &AtomicU64) {
        c.fetch_add(1, Ordering::Relaxed);
    }
    pub fn add(c: &AtomicU64, n: u64) {
        c.fetch_add(n, Ordering::Relaxed);
    }
}
