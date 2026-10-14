// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! S3 garbage collection (ADR 0011).
//!
//! The refcount authority is the **caller**, never diskd: it knows which
//! baked layers (plus their `l0`) and which pods' checkpoint heads are
//! still in use, and each head brings its disk chain and memory layer. diskd owns the object store, so the sweep runs here with the
//! live set handed in; this module is the pure decision half (no I/O, so
//! the delete sets are unit-testable against a fake listing).
//!
//! Three passes, each over one prefix listed exactly once (listing can be
//! slow):
//!
//! 1. `snapshots/` — an epoch prefix whose `head.json` the caller does not name
//!    is dead, and so is every object under it (vmstate included).
//! 2. `layers/` — a manifest neither the catalog nor a surviving head's
//!    chain names is dead.
//! 3. `chunks/` — a chunk no surviving manifest references is dead.
//!
//! **Grace.** Nothing younger than `grace_seconds` is ever deleted. The
//! upload order is chunks → manifest → head, so between the first chunk
//! PUT and the head that will reference it there is a window in which
//! every one of those objects looks unreferenced. The grace is what
//! keeps a sweep from eating a checkpoint that is still being written.

use std::collections::{HashMap, HashSet};

use crate::chunk::Hash;
use crate::s3::RemoteObject;

/// What one sweep decided, per prefix. Keys are relative to the store
/// prefix, ready for `S3::delete_object_key`.
#[derive(Debug, Default, Clone, PartialEq, Eq)]
pub struct SweepPlan {
    pub dead_snapshot_keys: Vec<String>,
    pub dead_bytes: u64,
    /// Heads that survived — the caller fetches them to expand their
    /// chains into the live layer set before pass 2.
    pub live_head_keys: Vec<String>,
}

/// `snapshots/<id>/<epoch>/…` → `("<id>/<epoch>", "<file>")`.
pub fn split_snapshot_key(key: &str) -> Option<(String, &str)> {
    let rest = key.strip_prefix("snapshots/")?;
    let (id, rest2) = rest.split_once('/')?;
    let (epoch, file) = rest2.split_once('/')?;
    if id.is_empty() || epoch.is_empty() || file.is_empty() {
        return None;
    }
    Some((format!("{id}/{epoch}"), file))
}

/// `layers/<layer-id>.manifest` → `<layer-id>`.
pub fn layer_id_of_key(key: &str) -> Option<&str> {
    key.strip_prefix("layers/")?.strip_suffix(".manifest")
}

/// `chunks/<hh>/<hex>.zst` → the hash's hex.
pub fn chunk_hex_of_key(key: &str) -> Option<&str> {
    let rest = key.strip_prefix("chunks/")?;
    let (hh, file) = rest.split_once('/')?;
    let hex = file.strip_suffix(".zst")?;
    if hex.len() != 64 || !hex.starts_with(hh) {
        return None;
    }
    Some(hex)
}

/// A non-positive grace protects nothing. Written this way on purpose:
/// a bucket's clock can sit a second or two ahead of ours, and
/// `now - modified < 0` would otherwise make every fresh object look
/// young forever under a zero grace.
fn too_young(o: &RemoteObject, now_unix: i64, grace_seconds: i64) -> bool {
    grace_seconds > 0 && now_unix.saturating_sub(o.modified_unix) < grace_seconds
}

