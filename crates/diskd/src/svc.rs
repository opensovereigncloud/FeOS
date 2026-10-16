// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! gRPC control plane (tonic on a unix socket).
//!
//! Locking: the device table is a `Mutex` used
//! ONLY here in the control plane — the ublk request path (`diskd::dev`)
//! denies disallowed types outright and never touches this map. Every
//! blocking store/device call runs in `spawn_blocking`.
//!
//! Beyond devices: `Bake` (live seal of a frozen top), flatten-on-seal as
//! `Seal`'s default behaviour, `SealFile` (flat memory
//! layers), `OpenDisk{head_key}` (restore from a foreign node's head),
//! `PutHead`/`GetHead` (a checkpoint's head, last), per-chunk upload
//! markers + bounded-parallel S3 PUT/GET (`Push`, `Pull --warm`, miss
//! hydration, materialize).

#![allow(clippy::disallowed_types)] // control plane only — see module docs

use std::collections::HashMap;
use std::path::PathBuf;
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::{Arc, Mutex};

use feos_proto::disk_service as pb;
use feos_proto::disk_service::disk_service_server::DiskService;
use futures::StreamExt as _;
use tonic::{Request, Response, Status};
use tracing::info;

use crate::dev::{self, DeviceCtx, StartedDevice};
use crate::state::{DeviceRow, State};
use crate::uffd::{self, MemStats, MemoryCtx, StartedMemory};

type DeviceTable = Arc<Mutex<HashMap<String, StartedDevice>>>;
/// Live UFFD memory handlers, one per restored pod. Control plane
/// only, exactly like `DeviceTable` — the fault path never looks here.
type MemoryTable = Arc<Mutex<HashMap<String, StartedMemory>>>;

#[derive(Clone)]
pub struct DiskSvc {
    pub state_dir: PathBuf,
    pub cache: Arc<disk_store::cache::Cache>,
    pub s3: Option<Arc<disk_store::s3::S3>>,
    pub devices: DeviceTable,
    pub memories: MemoryTable,
    /// Bounded parallelism for S3 PUT/GET (`--s3-concurrency`).
    pub s3_concurrency: usize,
    /// `--mem-readahead`: chunks the UFFD backend pulls ahead of a fault.
    pub mem_readahead: u32,
}

/// Nothing younger than this is swept (ADR 0011): between the first
/// chunk PUT of a checkpoint and the head.json that names it, every one
/// of its objects looks unreferenced from the bucket's side.
const DEFAULT_SWEEP_GRACE_SECONDS: i64 = 24 * 60 * 60;
/// Soft wall-clock budget for one sweep. Listing can be slow; a
/// sweep that runs out of budget stops between passes and the next one
/// picks up where the bucket now is.
const DEFAULT_SWEEP_BUDGET_SECONDS: u64 = 15 * 60;

fn internal(e: impl std::fmt::Display) -> Status {
    Status::internal(e.to_string())
}

/// Upload/download paths run at SCHED_IDLE so S3 I/O never contends with
/// the pod's own I/O in the other direction. Best-effort: failure (e.g. in
/// a container) is silent. The raw syscall: musl's sched_setscheduler
/// only returns ENOSYS.
pub(crate) fn set_sched_idle() {
    // SAFETY: thread 0 = calling thread; sched_param is zeroed plain data
    // (priority 0, as SCHED_IDLE wants) and outlives the call.
    unsafe {
        let param: libc::sched_param = std::mem::zeroed();
        libc::syscall(
            libc::SYS_sched_setscheduler,
            0,
            libc::SCHED_IDLE,
            &param as *const libc::sched_param,
        );
    }
}

impl DiskSvc {
    pub fn link_path(&self, pod_id: &str) -> PathBuf {
        self.state_dir.join("live").join(pod_id).join("disk")
    }

    pub fn top_path(&self, pod_id: &str) -> PathBuf {
        self.state_dir.join("live").join(pod_id).join("top.img")
    }

    fn load_layer(&self, id: &str) -> Result<disk_store::Manifest, String> {
        let layer_id = disk_store::manifest::LayerId::from_hex(id)
            .map_err(|e| format!("bad layer id {id}: {e}"))?;
        let p = self
            .state_dir
            .join("layers")
            .join(format!("{layer_id}.manifest"));
        let buf = std::fs::read(&p).map_err(|e| format!("{}: {e}", p.display()))?;
        disk_store::Manifest::decode(&buf).map_err(|e| e.to_string())
    }

    fn load_chain(&self, ids: &[String]) -> Result<Vec<disk_store::Manifest>, String> {
        let mut out = Vec::with_capacity(ids.len());
        for (i, id) in ids.iter().enumerate() {
            let m = self.load_layer(id)?;
            if i > 0 {
                let want = disk_store::manifest::LayerId::from_hex(&ids[i - 1])
                    .map_err(|e| e.to_string())?;
                if m.parent != Some(want) {
                    return Err(format!("chain not contiguous at {id}"));
                }
            }
            out.push(m);
        }
        Ok(out)
    }

    fn layer_path(&self, id_hex: &str) -> PathBuf {
        self.state_dir
            .join("layers")
            .join(format!("{id_hex}.manifest"))
    }

    fn save_row(&self, pod_id: &str, row: DeviceRow) -> Result<(), String> {
        let mut st = State::load(&self.state_dir).map_err(|e| e.to_string())?;
        st.devices.insert(pod_id.to_string(), row);
        State::store(&self.state_dir, &st).map_err(|e| e.to_string())
    }

    /// Startup/drill recovery: reattach a previously-seen device.
    pub(crate) fn spawn_dev_recovery(
        &self,
        row: DeviceRow,
        pod_id: String,
    ) -> Result<pb::Device, String> {
        self.spawn_dev(row, pod_id, true)
    }

    /// Everything device spawn needs (runs on a blocking thread).
    fn spawn_dev(
        &self,
        row: DeviceRow,
        pod_id: String,
        recover: bool,
    ) -> Result<pb::Device, String> {
        let chain = self.load_chain(&row.chain)?;
        let logical = row.logical_size;
        let live = self.state_dir.join("live").join(&pod_id);
        std::fs::create_dir_all(&live).map_err(|e| e.to_string())?;
        let top = self.top_path(&pod_id);
        if !top.is_file() {
            let f = std::fs::File::create(&top).map_err(|e| e.to_string())?;
            f.set_len(logical).map_err(|e| e.to_string())?;
        }

        let started = dev::spawn(
            DeviceCtx {
                pod_id: pod_id.clone(),
                state_dir: self.state_dir.clone(),
                chain: Arc::new(chain),
                top: Some(top),
                logical_size: logical,
                cache: self.cache.clone(),
                s3: self.s3.clone(),
                shared: Arc::new(dev::DevShared::default()),
                fetch_concurrency: self.s3_concurrency,
            },
            recover,
        )?;
        let dev_id = started.dev_id;
        let bdev = started.bdev.clone();

        // The symlink Firecracker is configured with (ADR 0011).
        let link = self.link_path(&pod_id);
        let _ = std::fs::remove_file(&link);
        std::os::unix::fs::symlink(&bdev, &link).map_err(|e| e.to_string())?;
        self.devices
            .lock()
            .map_err(|_| "poisoned")?
            .insert(pod_id.clone(), started);

        self.save_row(
            &pod_id,
            DeviceRow {
                dev_id: Some(dev_id),
                ..row
            },
        )?;

        Ok(pb::Device {
            pod_id,
            dev_path: bdev.display().to_string(),
            link_path: link.display().to_string(),
            size_bytes: logical,
        })
    }
}

// ---- upload tracking -----------------------------------------------------
//
// Per-chunk upload markers under `uploaded/chunks/<hh>/<hash>`: `Push` skips marked chunks — this is what makes memory
// "increments" and flatten cheap on the wire (an unchanged page hashes to
// a chunk S3 already has and is not uploaded again). Markers are HINTS,
// never durability-proofed: they are written unsynced, a lost marker
// costs one redundant PUT, and `Fsck --check-s3` rebuilds them from S3
// truth (marker present ⟺ object present).

impl DiskSvc {
    fn chunk_marker_dir(&self) -> PathBuf {
        self.state_dir.join("uploaded").join("chunks")
    }

    fn chunk_marker(&self, h: &disk_store::chunk::Hash) -> PathBuf {
        let hex = h.to_hex().to_string();
        self.chunk_marker_dir().join(&hex[0..2]).join(hex)
    }

