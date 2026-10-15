// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! A pod's head (`head.json`): the binding point between its disk chain
//! and its memory snapshot. JSON, committed last in every pause/seal
//! sequence (ADR 0011).

use std::fs;
use std::path::{Path, PathBuf};

use serde::{Deserialize, Serialize};

use crate::cache::fsync_dir;
use crate::error::{Error, Result};

/// Memory side of a hibernation checkpoint (ADR 0011). `layer` is the flat memory layer (SealFile), `vmstate`
/// the Firecracker snapshot object; both under the same
/// `snapshots/<id>/<epoch>/` prefix as the head itself.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct MemoryRef {
    /// Layer id (hex) of the flat memory-dump layer.
    pub layer: String,
    /// S3 key of the Firecracker vmstate object.
    pub vmstate_key: String,
    /// Manager pause epoch this checkpoint belongs to.
    pub epoch: u64,
    /// Placement fingerprint: restore needs a matching node
    /// (`snapshot/load` refuses otherwise).
    pub fc_version: String,
    pub cpu_model: String,
    /// The host kernel whose KVM saved the vCPUs; empty in heads from
    /// before it was recorded.
    #[serde(default)]
    pub kernel_release: String,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct PodHead {
    pub version: u32, // = 1
    pub pod_id: String,
    pub logical_size: u64,
    pub chunk_size: u32,
    /// Ordered layer chain, L0 first. The newest entry is the layer the
    /// mutable top (if any) builds on.
    pub chain: Vec<String>,
    /// How many leading layers of the chain are shared; the rest, at most
    /// one, is the pod's own, which the next seal replaces. A restore
    /// keeps it, so that the chain does not grow by a layer per node it
    /// passes. Heads from before it was recorded hold the whole chain as
    /// shared.
    #[serde(default)]
    pub shared_depth: Option<usize>,
    /// Whether `live/<id>/top.img` currently exists (dirty mutable layer).
    pub has_top: bool,
    /// The checkpoint's epoch, as in its key; 0 in a node's local head.
    #[serde(default)]
    pub epoch: u64,
    /// Memory checkpoint refs (ADR 0011).
    #[serde(default)]
    pub memory: Option<MemoryRef>,
    /// RFC 3339 for operators' eyes only.
    pub updated_at: String,
}

/// S3 key of the head of pause epoch `epoch`
/// (`snapshots/<id>/<epoch>/head.json`; never overwritten — the caller
/// decides which one is current).
pub fn head_key(pod_id: &str, epoch: u64) -> String {
    format!("snapshots/{pod_id}/{epoch}/head.json")
}

/// S3 key of the Firecracker vmstate object of pause epoch `epoch`.
pub fn vmstate_key(pod_id: &str, epoch: u64) -> String {
    format!("snapshots/{pod_id}/{epoch}/vmstate")
}

impl PodHead {
    pub fn new(
        pod_id: impl Into<String>,
        logical_size: u64,
        chunk_size: u32,
        chain: Vec<String>,
    ) -> Self {
        PodHead {
            version: 1,
            pod_id: pod_id.into(),
            logical_size,
            chunk_size,
            chain,
            shared_depth: None,
            has_top: false,
            epoch: 0,
            memory: None,
            updated_at: String::new(),
        }
    }

    /// How many leading layers of the chain are shared: what the head
    /// says, and the whole chain for a head that does not say.
    pub fn shared_depth(&self) -> usize {
        self.shared_depth
            .unwrap_or(self.chain.len())
            .min(self.chain.len())
    }

    pub fn path(state_dir: &Path, pod_id: &str) -> PathBuf {
        state_dir.join("snapshots").join(pod_id).join("head.json")
    }

    pub fn load(state_dir: &Path, pod_id: &str) -> Result<PodHead> {
        let p = Self::path(state_dir, pod_id);
        let buf = fs::read(&p).map_err(|e| Error::Head(format!("{}: {e}", p.display())))?;
        Self::decode(&buf)
    }

    /// Parse and **validate**. A head is the only description a restoring
    /// node has of the device it is about to serve: `OpenDisk{head_key}`
    /// builds its `DeviceRow` straight out of `logical_size`/`chunk_size`.
    /// A head that omits them (a writer that forgot to fill the fields)
    /// used to yield a silently zero-sized ublk device whose first guest
    /// read blew up as EBADMSG far away from the cause — so reject it
    /// here, where the key of the offending object is still in hand.
    pub fn decode(buf: &[u8]) -> Result<PodHead> {
        let head: PodHead =
            serde_json::from_slice(buf).map_err(|e| Error::Head(format!("head.json: {e}")))?;
        head.validate()?;
        Ok(head)
    }