/// Pass 1. An epoch prefix is dead when the caller does not name its head; a
/// prefix without a `head.json` at all is an interrupted upload and dies
/// the same way once it is past the grace. A single object still inside
/// the grace pins its whole epoch prefix — partial deletion of a
/// checkpoint is worse than keeping it one sweep longer.
pub fn plan_snapshots(
    objects: &[RemoteObject],
    live_head_keys: &HashSet<String>,
    now_unix: i64,
    grace_seconds: i64,
) -> SweepPlan {
    let mut by_epoch: HashMap<String, Vec<&RemoteObject>> = HashMap::new();
    for o in objects {
        if let Some((epoch_prefix, _)) = split_snapshot_key(&o.key) {
            by_epoch.entry(epoch_prefix).or_default().push(o);
        }
    }
    let mut plan = SweepPlan::default();
    for (_, group) in by_epoch {
        let head = group
            .iter()
            .find(|o| split_snapshot_key(&o.key).map(|(_, f)| f) == Some("head.json"));
        if let Some(h) = head
            && live_head_keys.contains(&h.key)
        {
            plan.live_head_keys.push(h.key.clone());
            continue;
        }
        if group.iter().any(|o| too_young(o, now_unix, grace_seconds)) {
            // Still inside the grace: it may be an upload in flight.
            if let Some(h) = head {
                plan.live_head_keys.push(h.key.clone());
            }
            continue;
        }
        for o in group {
            plan.dead_snapshot_keys.push(o.key.clone());
            plan.dead_bytes += o.size;
        }
    }
    plan.dead_snapshot_keys.sort();
    plan.live_head_keys.sort();
    plan
}

/// Pass 2. `live_layers` is the catalog's layers ∪ every surviving
/// head's chain ∪ its memory layer — the caller assembles it after
/// fetching the heads pass 1 kept.
pub fn plan_layers(
    objects: &[RemoteObject],
    live_layers: &HashSet<String>,
    now_unix: i64,
    grace_seconds: i64,
) -> (Vec<String>, u64) {
    let mut dead = Vec::new();
    let mut bytes = 0;
    for o in objects {
        let Some(id) = layer_id_of_key(&o.key) else {
            continue;
        };
        if live_layers.contains(id) || too_young(o, now_unix, grace_seconds) {
            continue;
        }
        dead.push(o.key.clone());
        bytes += o.size;
    }
    dead.sort();
    (dead, bytes)
}

/// Pass 3. `referenced` is the union of the entry tables of every
/// manifest that survived pass 2 — a chunk outside it is owned by
/// nobody.
pub fn plan_chunks(
    objects: &[RemoteObject],
    referenced: &HashSet<Hash>,
    now_unix: i64,
    grace_seconds: i64,
) -> (Vec<String>, u64) {
    let hexes: HashSet<String> = referenced.iter().map(|h| h.to_hex().to_string()).collect();
    let mut dead = Vec::new();
    let mut bytes = 0;
    for o in objects {
        let Some(hex) = chunk_hex_of_key(&o.key) else {
            continue;
        };
        if hexes.contains(hex) || too_young(o, now_unix, grace_seconds) {
            continue;
        }
        dead.push(o.key.clone());
        bytes += o.size;
    }
    dead.sort();
    (dead, bytes)
}

#[cfg(test)]
mod tests {
    use super::*;

    fn obj(key: &str, size: u64, modified_unix: i64) -> RemoteObject {
        RemoteObject {
            key: key.into(),
            size,
            modified_unix,
        }
    }

    const NOW: i64 = 1_000_000;
    const GRACE: i64 = 86_400;
    const OLD: i64 = NOW - 2 * GRACE;
    const FRESH: i64 = NOW - 10;

    #[test]
    fn key_parsers() {
        assert_eq!(
            split_snapshot_key("snapshots/sb-1/7/head.json"),
            Some(("sb-1/7".into(), "head.json"))
        );
        assert_eq!(
            split_snapshot_key("snapshots/sb-1/7/vmstate"),
            Some(("sb-1/7".into(), "vmstate"))
        );
        assert_eq!(split_snapshot_key("snapshots/sb-1/head.json"), None);
        assert_eq!(layer_id_of_key("layers/abcd.manifest"), Some("abcd"));
        assert_eq!(layer_id_of_key("layers/abcd.other"), None);
        let hex = "aa".to_string() + &"b".repeat(62);
        assert_eq!(
            chunk_hex_of_key(&format!("chunks/aa/{hex}.zst")),
            Some(hex.as_str())
        );
        // the shard must match the hash's first byte
        assert_eq!(chunk_hex_of_key(&format!("chunks/zz/{hex}.zst")), None);
    }