    fn chunk_uploaded(&self, h: &disk_store::chunk::Hash) -> bool {
        self.chunk_marker(h).is_file()
    }

    /// Record "this chunk is in S3". Hint only — no fsync (see module
    /// comment above); errors are logged, not fatal.
    fn mark_chunk_uploaded(&self, h: &disk_store::chunk::Hash) {
        let p = self.chunk_marker(h);
        if p.is_file() {
            return;
        }
        if let Some(dir) = p.parent()
            && !dir.is_dir()
        {
            let _ = std::fs::create_dir_all(dir);
        }
        if let Err(e) = std::fs::write(&p, b"") {
            tracing::debug!(hash = %h.to_hex(), error = %e, "chunk marker write failed");
        }
    }

    fn unmark_chunk(&self, h: &disk_store::chunk::Hash) {
        let _ = std::fs::remove_file(self.chunk_marker(h));
    }

    fn mark_uploaded(&self, layer_id: &str) -> Result<(), String> {
        let dir = self.state_dir.join("uploaded");
        std::fs::create_dir_all(&dir).map_err(|e| e.to_string())?;
        std::fs::write(
            dir.join(layer_id),
            "
",
        )
        .map_err(|e| e.to_string())
    }

    /// Chunks that must never be evicted by policy: chunks of live device
    /// chains plus chunks of layers not yet pushed (tenure).
    fn tenure_protected(&self) -> std::collections::HashSet<disk_store::chunk::Hash> {
        let mut keep = std::collections::HashSet::new();
        let add_layer =
            |id: &str, keep: &mut std::collections::HashSet<disk_store::chunk::Hash>| {
                if let Ok(m) = self.load_layer(id) {
                    for e in &m.entries {
                        if e.kind == disk_store::manifest::EntryKind::Data {
                            keep.insert(disk_store::chunk::Hash::from(e.hash));
                        }
                    }
                }
            };
        if let Ok(st) = State::load(&self.state_dir) {
            for row in st.devices.values() {
                for id in &row.chain {
                    add_layer(id, &mut keep);
                }
            }
        }
        if let Ok(rd) = std::fs::read_dir(self.state_dir.join("layers")) {
            for e in rd.flatten() {
                let id = e
                    .file_name()
                    .to_string_lossy()
                    .trim_end_matches(".manifest")
                    .to_string();
                if !self.state_dir.join("uploaded").join(&id).is_file() {
                    add_layer(&id, &mut keep);
                }
            }
        }
        keep
    }

    /// Push a layer and all missing ancestors, oldest-first (a chain is
    /// only usable whole on another node).
    async fn push_chain(&self, id_hex: &str) -> Result<(), String> {
        // collect the not-yet-uploaded chain segment, head -> old
        let mut pending: Vec<String> = Vec::new();
        let mut cur = id_hex.to_string();
        let s3 = self.s3.clone();
        loop {
            let m = self.load_layer(&cur)?;
            let marker = self.state_dir.join("uploaded").join(&cur).is_file();
            // marker + real probe: stale markers (e.g. lab cleanup
            // deleting manifests) must not masquerade as tiered
            let tiered = if marker {
                if let Some(s3) = &s3 {
                    let lid =
                        disk_store::manifest::LayerId::from_hex(&cur).map_err(|e| e.to_string())?;
                    s3.has_manifest(&lid).await
                } else {
                    marker
                }
            } else {
                false
            };
            // Scan the WHOLE chain — an ancestor's upload may be stale
            // even when the head is fine (S3 deletions, partial pushes).
            if !tiered {
                pending.push(cur.clone());
            }
            match m.parent {
                Some(p) => cur = p.hex(),
                None => break,
            }
        }
        let pending: Vec<String> = pending.into_iter().rev().collect();
        for id in &pending {
            self.push_layer(id).await?;
        }
        Ok(())
    }

    /// Push one layer: chunks (marked-uploaded ones skipped) at bounded
    /// concurrency, manifest last (crash-safe ordering, ADR 0011).
    async fn push_layer(&self, id_hex: &str) -> Result<(), String> {
        set_sched_idle();
        let s3 = self
            .s3
            .clone()
            .ok_or_else(|| "S3 not configured".to_string())?;
        let lid = disk_store::manifest::LayerId::from_hex(id_hex).map_err(|e| e.to_string())?;
        let m = self.load_layer(id_hex)?;
        let hashes: Vec<disk_store::chunk::Hash> = m
            .entries
            .iter()
            .filter(|e| e.kind == disk_store::manifest::EntryKind::Data)
            .map(|e| disk_store::chunk::Hash::from(e.hash))
            .collect();
        let n_bytes = AtomicU64::new(0);
        let conc = self.s3_concurrency.max(1);
        let results: Vec<Result<(), String>> = futures::stream::iter(hashes)
            .map(|h| {
                let s3 = s3.clone();
                let n_bytes = &n_bytes;
                async move {
                    if self.chunk_uploaded(&h) {
                        return Ok(());
                    }
                    let p = self.cache.chunk_path(&h);
                    if p.is_file() {
                        let raw = tokio::task::spawn_blocking({
                            let p = p.clone();
                            move || std::fs::read(&p)
                        })
                        .await
                        .map_err(|e| e.to_string())?
                        .map_err(|e| e.to_string())?;
                        s3.put_chunk(&h, &raw).await.map_err(|e| e.to_string())?;
                        n_bytes.fetch_add(raw.len() as u64, Ordering::Relaxed);
                        self.mark_chunk_uploaded(&h);
                    } else if s3.has_chunk(&h).await {
                        // Evicted locally, present remotely (the memory-increment
                        // case): learn the marker without a re-upload.
                        self.mark_chunk_uploaded(&h);
                    } else {
                        return Err(format!("chunk missing locally and in S3: {}", h.to_hex()));
                    }
                    Ok(())
                }
            })
            .buffer_unordered(conc)
            .collect()
            .await;
        for r in results {
            r?;
        }
        s3.put_manifest(&lid, &m).await.map_err(|e| e.to_string())?;
        let dir = self.state_dir.join("uploaded");
        std::fs::create_dir_all(&dir).map_err(|e| e.to_string())?;
        let n_bytes = n_bytes.load(Ordering::Relaxed);
        std::fs::write(dir.join(id_hex), format!("{n_bytes}\n")).map_err(|e| e.to_string())?;
        info!(layer = %id_hex, bytes = n_bytes, "push: layer uploaded");
        Ok(())
    }

    /// Ensure the manifest of `id_hex` and its ancestors are local (one
    /// S3 fetch walk), marking every fetched layer uploaded. The S3-side
    /// existence of a manifest proves every chunk it references is in S3
    /// (chunks-before-manifest upload rule) — so chunk markers are
    /// learned for free and later pushes dedup against them.
    /// A manifest for the sweep's reachability walk: the local copy when
    /// this node happens to have one, otherwise straight from S3. Never
    /// caches — the sweeper must not pull the whole bucket onto a node.
    async fn manifest_for_sweep(
        &self,
        id_hex: &str,
        s3: &disk_store::s3::S3,
    ) -> Result<disk_store::Manifest, String> {
        if self.layer_path(id_hex).is_file() {
            return self.load_layer(id_hex);
        }
        let lid = disk_store::manifest::LayerId::from_hex(id_hex).map_err(|e| e.to_string())?;
        s3.get_manifest(&lid)
            .await
            .map_err(|e| format!("sweep: manifest {id_hex}: {e}"))
    }

    /// Transitive parents of `ids`. A head names its whole chain today,
    /// but the catalog names only the baked layer — its parent L0 is
    /// reachable and must survive.
    async fn expand_parents(
        &self,
        ids: &std::collections::HashSet<String>,
        s3: &disk_store::s3::S3,
    ) -> Vec<String> {
        let mut out = Vec::new();
        let mut seen: std::collections::HashSet<String> = ids.clone();
        let mut queue: Vec<String> = ids.iter().cloned().collect();
        while let Some(id) = queue.pop() {
            let m = match self.manifest_for_sweep(&id, s3).await {
                Ok(m) => m,
                // A live id with no manifest anywhere is the caller's
                // problem, not the sweeper's; skipping it only ever
                // keeps MORE objects (its chunks stay unreferenced-
                // looking, and their own grace still protects them).
                Err(_) => continue,
            };
            if let Some(parent) = m.parent {
                let pid = parent.hex();
                if seen.insert(pid.clone()) {
                    out.push(pid.clone());
                    queue.push(pid);
                }
            }
        }
        out
    }