    /// Structural invariants of a head, checked on every decode.
    pub fn validate(&self) -> Result<()> {
        if self.pod_id.is_empty() {
            return Err(Error::Head("head.json: empty pod_id".into()));
        }
        if self.chain.is_empty() {
            return Err(Error::Head(format!(
                "head.json for {}: empty chain",
                self.pod_id
            )));
        }
        if self.logical_size == 0 || self.chunk_size == 0 {
            return Err(Error::Head(format!(
                "head.json for {}: missing disk geometry (logical_size={} chunk_size={}) \
                 — a device opened from it would have size 0",
                self.pod_id, self.logical_size, self.chunk_size
            )));
        }
        // L0 is always shared: a seal must never merge it away.
        if let Some(depth) = self.shared_depth
            && (depth == 0 || depth > self.chain.len())
        {
            return Err(Error::Head(format!(
                "head.json for {}: {depth} shared layers of a chain of {}",
                self.pod_id,
                self.chain.len()
            )));
        }
        Ok(())
    }

    pub fn encode(&self) -> Result<Vec<u8>> {
        serde_json::to_vec_pretty(self).map_err(|e| Error::Head(e.to_string()))
    }

    /// Records that the head changes now.
    pub fn stamp(&mut self) {
        self.updated_at = now_rfc3339();
    }

    /// Atomic commit: tmp file, fsync, rename, dir fsync. This is the
    /// last write of every seal/pause sequence.
    pub fn store(&mut self, state_dir: &Path) -> Result<()> {
        self.stamp();
        let p = Self::path(state_dir, &self.pod_id);
        if let Some(dir) = p.parent() {
            fs::create_dir_all(dir)?;
        }
        let tmp = p.with_extension("json.tmp");
        let buf = serde_json::to_vec_pretty(self).map_err(|e| Error::Head(e.to_string()))?;
        fs::write(&tmp, &buf)?;
        fs::File::open(&tmp)?.sync_all()?;
        fs::rename(&tmp, &p)?;
        if let Some(dir) = p.parent() {
            fsync_dir(dir)?;
        }
        Ok(())
    }
}

fn now_rfc3339() -> String {
    // Avoid a chrono dependency for a cosmetic field; seconds since epoch
    // formatted minimally is fine for v1.
    match std::time::SystemTime::now().duration_since(std::time::UNIX_EPOCH) {
        Ok(d) => format!("{}s", d.as_secs()),
        Err(_) => String::new(),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn decode_rejects_missing_geometry() {
        // Exactly the shape a caller that forgets to fill the geometry
        // produces; restoring from it used to hand out a size-0 device.
        let buf = br#"{"version":1,"pod_id":"sb","logical_size":0,
            "chunk_size":0,"chain":["aa"],"has_top":true,"updated_at":""}"#;
        let err = PodHead::decode(buf).expect_err("zero geometry must not decode");
        assert!(err.to_string().contains("geometry"), "{err}");
    }

    #[test]
    fn decode_rejects_empty_chain() {
        let buf = br#"{"version":1,"pod_id":"sb","logical_size":1024,
            "chunk_size":65536,"chain":[],"has_top":false,"updated_at":""}"#;
        PodHead::decode(buf).expect_err("empty chain must not decode");
    }

    #[test]
    fn decode_accepts_a_well_formed_head() {
        let mut h = PodHead::new("sb", 1 << 30, 1 << 16, vec!["aa".into()]);
        h.has_top = true;
        let buf = h.encode().unwrap();
        let back = PodHead::decode(&buf).expect("round-trip");
        assert_eq!(back.logical_size, 1 << 30);
        assert_eq!(back.chunk_size, 1 << 16);
    }

    #[test]
    fn a_head_keeps_which_layer_is_the_pods_own() {
        let mut h = PodHead::new("sb", 1 << 30, 1 << 16, vec!["aa".into(), "bb".into()]);
        // A head from before shared_depth: the whole chain is shared.
        assert_eq!(h.shared_depth(), 2);
        h.shared_depth = Some(1);
        h.epoch = 7;
        let back = PodHead::decode(&h.encode().unwrap()).unwrap();
        assert_eq!((back.shared_depth(), back.epoch), (1, 7));
        h.shared_depth = Some(0);
        PodHead::decode(&h.encode().unwrap()).expect_err("L0 is never the pod's own");
        h.shared_depth = Some(3);
        PodHead::decode(&h.encode().unwrap()).expect_err("deeper than the chain");
        // Old heads decode, without the new fields.
        let old = br#"{"version":1,"pod_id":"sb","logical_size":1024,"chunk_size":65536,
            "chain":["aa"],"has_top":false,"updated_at":"",
            "memory":{"layer":"cc","vmstate_key":"k","epoch":1,"fc_version":"1.17.0",
            "cpu_model":"x"}}"#;
        let old = PodHead::decode(old).unwrap();
        assert_eq!(old.shared_depth(), 1);
        assert_eq!(old.memory.unwrap().kernel_release, "");
    }
}