    #[test]
    fn snapshots_keep_referenced_and_fresh_epochs() {
        let objects = vec![
            obj("snapshots/a/1/head.json", 10, OLD),
            obj("snapshots/a/1/vmstate", 100, OLD),
            obj("snapshots/a/2/head.json", 10, OLD),
            obj("snapshots/a/2/vmstate", 100, OLD),
            // an upload in flight: no head yet, written seconds ago
            obj("snapshots/b/9/vmstate", 100, FRESH),
        ];
        let live: HashSet<String> = ["snapshots/a/2/head.json".to_string()].into();
        let plan = plan_snapshots(&objects, &live, NOW, GRACE);
        assert_eq!(
            plan.dead_snapshot_keys,
            vec!["snapshots/a/1/head.json", "snapshots/a/1/vmstate"]
        );
        assert_eq!(plan.dead_bytes, 110);
        assert_eq!(plan.live_head_keys, vec!["snapshots/a/2/head.json"]);
    }

    #[test]
    fn snapshots_sweep_an_abandoned_partial_upload_past_the_grace() {
        let objects = vec![obj("snapshots/b/9/vmstate", 100, OLD)];
        let plan = plan_snapshots(&objects, &HashSet::new(), NOW, GRACE);
        assert_eq!(plan.dead_snapshot_keys, vec!["snapshots/b/9/vmstate"]);
        assert!(plan.live_head_keys.is_empty());
    }

    #[test]
    fn layers_keep_the_catalog_and_the_grace() {
        let objects = vec![
            obj("layers/aa.manifest", 5, OLD),   // catalog layer
            obj("layers/bb.manifest", 7, OLD),   // dead private layer
            obj("layers/cc.manifest", 9, FRESH), // just pushed
        ];
        let live: HashSet<String> = ["aa".to_string()].into();
        let (dead, bytes) = plan_layers(&objects, &live, NOW, GRACE);
        assert_eq!(dead, vec!["layers/bb.manifest"]);
        assert_eq!(bytes, 7);
    }

    #[test]
    fn chunks_keep_everything_a_live_manifest_names() {
        let keep = Hash::from_bytes([0x11; 32]);
        let drop_it = Hash::from_bytes([0x22; 32]);
        let fresh = Hash::from_bytes([0x33; 32]);
        let key = |h: &Hash| {
            let hex = h.to_hex().to_string();
            format!("chunks/{}/{}.zst", &hex[0..2], hex)
        };
        let objects = vec![
            obj(&key(&keep), 64, OLD),
            obj(&key(&drop_it), 64, OLD),
            obj(&key(&fresh), 64, FRESH),
        ];
        let referenced: HashSet<Hash> = [keep].into();
        let (dead, bytes) = plan_chunks(&objects, &referenced, NOW, GRACE);
        assert_eq!(dead, vec![key(&drop_it)]);
        assert_eq!(bytes, 64);
    }

    #[test]
    fn a_zero_grace_protects_nothing_even_with_a_clock_ahead_of_ours() {
        // An object store's mtime a second in our future must not read as
        // "infinitely young".
        let objects = vec![
            obj("snapshots/a/1/head.json", 10, NOW + 2),
            obj("snapshots/a/1/vmstate", 100, NOW + 2),
        ];
        let plan = plan_snapshots(&objects, &HashSet::new(), NOW, 0);
        assert_eq!(plan.dead_snapshot_keys.len(), 2);
    }

    #[test]
    fn an_empty_live_set_still_respects_the_grace() {
        // The degenerate call (nothing is live at all) must not wipe a
        // checkpoint that is mid-upload.
        let objects = vec![
            obj("snapshots/a/1/head.json", 10, FRESH),
            obj("snapshots/a/1/vmstate", 100, FRESH),
        ];
        let plan = plan_snapshots(&objects, &HashSet::new(), NOW, GRACE);
        assert!(plan.dead_snapshot_keys.is_empty());
    }
}