    async fn ensure_manifests(&self, id_hex: &str) -> Result<(), String> {
        let s3 = self
            .s3
            .clone()
            .ok_or_else(|| "S3 not configured".to_string())?;
        let mut id = id_hex.to_string();
        loop {
            let path = self.layer_path(&id);
            let m = if path.is_file() {
                self.load_layer(&id)?
            } else {
                let lid =
                    disk_store::manifest::LayerId::from_hex(&id).map_err(|e| e.to_string())?;
                let m = s3.get_manifest(&lid).await.map_err(|e| e.to_string())?;
                if m.layer_id() != lid {
                    return Err(format!("manifest id mismatch for {id}"));
                }
                std::fs::create_dir_all(self.state_dir.join("layers"))
                    .map_err(|e| e.to_string())?;
                let tmp = self.state_dir.join(format!("layers/.{id}.tmp"));
                std::fs::write(&tmp, m.encode()).map_err(|e| e.to_string())?;
                std::fs::rename(&tmp, &path).map_err(|e| e.to_string())?;
                for e in &m.entries {
                    if e.kind == disk_store::manifest::EntryKind::Data {
                        self.mark_chunk_uploaded(&disk_store::chunk::Hash::from(e.hash));
                    }
                }
                self.mark_uploaded(&id)?;
                m
            };
            match m.parent {
                Some(p) => id = p.hex(),
                None => return Ok(()),
            }
        }
    }
}

#[tonic::async_trait]
impl DiskService for DiskSvc {
    async fn import_base(
        &self,
        request: Request<pb::ImportBaseRequest>,
    ) -> Result<Response<pb::Layer>, Status> {
        let req = request.into_inner();
        let this = self.clone();
        let path = req.path.clone();
        let res = tokio::task::spawn_blocking(move || {
            // A compressed image is unpacked first, sparse, next to the
            // cache: the seal walks the image's data extents only.
            let unpacked = path.ends_with(".zst").then(|| {
                static IMPORTS: AtomicU64 = AtomicU64::new(0);
                this.state_dir.join("staging").join(format!(
                    "import-{}-{}.img",
                    std::process::id(),
                    IMPORTS.fetch_add(1, Ordering::Relaxed)
                ))
            });
            let image = match &unpacked {
                Some(raw) => {
                    std::fs::create_dir_all(this.state_dir.join("staging"))
                        .map_err(|e| e.to_string())?;
                    disk_store::materialize::unpack_zstd(std::path::Path::new(&path), raw)
                        .map_err(|e| format!("unpack {path}: {e}"))?;
                    raw.clone()
                }
                None => PathBuf::from(&path),
            };
            let sealed = (|| {
                let md = std::fs::metadata(&image).map_err(|e| e.to_string())?;
                let logical = md.len();
                let chunk_size = if req.chunk_size == 0 {
                    disk_store::chunk::DEFAULT_DISK_CHUNK_SIZE
                } else {
                    req.chunk_size
                };
                disk_store::seal::seal_top(
                    &image,
                    &[],
                    None,
                    &this.cache,
                    &this.state_dir,
                    logical,
                    chunk_size,
                    None,
                )
                .map_err(|e| e.to_string())
            })();
            if let Some(raw) = &unpacked {
                let _ = std::fs::remove_file(raw);
            }
            sealed
        })
        .await
        .map_err(internal)?
        .map_err(internal)?;
        info!(layer = %res.layer_id, path = %req.path, "import_base");
        Ok(Response::new(pb::Layer {
            layer_id: res.layer_id.hex(),
            entries: res.manifest.entries.len() as u64,
            chunks_new: res.stats.chunks_new,
            chunks_inherited: res.stats.chunks_inherited,
            chunks_zeroed: res.stats.chunks_zeroed,
            chunks_pool_hit: res.stats.chunks_pool_hit,
            bytes_new: res.stats.bytes_new,
            seal_seconds: res.stats.seal_seconds,
            logical_bytes: res.manifest.data_bytes(),
        }))
    }

    async fn create_disk(
        &self,
        request: Request<pb::CreateDiskRequest>,
    ) -> Result<Response<pb::Device>, Status> {
        let req = request.into_inner();
        if req.pod_id.is_empty() || req.chain.is_empty() {
            return Err(Status::invalid_argument("pod_id and chain required"));
        }
        if self
            .devices
            .lock()
            .map_err(|_| internal("poisoned"))?
            .contains_key(&req.pod_id)
        {
            return Err(Status::already_exists(req.pod_id));
        }
        let chain = self.load_chain(&req.chain).map_err(internal)?;
        let head = chain.last().ok_or_else(|| internal("empty chain"))?;
        let logical = if req.logical_size == 0 {
            head.logical_size
        } else {
            req.logical_size
        };
        let row = DeviceRow {
            dev_id: None,
            chain: req.chain.clone(),
            logical_size: logical,
            chunk_size: head.chunk_size,
            has_top: true,
            // Every layer handed in at create is shared/immutable; the
            // pod's private layer only appears at the first seal.
            shared_depth: req.chain.len(),
        };
        let this = self.clone();
        let pod_id = req.pod_id;
        let dev = tokio::task::spawn_blocking(move || this.spawn_dev(row, pod_id, false))
            .await
            .map_err(internal)?
            .map_err(internal)?;
        Ok(Response::new(dev))
    }

    async fn open_disk(
        &self,
        request: Request<pb::OpenDiskRequest>,
    ) -> Result<Response<pb::Device>, Status> {
        let req = request.into_inner();
        let (pod_id, row) = if req.head_key.is_empty() {
            // Started already: its caller lost the answer, or never knew.
            let live = self
                .devices
                .lock()
                .map_err(|_| internal("poisoned"))?
                .get(&req.pod_id)
                .map(|d| d.bdev.clone());
            if let Some(bdev) = live {
                let st = State::load(&self.state_dir).map_err(internal)?;
                return Ok(Response::new(pb::Device {
                    dev_path: bdev.display().to_string(),
                    link_path: self.link_path(&req.pod_id).display().to_string(),
                    size_bytes: st
                        .devices
                        .get(&req.pod_id)
                        .map_or(0, |row| row.logical_size),
                    pod_id: req.pod_id,
                }));
            }
            let st = State::load(&self.state_dir).map_err(internal)?;
            let row = st
                .devices
                .get(&req.pod_id)
                .cloned()
                .ok_or_else(|| Status::not_found(req.pod_id.clone()))?;
            (req.pod_id.clone(), row)
        } else {
            // Restore on a foreign node: pull the head and its manifests
            // from S3, create the row here, start over the chain with a
            // fresh top.
            let s3 = self
                .s3
                .clone()
                .ok_or_else(|| Status::failed_precondition("S3 not configured"))?;
            let buf = s3
                .get_object(&req.head_key)
                .await
                .map_err(|e| internal(format!("head {}: {e}", req.head_key)))?;
            let head = disk_store::head::PodHead::decode(&buf)
                .map_err(|e| internal(format!("head {}: {e}", req.head_key)))?;
            if !req.pod_id.is_empty() && req.pod_id != head.pod_id {
                return Err(Status::invalid_argument(format!(
                    "pod {} != head's {}",
                    req.pod_id, head.pod_id
                )));
            }
            let Some(chain_head) = head.chain.last() else {
                return Err(Status::invalid_argument("head has an empty chain"));
            };
            if self
                .devices
                .lock()
                .map_err(|_| internal("poisoned"))?
                .contains_key(&head.pod_id)
            {
                return Err(Status::already_exists(head.pod_id));
            }
            // A disk the node has of the pod may hold writes the head does
            // not: its top would end up over another chain.
            if State::load(&self.state_dir)
                .map_err(internal)?
                .devices
                .contains_key(&head.pod_id)
            {
                return Err(Status::failed_precondition(format!(
                    "this node has a disk of pod {} already: remove it first",
                    head.pod_id
                )));
            }
            self.ensure_manifests(chain_head).await.map_err(internal)?;
            // The pod's own layer stays its own, so that the next seal
            // replaces it and the chain keeps its shape; a head that does
            // not say which it is holds the whole chain as shared.
            let row = DeviceRow {
                dev_id: None,
                chain: head.chain.clone(),
                logical_size: head.logical_size,
                chunk_size: head.chunk_size,
                has_top: true,
                shared_depth: head.shared_depth(),
            };
            (head.pod_id.clone(), row)
        };
        let this = self.clone();
        let dev = tokio::task::spawn_blocking(move || this.spawn_dev(row, pod_id, false))
            .await
            .map_err(internal)?
            .map_err(internal)?;
        Ok(Response::new(dev))
    }

