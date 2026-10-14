// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! diskd's chunk store (ADR 0011).
//!
//! Headless engine: no ublk, no gRPC. Chunking + blake3 + zstd, binary
//! layer manifests, seal (Variant A: SEEK_DATA/SEEK_HOLE change-set
//! rediscovery), local raw chunk cache with a budget actor, S3 tiering,
//! GC helpers.
//!
//! Shared-nothing: no Mutex/RwLock anywhere (clippy.toml denies them).
//! The budget actor is a std thread fed by an mpsc channel.

// Per-request/steady-state code must not panic; unwrap/expect are allowed
// in tests and at startup only.
#![cfg_attr(not(test), deny(clippy::unwrap_used, clippy::expect_used))]

pub mod cache;
pub mod chunk;
pub mod error;
pub mod gc;
pub mod head;
pub mod manifest;
pub mod materialize;
pub mod s3;
pub mod seal;
pub mod sweep;

pub use error::{Error, Result};
pub use manifest::{Entry, EntryKind, LayerId, Manifest};