    async fn stop_disk(
        &self,
        request: Request<pb::StopDiskRequest>,
    ) -> Result<Response<pb::Empty>, Status> {
        let req = request.into_inner();
        let started = {
            let mut t = self.devices.lock().map_err(|_| internal("poisoned"))?;
            t.remove(&req.pod_id)
                .ok_or_else(|| Status::not_found(req.pod_id.clone()))?
        };
        tokio::task::spawn_blocking(move || dev::stop(started))
            .await
            .map_err(internal)?
            .map_err(internal)?;
        let _ = std::fs::remove_file(self.link_path(&req.pod_id));
        // Row stays (top kept); clear the dev id.
        let mut st = State::load(&self.state_dir).map_err(internal)?;
        if let Some(row) = st.devices.get_mut(&req.pod_id) {
            row.dev_id = None;
        }
        State::store(&self.state_dir, &st).map_err(internal)?;
        info!(pod = %req.pod_id, "stop_disk");
        Ok(Response::new(pb::Empty {}))
    }

    async fn remove_device(
        &self,
        request: Request<pb::RemoveDeviceRequest>,
    ) -> Result<Response<pb::RemoveDeviceResponse>, Status> {
        let req = request.into_inner();
        if req.pod_id.is_empty() {
            return Err(Status::invalid_argument("pod_id required"));
        }
        if self
            .devices
            .lock()
            .map_err(|_| internal("poisoned"))?
            .contains_key(&req.pod_id)
        {
            return Err(Status::failed_precondition(
                "device still live; stop it first",
            ));
        }
        let this = self.clone();
        let pod = req.pod_id;
        tokio::task::spawn_blocking(move || -> Result<(), String> {
            let mut st = State::load(&this.state_dir).map_err(|e| e.to_string())?;
            let removed = st.devices.remove(&pod).is_some();
            if removed {
                State::store(&this.state_dir, &st).map_err(|e| e.to_string())?;
            }
            let dir = this.state_dir.join("live").join(&pod);
            if dir.exists() {
                std::fs::remove_dir_all(&dir).map_err(|e| e.to_string())?;
            }
            Ok(())
        })
        .await
        .map_err(internal)?
        .map_err(internal)?;
        Ok(Response::new(pb::RemoveDeviceResponse {}))
    }

    async fn seal(&self, request: Request<pb::SealRequest>) -> Result<Response<pb::Layer>, Status> {
        let req = request.into_inner();
        if req.pod_id.is_empty() {
            return Err(Status::invalid_argument("pod_id required"));
        }
        if self
            .devices
            .lock()
            .map_err(|_| internal("poisoned"))?
            .contains_key(&req.pod_id)
        {
            return Err(Status::failed_precondition(
                "device still live; stop_disk first",
            ));
        }
        if req.upload && self.s3.is_none() {
            return Err(Status::failed_precondition("S3 not configured"));
        }
        let this = self.clone();
        let pod = req.pod_id.clone();
        let pod_log = pod.clone();
        let res = tokio::task::spawn_blocking(
            move || -> Result<disk_store::seal::SealOutcome, String> {
                let st = State::load(&this.state_dir).map_err(|e| e.to_string())?;
                let row = st
                    .devices
                    .get(&pod)
                    .ok_or_else(|| format!("no device row for {pod}"))?
                    .clone();
                let chain = this.load_chain(&row.chain)?;
                // Flatten-on-seal (ADR 0011): the chain has
                // the fixed shape <shared…, S, top>. Exactly one private
                // layer S may sit above the shared prefix; the seal
                // replaces it with a merged S' (parent = last shared).
                let depth = row.shared_depth.min(chain.len());
                if chain.len() > depth + 1 {
                    return Err(format!(
                        "row {} has {} private layers (chain shape invariant broken)",
                        pod,
                        chain.len() - depth
                    ));
                }
                let merged: Option<&disk_store::Manifest> = if chain.len() > depth {
                    chain.last()
                } else {
                    None
                };
                let parents: Vec<&disk_store::Manifest> = chain.iter().collect();
                let top = this.top_path(&pod);
                if !top.is_file() {
                    // Sealed already, by a seal whose caller lost the
                    // answer: the row names the newest layer, and the top
                    // goes only after it does (below).
                    let newest = row.chain.last().ok_or("empty chain")?;
                    let manifest = this.load_layer(newest)?;
                    return Ok(disk_store::seal::SealOutcome {
                        layer_id: manifest.layer_id(),
                        manifest_path: this.layer_path(newest),
                        manifest,
                        stats: Default::default(),
                    });
                }
                let outcome = disk_store::seal::seal_top(
                    &top,
                    &parents,
                    merged,
                    &this.cache,
                    &this.state_dir,
                    row.logical_size,
                    row.chunk_size,
                    None,
                )
                .map_err(|e| e.to_string())?;
                // Commit order: seal_top already committed chunks+manifest;
                // now point the row at shared… + S', and then delete the
                // top, which S' holds. A crash in between leaves a top
                // over S' that reads as S' does.
                let new_id = outcome.layer_id.hex();
                let old_private: Option<String> = if row.chain.len() > depth {
                    row.chain.last().cloned()
                } else {
                    None
                };
                let mut st2 = State::load(&this.state_dir).map_err(|e| e.to_string())?;
                let new_chain = {
                    let row2 = st2
                        .devices
                        .get_mut(&pod)
                        .ok_or_else(|| format!("no device row for {pod}"))?;
                    row2.chain.truncate(depth);
                    row2.chain.push(new_id.clone());
                    row2.dev_id = None;
                    row2.has_top = false;
                    row2.chain.clone()
                };
                State::store(&this.state_dir, &st2).map_err(|e| e.to_string())?;
                std::fs::remove_file(&top).map_err(|e| e.to_string())?;
                // head.json is committed after the row (proto contract);
                // existing memory refs survive a disk-only seal.
                let mut head = disk_store::head::PodHead::load(&this.state_dir, &pod)
                    .unwrap_or_else(|_| {
                        disk_store::head::PodHead::new(
                            &pod,
                            row.logical_size,
                            row.chunk_size,
                            new_chain.clone(),
                        )
                    });
                head.chain = new_chain;
                head.shared_depth = Some(depth.max(1));
                head.has_top = false;
                head.store(&this.state_dir).map_err(|e| e.to_string())?;
                // The replaced private layer is local garbage the moment
                // the row points at S' (GCP: S's exclusively-owned chunks
                // are fsck garbage; an S3 copy, if any, is the sweeper's
                // business). Same id (idempotent reseal) → keep.
                if let Some(old) = old_private
                    && old != new_id
                {
                    let _ = std::fs::remove_file(this.layer_path(&old));
                }
                Ok(outcome)
            },
        )
        .await
        .map_err(internal)?
        .map_err(internal)?;
        if req.upload {
            self.push_chain(&res.layer_id.hex())
                .await
                .map_err(|e| Status::internal(format!("seal uploaded but push failed: {e}")))?;
        }
        info!(pod = %pod_log, layer = %res.layer_id, upload = req.upload, "seal");
        Ok(Response::new(pb::Layer {
            layer_id: res.layer_id.hex(),
            entries: res.manifest.entries.len() as u64,
            chunks_new: res.stats.chunks_new,
            chunks_inherited: res.stats.chunks_inherited,
            chunks_zeroed: res.stats.chunks_zeroed,
            chunks_pool_hit: res.stats.chunks_pool_hit,
            bytes_new: res.stats.bytes_new,
            seal_seconds: res.stats.seal_seconds,
            logical_bytes: res.manifest.data_bytes(),
        }))
    }

    async fn bake(&self, request: Request<pb::BakeRequest>) -> Result<Response<pb::Layer>, Status> {
        let req = request.into_inner();
        if req.pod_id.is_empty() {
            return Err(Status::invalid_argument("pod_id required"));
        }
        // Live seal (ADR 0011): the device keeps serving.
        let shared = {
            self.devices
                .lock()
                .map_err(|_| internal("poisoned"))?
                .get(&req.pod_id)
                .map(|d| d.shared.clone())
        }
        .ok_or_else(|| Status::failed_precondition("device not live"))?;
        // Write guard (before): the guest MUST be frozen; a write during
        // the seal makes the layer torn.
        let writes_before = shared.stats.writes.load(Ordering::Relaxed);
        let this = self.clone();
        let pod = req.pod_id.clone();
        let pod_log = pod.clone();
        let res = tokio::task::spawn_blocking(
            move || -> Result<disk_store::seal::SealOutcome, String> {
                let st = State::load(&this.state_dir).map_err(|e| e.to_string())?;
                let row = st
                    .devices
                    .get(&pod)
                    .ok_or_else(|| format!("no device row for {pod}"))?
                    .clone();
                let chain = this.load_chain(&row.chain)?;
                let parents: Vec<&disk_store::Manifest> = chain.iter().collect();
                let top = this.top_path(&pod);
                if !top.is_file() {
                    return Err("no mutable top; nothing to bake".into());
                }
                disk_store::seal::seal_top(
                    &top,
                    &parents,
                    None, // never merge: the row chain is not mutated
                    &this.cache,
                    &this.state_dir,
                    row.logical_size,
                    row.chunk_size,
                    None,
                )
                .map_err(|e| e.to_string())
            },
        )
        .await
        .map_err(internal)?
        .map_err(internal)?;
        // Write guard (after): any write in between → torn layer; discard
        // the manifest (orphaned chunks are GC garbage) and fail.
        let writes_after = shared.stats.writes.load(Ordering::Relaxed);
        if writes_after != writes_before {
            let _ = std::fs::remove_file(&res.manifest_path);
            info!(pod = %pod_log, writes_before, writes_after, "bake rejected: guest not frozen");
            return Err(Status::failed_precondition(
                "writes during bake: guest not frozen",
            ));
        }
        if req.upload {
            self.push_chain(&res.layer_id.hex())
                .await
                .map_err(|e| Status::internal(format!("bake sealed but push failed: {e}")))?;
        }
        info!(pod = %pod_log, layer = %res.layer_id, upload = req.upload, "bake");
        Ok(Response::new(pb::Layer {
            layer_id: res.layer_id.hex(),
            entries: res.manifest.entries.len() as u64,
            chunks_new: res.stats.chunks_new,
            chunks_inherited: res.stats.chunks_inherited,
            chunks_zeroed: res.stats.chunks_zeroed,
            chunks_pool_hit: res.stats.chunks_pool_hit,
            bytes_new: res.stats.bytes_new,
            seal_seconds: res.stats.seal_seconds,
            logical_bytes: res.manifest.data_bytes(),
        }))
    }

    async fn seal_file(
        &self,
        request: Request<pb::SealFileRequest>,
    ) -> Result<Response<pb::Layer>, Status> {
        let req = request.into_inner();
        if req.path.is_empty() {
            return Err(Status::invalid_argument("path required"));
        }
        if req.upload && self.s3.is_none() {
            return Err(Status::failed_precondition("S3 not configured"));
        }
        let chunk_size = if req.chunk_size == 0 {
            disk_store::chunk::DEFAULT_MEM_CHUNK_SIZE
        } else {
            req.chunk_size
        };
        let this = self.clone();
        let path = req.path.clone();
        let res = tokio::task::spawn_blocking(move || {
            disk_store::seal::seal_file(
                std::path::Path::new(&path),
                &this.cache,
                &this.state_dir,
                chunk_size,
                None,
            )
            .map_err(|e| e.to_string())
        })
        .await
        .map_err(internal)?
        .map_err(internal)?;
        if req.upload {
            // Flat layer — no ancestors.
            self.push_layer(&res.layer_id.hex()).await.map_err(|e| {
                Status::internal(format!("seal_file uploaded but push failed: {e}"))
            })?;
        }
        info!(path = %req.path, layer = %res.layer_id, upload = req.upload, "seal_file");
        Ok(Response::new(pb::Layer {
            layer_id: res.layer_id.hex(),
            entries: res.manifest.entries.len() as u64,
            chunks_new: res.stats.chunks_new,
            chunks_inherited: res.stats.chunks_inherited,
            chunks_zeroed: res.stats.chunks_zeroed,
            chunks_pool_hit: res.stats.chunks_pool_hit,
            bytes_new: res.stats.bytes_new,
            seal_seconds: res.stats.seal_seconds,
            logical_bytes: res.manifest.data_bytes(),
        }))
    }

    async fn materialize(
        &self,
        request: Request<pb::MaterializeRequest>,
    ) -> Result<Response<pb::Empty>, Status> {
        let req = request.into_inner();
        let this = self.clone();
        // A restore on a node that has never seen this pod knows the
        // layer ids and nothing else — not even the manifests. Pull those
        // first (with their parents), or load_chain below fails with a
        // bare ENOENT on a layers/<id>.manifest path (the restoring
        // node starts cold by definition).
        if this.s3.is_some() {
            for id in &req.chain {
                if this.layer_path(id).is_file() {
                    continue;
                }
                this.ensure_manifests(id)
                    .await
                    .map_err(|e| internal(format!("manifest {id}: {e}")))?;
            }
        }
        // Hydrate next (concurrent GET): memory-restore and cross-node
        // materialize operate on chains whose chunks are S3-only.
        {
            let chain = this.load_chain(&req.chain).map_err(internal)?;
            if let Some(s3) = &this.s3 {
                let refs: Vec<&disk_store::Manifest> = chain.iter().collect();
                disk_store::s3::hydrate_chain(&refs, &this.cache, s3, this.s3_concurrency)
                    .await
                    .map_err(|e| internal(format!("hydrate: {e}")))?;
            }
        }
        tokio::task::spawn_blocking(move || {
            let chain = this.load_chain(&req.chain).map_err(|e| e.to_string())?;
            let refs: Vec<&disk_store::Manifest> = chain.iter().collect();
            disk_store::materialize::materialize(
                &refs,
                &this.cache,
                std::path::Path::new(&req.out_path),
            )
            .map_err(|e| e.to_string())
        })
        .await
        .map_err(internal)?
        .map_err(internal)?;
        Ok(Response::new(pb::Empty {}))
    }

    async fn push(&self, r: Request<pb::LayerRef>) -> Result<Response<pb::Empty>, Status> {
        let req = r.into_inner();
        if req.layer_id.is_empty() {
            return Err(Status::invalid_argument("layer_id required"));
        }
        if self.s3.is_none() {
            return Err(Status::failed_precondition("S3 not configured"));
        }
        self.push_layer(&req.layer_id)
            .await
            .map_err(Status::internal)?;
        Ok(Response::new(pb::Empty {}))
    }

    async fn pull(&self, r: Request<pb::LayerRef>) -> Result<Response<pb::Empty>, Status> {
        let req = r.into_inner();
        if req.layer_id.is_empty() {
            return Err(Status::invalid_argument("layer_id required"));
        }
        let s3 = self
            .s3
            .clone()
            .ok_or_else(|| Status::failed_precondition("S3 not configured"))?;
        let head_id = req.layer_id.clone();
        self.ensure_manifests(&req.layer_id)
            .await
            .map_err(Status::internal)?;
        if req.warm {
            set_sched_idle();
            // fetch every missing data chunk of the whole chain (ids
            // come out head-first; load_chain wants L0 first)
            let mut ids = Vec::new();
            let mut cur = Some(req.layer_id.clone());
            while let Some(id) = cur {
                let m = self.load_layer(&id).map_err(internal)?;
                cur = m.parent.map(|p| p.hex());
                ids.push(id);
            }
            ids.reverse();
            let chain = self.load_chain(&ids).map_err(internal)?;
            let refs: Vec<&disk_store::Manifest> = chain.iter().collect();
            let fetched =
                disk_store::s3::hydrate_chain(&refs, &self.cache, &s3, self.s3_concurrency)
                    .await
                    .map_err(|e| internal(format!("warm pull: {e}")))?;
            info!(layer = %head_id, bytes = fetched, "pull --warm");
        }
        info!(layer = %head_id, warm = req.warm, "pull");
        Ok(Response::new(pb::Empty {}))
    }

    async fn evict(
        &self,
        request: Request<pb::EvictRequest>,
    ) -> Result<Response<pb::EvictStats>, Status> {
        let req = request.into_inner();
        let target = match pb::evict_request::Target::try_from(req.target) {
            Ok(pb::evict_request::Target::All) => disk_store::cache::EvictTarget::All,
            _ => disk_store::cache::EvictTarget::Budget,
        };
        let keep = if req.force {
            std::collections::HashSet::new()
        } else {
            self.tenure_protected()
        };
        let st = self.cache.evict_skip(target, &keep).map_err(internal)?;
        Ok(Response::new(pb::EvictStats {
            removed_entries: st.removed_entries,
            freed_bytes: st.freed_bytes,
        }))
    }

    async fn stats(
        &self,
        request: Request<pb::StatsRequest>,
    ) -> Result<Response<pb::StatsResponse>, Status> {
        let req = request.into_inner();
        let mut out = pb::StatsResponse {
            devices: Vec::new(),
            cache_bytes: 0,
            cache_entries: 0,
            s3_puts: 0,
            s3_put_bytes: 0,
            s3_gets: 0,
            s3_get_bytes: 0,
            memories: Vec::new(),
            object_store: self.s3.is_some(),
            object_store_id: self
                .s3
                .as_ref()
                .map(|s3| s3.id().to_string())
                .unwrap_or_default(),
            version: env!("CARGO_PKG_VERSION").to_string(),
        };
        if let Ok(st) = self.cache.stats() {
            out.cache_bytes = st.bytes;
            out.cache_entries = st.entries;
        }
        if let Some(s3) = &self.s3 {
            out.s3_puts = s3.puts.load(Ordering::Relaxed);
            out.s3_put_bytes = s3.bytes_up.load(Ordering::Relaxed);
            out.s3_gets = s3.gets.load(Ordering::Relaxed);
            out.s3_get_bytes = s3.bytes_down.load(Ordering::Relaxed);
        }
        let devs = self.devices.lock().map_err(|_| internal("poisoned"))?;
        for (id, d) in devs.iter() {
            if !req.pod_id.is_empty() && req.pod_id != *id {
                continue;
            }
            let s = &d.shared.stats;
            let ld = |a: &std::sync::atomic::AtomicU64| a.load(Ordering::Relaxed);
            out.devices.push(pb::DeviceStats {
                pod_id: id.clone(),
                dev_path: d.bdev.display().to_string(),
                reads: ld(&s.reads),
                writes: ld(&s.writes),
                flushes: ld(&s.flushes),
                discards: ld(&s.discards),
                read_bytes: ld(&s.read_bytes),
                write_bytes: ld(&s.write_bytes),
                cache_hits: ld(&s.cache_hits),
                cache_misses: ld(&s.cache_misses),
                s3_fetches: ld(&s.s3_fetches),
                s3_fetch_bytes: ld(&s.s3_fetch_bytes),
                errors: ld(&s.errors),
                histograms: vec![
                    pb::Histogram {
                        name: "read".into(),
                        buckets: s.read_hist.snapshot(),
                        p50_us: s.read_hist.percentile(0.5),
                        p99_us: s.read_hist.percentile(0.99),
                    },
                    pb::Histogram {
                        name: "flush".into(),
                        buckets: s.flush_hist.snapshot(),
                        p50_us: s.flush_hist.percentile(0.5),
                        p99_us: s.flush_hist.percentile(0.99),
                    },
                ],
            });
        }
        drop(devs);
        let mems = self.memories.lock().map_err(|_| internal("poisoned"))?;
        for (id, m) in mems.iter() {
            if !req.pod_id.is_empty() && req.pod_id != *id {
                continue;
            }
            let s = &m.stats;
            let ld = |a: &std::sync::atomic::AtomicU64| a.load(Ordering::Relaxed);
            out.memories.push(pb::MemoryStats {
                pod_id: id.clone(),
                attached: s.attached.load(Ordering::Relaxed),
                faults: ld(&s.faults),
                chunks_served: ld(&s.chunks_served),
                chunks_zeroed: ld(&s.chunks_zeroed),
                bytes_served: ld(&s.bytes_served),
                cache_hits: ld(&s.cache_hits),
                cache_misses: ld(&s.cache_misses),
                s3_fetches: ld(&s.s3_fetches),
                s3_fetch_bytes: ld(&s.s3_fetch_bytes),
                removes: ld(&s.removes),
                pages_reclaimed: ld(&s.pages_reclaimed),
                errors: ld(&s.errors),
                chunks_total: ld(&s.chunks_total),
            });
        }
        Ok(Response::new(out))
    }

    async fn fsck(
        &self,
        request: Request<pb::FsckRequest>,
    ) -> Result<Response<pb::FsckReport>, Status> {
        let check_s3 = request.into_inner().check_s3;
        let this = self.clone();

        // --check-s3: rebuild the upload markers from S3 truth. Probe
        // every chunk referenced by any local manifest at bounded
        // concurrency; afterwards a marker exists ⟺ the object exists.
        let mut remote: std::collections::HashSet<disk_store::chunk::Hash> =
            std::collections::HashSet::new();
        if check_s3 {
            let s3 = this
                .s3
                .clone()
                .ok_or_else(|| Status::failed_precondition("S3 not configured"))?;
            let layers = load_local_layers(&this.state_dir).map_err(internal)?;
            let mut probes: Vec<disk_store::chunk::Hash> = Vec::new();
            for (_, m) in &layers {
                for e in &m.entries {
                    if e.kind == disk_store::manifest::EntryKind::Data {
                        probes.push(disk_store::chunk::Hash::from(e.hash));
                    }
                }
            }
            let probes: Vec<disk_store::chunk::Hash> = probes
                .into_iter()
                .collect::<std::collections::HashSet<_>>()
                .into_iter()
                .collect();
            let found: Vec<Option<disk_store::chunk::Hash>> =
                futures::stream::iter(probes.iter().copied())
                    .map(|h| {
                        let s3 = s3.clone();
                        async move { s3.has_chunk(&h).await.then_some(h) }
                    })
                    .buffer_unordered(this.s3_concurrency.max(1))
                    .collect()
                    .await;
            for h in found.into_iter().flatten() {
                remote.insert(h);
            }
            for h in &probes {
                if remote.contains(h) {
                    this.mark_chunk_uploaded(h);
                } else {
                    this.unmark_chunk(h);
                }
            }
        }

        let report = tokio::task::spawn_blocking(move || -> Result<_, String> {
            let layers = load_local_layers(&this.state_dir)?;
            let s3_has = |h: &disk_store::chunk::Hash| remote.contains(h);
            disk_store::gc::fsck(
                &this.state_dir,
                &layers,
                if check_s3 { Some(&s3_has) } else { None },
            )
            .map_err(|e| e.to_string())
        })
        .await
        .map_err(internal)?
        .map_err(internal)?;
        Ok(Response::new(pb::FsckReport {
            complete_layers: report.complete_layers,
            dangling_layers: report
                .dangling_layers
                .iter()
                .map(|d| d.layer_id.clone())
                .collect(),
            garbage_chunks: report.garbage_chunks,
            garbage_bytes: report.garbage_bytes,
        }))
    }

    // Object-store GC (ADR 0011). The caller owns the refcounts, so
    // the live set arrives as an argument; diskd owns the bucket, so
    // the walking and the deleting happen here. Three passes, each over
    // one prefix, each only ever deleting objects that are unreachable
    // AND past the grace — a sweep that stops halfway (budget, error)
    // leaves the store consistent.
    async fn sweep(
        &self,
        request: Request<pb::SweepRequest>,
    ) -> Result<Response<pb::SweepReport>, Status> {
        let req = request.into_inner();
        let s3 = self
            .s3
            .clone()
            .ok_or_else(|| Status::failed_precondition("S3 not configured"))?;
        let grace = match (req.no_grace, req.grace_seconds) {
            (true, _) => 0, // labs only; see SweepRequest.no_grace
            (false, 0) => DEFAULT_SWEEP_GRACE_SECONDS,
            (false, n) => n as i64,
        };
        let budget = std::time::Duration::from_secs(if req.budget_seconds == 0 {
            DEFAULT_SWEEP_BUDGET_SECONDS
        } else {
            req.budget_seconds
        });
        let started = std::time::Instant::now();
        let now = std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .map(|d| d.as_secs() as i64)
            .unwrap_or_default();

        let mut rep = pb::SweepReport::default();
        let delete_all = |keys: Vec<String>| {
            let s3 = s3.clone();
            let conc = self.s3_concurrency.max(1);
            async move {
                if req.dry_run {
                    return Ok::<(), String>(());
                }
                let errs: Vec<String> = futures::stream::iter(keys)
                    .map(|k| {
                        let s3 = s3.clone();
                        async move {
                            s3.delete_object_key(&k)
                                .await
                                .err()
                                .map(|e| format!("{k}: {e}"))
                        }
                    })
                    .buffer_unordered(conc)
                    .collect::<Vec<_>>()
                    .await
                    .into_iter()
                    .flatten()
                    .collect();
                if let Some(first) = errs.first() {
                    return Err(format!("{} delete(s) failed, first: {first}", errs.len()));
                }
                Ok(())
            }
        };

        // --- pass 1: snapshots/ ------------------------------------
        let snaps = s3.list("snapshots/").await.map_err(internal)?;
        rep.snapshot_objects_seen = snaps.len() as u64;
        let mut live_heads: std::collections::HashSet<String> =
            req.live_heads.iter().cloned().collect();
        // A live head keeps the heads of its pod's volumes (ADR 0016).
        for key in &req.live_heads {
            let buf = match s3.find_object(key).await {
                Ok(Some(buf)) => buf,
                // A head that is gone names nothing; pass 1 sees it.
                Ok(None) => continue,
                Err(e) => return Err(internal(format!("sweep: live head {key}: {e}"))),
            };
            let head = disk_store::head::PodHead::decode(&buf)
                .map_err(|e| internal(format!("sweep: live head {key}: {e}")))?;
            live_heads.extend(head.volumes);
        }
        let plan = disk_store::sweep::plan_snapshots(&snaps, &live_heads, now, grace);
        rep.snapshot_objects_deleted = plan.dead_snapshot_keys.len() as u64;
        rep.bytes_deleted += plan.dead_bytes;
        rep.deleted_keys
            .extend(plan.dead_snapshot_keys.iter().cloned());
        delete_all(plan.dead_snapshot_keys.clone())
            .await
            .map_err(internal)?;

        // Every surviving head keeps its disk chain and its memory
        // layer alive; walk the manifests' parent pointers too, because
        // a chain entry's parent is reachable data even when the head
        // does not spell it out.
        let mut live_layers: std::collections::HashSet<String> =
            req.live_layers.iter().cloned().collect();
        for key in &plan.live_head_keys {
            let buf = match s3.get_object(key).await {
                Ok(b) => b,
                Err(e) => {
                    // A head we cannot read is a head we must not
                    // garbage-collect around: abort rather than delete
                    // layers it might reference.
                    return Err(internal(format!("sweep: live head {key}: {e}")));
                }
            };
            let head = disk_store::head::PodHead::decode(&buf)
                .map_err(|e| internal(format!("sweep: live head {key}: {e}")))?;
            live_layers.extend(head.chain.iter().cloned());
            if let Some(m) = &head.memory {
                live_layers.insert(m.layer.clone());
            }
        }
        for id in self.expand_parents(&live_layers, &s3).await {
            live_layers.insert(id);
        }
        if started.elapsed() >= budget {
            rep.budget_exhausted = true;
            return Ok(Response::new(rep));
        }

        // --- pass 2: layers/ ---------------------------------------
        let layer_objs = s3.list("layers/").await.map_err(internal)?;
        rep.layers_seen = layer_objs.len() as u64;
        let (dead_layers, bytes) =
            disk_store::sweep::plan_layers(&layer_objs, &live_layers, now, grace);
        rep.layers_deleted = dead_layers.len() as u64;
        rep.bytes_deleted += bytes;
        rep.deleted_keys.extend(dead_layers.iter().cloned());
        delete_all(dead_layers.clone()).await.map_err(internal)?;
        if started.elapsed() >= budget {
            rep.budget_exhausted = true;
            return Ok(Response::new(rep));
        }

        // --- pass 3: chunks/ ---------------------------------------
        // Referenced = the entry tables of every manifest that survived
        // pass 2 (not just the live set: a manifest we chose to keep
        // because it is inside the grace owns its chunks too).
        let dead_layer_ids: std::collections::HashSet<&str> = dead_layers
            .iter()
            .filter_map(|k| disk_store::sweep::layer_id_of_key(k))
            .collect();
        let surviving: Vec<String> = layer_objs
            .iter()
            .filter_map(|o| disk_store::sweep::layer_id_of_key(&o.key))
            .filter(|id| !dead_layer_ids.contains(id))
            .map(|id| id.to_string())
            .collect();
        let mut referenced: std::collections::HashSet<disk_store::chunk::Hash> =
            std::collections::HashSet::new();
        for id in &surviving {
            let m = self.manifest_for_sweep(id, &s3).await.map_err(internal)?;
            for e in &m.entries {
                if e.kind == disk_store::manifest::EntryKind::Data {
                    referenced.insert(disk_store::chunk::Hash::from(e.hash));
                }
            }
        }
        let chunk_objs = s3.list("chunks/").await.map_err(internal)?;
        rep.chunks_seen = chunk_objs.len() as u64;
        let (dead_chunks, bytes) =
            disk_store::sweep::plan_chunks(&chunk_objs, &referenced, now, grace);
        rep.chunks_deleted = dead_chunks.len() as u64;
        rep.bytes_deleted += bytes;
        rep.deleted_keys.extend(dead_chunks.iter().cloned());
        delete_all(dead_chunks.clone()).await.map_err(internal)?;
        if !req.dry_run {
            // The upload markers are a claim about S3; a chunk we just
            // removed from S3 must lose its marker or the next Push
            // would skip a PUT the bucket needs (the stale-marker trap).
            for k in &dead_chunks {
                if let Some(hex) = disk_store::sweep::chunk_hex_of_key(k)
                    && let Ok(h) = disk_store::chunk::Hash::from_hex(hex)
                {
                    self.unmark_chunk(&h);
                }
            }
        }
        info!(
            snapshots = rep.snapshot_objects_deleted,
            layers = rep.layers_deleted,
            chunks = rep.chunks_deleted,
            bytes = rep.bytes_deleted,
            dry_run = req.dry_run,
            seconds = started.elapsed().as_secs_f64(),
            "sweep"
        );
        Ok(Response::new(rep))
    }

    // Generic object gateway (the caller writes and reads vmstate and
    // head.json through here: all S3 traffic lives in diskd).
    /// Bind a Firecracker UFFD backend socket for this pod's guest
    /// RAM. Returns once the socket exists: the caller's
    /// `PUT /snapshot/load` is what makes Firecracker connect to it, and
    /// the VM is running long before the first chunk is on the wire.
    ///
    /// The memory layer is flat (`SealFile`: no parents), so "resolve the
    /// chain" is one manifest — but it is fetched, cached and verified by
    /// exactly the code the ublk device reads through.
    async fn open_memory(
        &self,
        request: Request<pb::OpenMemoryRequest>,
    ) -> Result<Response<pb::Memory>, Status> {
        let req = request.into_inner();
        if req.pod_id.is_empty() || req.layer.is_empty() || req.socket_path.is_empty() {
            return Err(Status::invalid_argument(
                "pod_id, layer and socket_path are required",
            ));
        }
        if self.s3.is_none() {
            return Err(Status::failed_precondition("S3 not configured"));
        }
        // Cold node by definition: pull the manifest before anything
        // else can ask it a question (same walk as Materialize's).
        if !self.layer_path(&req.layer).is_file() {
            self.ensure_manifests(&req.layer)
                .await
                .map_err(|e| internal(format!("memory manifest {}: {e}", req.layer)))?;
        }
        let manifest = self.load_layer(&req.layer).map_err(internal)?;
        let logical_size = manifest.logical_size;
        let chunk_size = manifest.chunk_size;
        let stats = Arc::new(MemStats::default());
        let started = uffd::serve(MemoryCtx {
            pod_id: req.pod_id.clone(),
            socket: PathBuf::from(&req.socket_path),
            manifest,
            cache: self.cache.clone(),
            readahead: self.mem_readahead,
            fetch_concurrency: self.s3_concurrency,
            stats: stats.clone(),
        })
        .map_err(internal)?;
        let chunks_total = stats.chunks_total.load(Ordering::Relaxed);
        let previous = {
            let mut t = self.memories.lock().map_err(|_| internal("poisoned"))?;
            t.insert(req.pod_id.clone(), started)
        };
        if let Some(old) = previous {
            tokio::task::spawn_blocking(move || uffd::stop(old))
                .await
                .map_err(internal)?;
        }
        info!(pod = %req.pod_id, layer = %req.layer, socket = %req.socket_path,
              chunks = chunks_total, "open_memory: uffd backend armed");
        Ok(Response::new(pb::Memory {
            pod_id: req.pod_id,
            socket_path: req.socket_path,
            logical_size,
            chunk_size,
            chunks_total,
        }))
    }

    /// Idempotent: the handler also lets go by itself when Firecracker
    /// closes the backend socket, so a pause/destroy that races the VMM's
    /// exit finds nothing here and that is not an error.
    async fn close_memory(
        &self,
        request: Request<pb::CloseMemoryRequest>,
    ) -> Result<Response<pb::Empty>, Status> {
        let req = request.into_inner();
        let started = {
            let mut t = self.memories.lock().map_err(|_| internal("poisoned"))?;
            t.remove(&req.pod_id)
        };
        if let Some(m) = started {
            tokio::task::spawn_blocking(move || uffd::stop(m))
                .await
                .map_err(internal)?;
            info!(pod = %req.pod_id, "close_memory: uffd backend detached");
        }
        Ok(Response::new(pb::Empty {}))
    }

    async fn put_object(
        &self,
        request: Request<pb::PutObjectRequest>,
    ) -> Result<Response<pb::Empty>, Status> {
        let req = request.into_inner();
        let s3 = self
            .s3
            .clone()
            .ok_or_else(|| Status::failed_precondition("S3 not configured"))?;
        let data = std::fs::read(&req.path)
            .map_err(|e| Status::invalid_argument(format!("read {}: {e}", req.path)))?;
        s3.put_object(&req.key, data).await.map_err(internal)?;
        Ok(Response::new(pb::Empty {}))
    }

    async fn get_object(
        &self,
        request: Request<pb::GetObjectRequest>,
    ) -> Result<Response<pb::Empty>, Status> {
        let req = request.into_inner();
        let s3 = self
            .s3
            .clone()
            .ok_or_else(|| Status::failed_precondition("S3 not configured"))?;
        let data = s3.get_object(&req.key).await.map_err(internal)?;
        let tmp = format!("{}.tmp-{}", req.path, std::process::id());
        std::fs::write(&tmp, &data)
            .and_then(|_| std::fs::rename(&tmp, &req.path))
            .map_err(|e| Status::internal(format!("write {}: {e}", req.path)))?;
        Ok(Response::new(pb::Empty {}))
    }

    // The head is a checkpoint's last object (ADR 0011's commit order):
    // it names the disk's chain as this node's row has it after a seal,
    // and the memory the caller sealed and put beside it. Every layer it
    // names must be in the bucket before it is, so that a head in the
    // bucket is always whole.
    async fn put_head(
        &self,
        request: Request<pb::PutHeadRequest>,
    ) -> Result<Response<pb::Head>, Status> {
        let req = request.into_inner();
        let s3 = self
            .s3
            .clone()
            .ok_or_else(|| Status::failed_precondition("S3 not configured"))?;
        if req.pod_id.is_empty() || req.epoch == 0 {
            return Err(Status::invalid_argument("pod_id and an epoch are required"));
        }
        if self
            .devices
            .lock()
            .map_err(|_| internal("poisoned"))?
            .contains_key(&req.pod_id)
        {
            return Err(Status::failed_precondition(
                "device still live; stop and seal it first",
            ));
        }
        let row = State::load(&self.state_dir)
            .map_err(internal)?
            .devices
            .get(&req.pod_id)
            .cloned()
            .ok_or_else(|| Status::not_found(format!("no disk of pod {}", req.pod_id)))?;
        if self.top_path(&req.pod_id).is_file() {
            return Err(Status::failed_precondition(
                "the disk has writes that are not sealed; seal it first",
            ));
        }
        let mut head = disk_store::head::PodHead::new(
            &req.pod_id,
            row.logical_size,
            row.chunk_size,
            row.chain.clone(),
        );
        head.shared_depth = Some(row.shared_depth.clamp(1, row.chain.len().max(1)));
        head.epoch = req.epoch;
        head.memory = req.memory.map(|m| disk_store::head::MemoryRef {
            layer: m.layer,
            vmstate_key: m.vmstate_key,
            epoch: req.epoch,
            fc_version: m.firecracker_version,
            cpu_model: m.cpu_model,
            kernel_release: m.kernel_release,
        });
        head.volumes = req.volume_heads;
        head.validate()
            .map_err(|e| Status::invalid_argument(e.to_string()))?;
        let own = format!("snapshots/{}/", req.pod_id);
        if let Some(key) = head.volumes.iter().find(|key| key.starts_with(&own)) {
            return Err(Status::invalid_argument(format!(
                "{key} is a head of pod {}'s own disk, not of a volume",
                req.pod_id
            )));
        }

        let mut layers = head.chain.clone();
        layers.extend(head.memory.as_ref().map(|m| m.layer.clone()));
        for id in &layers {
            let lid = disk_store::manifest::LayerId::from_hex(id)
                .map_err(|e| Status::invalid_argument(format!("layer {id}: {e}")))?;
            if !s3.has_manifest(&lid).await {
                return Err(Status::failed_precondition(format!(
                    "layer {id} is not in the object store; push it first"
                )));
            }
        }
        if let Some(m) = &head.memory
            && !s3.has_object(&m.vmstate_key).await
        {
            return Err(Status::failed_precondition(format!(
                "{} is not in the object store",
                m.vmstate_key
            )));
        }
        for key in &head.volumes {
            if !s3.has_object(key).await {
                return Err(Status::failed_precondition(format!(
                    "the volume's head {key} is not in the object store; put it first"
                )));
            }
        }
        let key = disk_store::head::head_key(&req.pod_id, req.epoch);
        if s3.has_object(&key).await {
            return Err(Status::already_exists(format!(
                "{key} exists: a head is never overwritten"
            )));
        }
        head.stamp();
        let buf = head.encode().map_err(internal)?;
        s3.put_object(&key, buf).await.map_err(internal)?;
        info!(
            pod = %req.pod_id,
            key = %key,
            memory = head.memory.is_some(),
            volumes = head.volumes.len(),
            "put_head"
        );
        Ok(Response::new(head_message(key, head)))
    }

    async fn get_head(
        &self,
        request: Request<pb::GetHeadRequest>,
    ) -> Result<Response<pb::Head>, Status> {
        let key = request.into_inner().key;
        let s3 = self
            .s3
            .clone()
            .ok_or_else(|| Status::failed_precondition("S3 not configured"))?;
        let buf = s3
            .find_object(&key)
            .await
            .map_err(|e| internal(format!("head {key}: {e}")))?
            .ok_or_else(|| Status::not_found(format!("no head {key}")))?;
        let head = disk_store::head::PodHead::decode(&buf)
            .map_err(|e| Status::invalid_argument(format!("head {key}: {e}")))?;
        Ok(Response::new(head_message(key, head)))
    }
}

/// A head as the API shows it.
fn head_message(key: String, head: disk_store::head::PodHead) -> pb::Head {
    pb::Head {
        key,
        epoch: head
            .memory
            .as_ref()
            .map_or(head.epoch, |m| head.epoch.max(m.epoch)),
        pod_id: head.pod_id,
        chain: head.chain,
        logical_size: head.logical_size,
        memory: head.memory.map(|m| pb::MemoryRef {
            layer: m.layer,
            vmstate_key: m.vmstate_key,
            firecracker_version: m.fc_version,
            cpu_model: m.cpu_model,
            kernel_release: m.kernel_release,
        }),
        volume_heads: head.volumes,
    }
}

fn load_local_layers(
    state_dir: &std::path::Path,
) -> Result<Vec<(String, disk_store::Manifest)>, String> {
    let dir = state_dir.join("layers");
    let mut layers = Vec::new();
    if dir.is_dir() {
        for e in std::fs::read_dir(&dir).map_err(|e| e.to_string())? {
            let e = e.map_err(|e| e.to_string())?;
            let name = e.file_name().to_string_lossy().to_string();
            if !name.ends_with(".manifest") {
                continue;
            }
            let id = name.trim_end_matches(".manifest").to_string();
            let m =
                disk_store::Manifest::decode(&std::fs::read(e.path()).map_err(|x| x.to_string())?)
                    .map_err(|x| x.to_string())?;
            layers.push((id, m));
        }
    }
    Ok(layers)
}
