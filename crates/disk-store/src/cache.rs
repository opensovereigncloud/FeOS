// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! Local raw chunk cache + budget actor (ADR 0011).
//!
//! Chunks are content-addressed immutable raw files under
//! `<root>/cache/<hh>/<hash>`. The filesystem *is* the index: admission is
//! write to `staging/` + fsync + `rename` (idempotent; racing admitters
//! of the same chunk are benign). The only shared mutable state — LRU
//! accounting and byte budget — lives in a single actor thread fed by an
//! mpsc channel. Eviction is `unlink`; readers holding an fd are
//! unaffected.

use std::fs;
use std::io;
use std::path::{Path, PathBuf};
use std::sync::mpsc;
use std::thread::JoinHandle;

use linked_hash_map::LinkedHashMap;
use tracing::{debug, instrument};

use crate::chunk::{self, Hash};
use crate::error::{Error, Result};

#[derive(Debug, Clone, Copy, Default)]
pub struct CacheStats {
    pub entries: u64,
    pub bytes: u64,
    pub evicted_entries: u64,
    pub evicted_bytes: u64,
}

#[derive(Debug, Clone, Copy, Default)]
pub struct EvictStats {
    pub removed_entries: u64,
    pub freed_bytes: u64,
}

/// What `evict` should drive the cache down to.
#[derive(Debug, Clone, Copy)]
pub enum EvictTarget {
    /// Drop every reconstructible byte.
    All,
    /// Drop until within the configured budget (no-op if already inside).
    Budget,
}

enum Msg {
    Admit {
        hash: Hash,
        len: u64,
    },
    Touch {
        hash: Hash,
    },
    Remove {
        hash: Hash,
    },
    Evict {
        target: EvictTarget,
        keep: std::collections::HashSet<Hash>,
        reply: mpsc::Sender<EvictStats>,
    },
    Stats {
        reply: mpsc::Sender<CacheStats>,
    },
    Shutdown,
}

/// Where a cache miss can be satisfied from once the local copy is gone.
///
/// The cache is reconstructible by construction: every chunk in it that
/// has been pushed also exists in the object store. Nothing at this layer
/// knew that, so an evicted chunk was simply missing — and the next
/// flatten-on-seal that needed it as a parent failed outright, which is a
/// hibernation that never happens because the budget did its job.
pub type Refill = std::sync::Arc<dyn Fn(&Hash, u32) -> Result<Vec<u8>> + Send + Sync>;

pub struct Cache {
    root: PathBuf, // state dir; chunks under root/cache, staging under root/staging
    budget: u64,
    tx: mpsc::Sender<Msg>,
    actor: Option<JoinHandle<CacheStats>>,
    refill: Option<Refill>,
}

impl Cache {
    /// Open (or create) the cache under `root`. Existing chunk files are
    /// adopted into the LRU ordered by mtime; leftover staging files are
    /// removed (an interrupted admission leaves a temp file, never a
    /// partial chunk).
    pub fn open(root: &Path, budget_bytes: u64) -> Result<Cache> {
        let cache_dir = root.join("cache");
        let staging = root.join("staging");
        fs::create_dir_all(&cache_dir)?;
        fs::create_dir_all(&staging)?;

        // Clean interrupted admissions.
        for e in fs::read_dir(&staging)? {
            let e = e?;
            if e.file_type()?.is_file() {
                debug!(path = %e.path().display(), "removing stale staging file");
                fs::remove_file(e.path())?;
            }
        }

        let mut lru: LinkedHashMap<Hash, u64> = LinkedHashMap::new();
        let mut total = 0u64;
        for (hash, len, mtime_rank) in scan_chunks(&cache_dir)? {
            // Insert in ascending mtime so the LRU order approximates the
            // pre-restart access history.
            let _ = mtime_rank;
            total += len;
            lru.insert(hash, len);
        }

        let (tx, rx) = mpsc::channel::<Msg>();
        let actor_root = cache_dir.clone();
        let actor = std::thread::Builder::new()
            .name("cache-budget".into())
            .spawn(move || actor_loop(actor_root, budget_bytes, rx, lru, total))
            .map_err(Error::Io)?;

        Ok(Cache {
            root: root.to_path_buf(),
            budget: budget_bytes,
            tx,
            actor: Some(actor),
            refill: None,
        })
    }

    /// Give the cache a source to refill misses from (diskd wires S3 in;
    /// tests leave it unset and keep the old "missing is missing").
    pub fn with_refill(mut self, refill: Refill) -> Self {
        self.refill = Some(refill);
        self
    }

    /// Make sure `hash` is on disk, fetching it if a source is configured.
    /// Idempotent and safe to call concurrently (admit is a rename).
    pub fn ensure_local(&self, hash: &Hash, raw_len: u32) -> Result<()> {
        if self.contains(hash) {
            self.touch(hash);
            return Ok(());
        }
        let Some(refill) = &self.refill else {
            return Err(Error::ChunkMissing(hash.to_hex().to_string()));
        };
        let raw = refill(hash, raw_len)?;
        self.admit(hash, &raw)
    }

    pub fn budget(&self) -> u64 {
        self.budget
    }

    pub fn dir(&self) -> PathBuf {
        self.root.join("cache")
    }

    pub fn staging_dir(&self) -> PathBuf {
        self.root.join("staging")
    }

    pub fn chunk_path(&self, hash: &Hash) -> PathBuf {
        self.dir().join(chunk::cache_rel_path(hash))
    }

    /// Filesystem truth — does the chunk file exist right now?
    pub fn contains(&self, hash: &Hash) -> bool {
        self.chunk_path(hash).is_file()
    }

    /// Admit a raw chunk: stage, fsync, rename. Idempotent.
    #[instrument(skip(self, bytes), fields(len = bytes.len()))]
    pub fn admit(&self, hash: &Hash, bytes: &[u8]) -> Result<()> {
        let dst = self.chunk_path(hash);
        if dst.is_file() {
            // Already admitted (possibly by a racing thread); LRU touch only.
            let _ = self.tx.send(Msg::Touch { hash: *hash });
            return Ok(());
        }
        if let Some(parent) = dst.parent() {
            fs::create_dir_all(parent)?;
        }
        let tmp = self.staging_dir().join(format!("{}.tmp", hash.to_hex()));
        fs::write(&tmp, bytes)?;
        fs::File::open(&tmp)?.sync_all()?;
        fs::rename(&tmp, &dst)?;
        fsync_dir(dst.parent().unwrap_or_else(|| Path::new("/")))?;
        let _ = self.tx.send(Msg::Admit {
            hash: *hash,
            len: bytes.len() as u64,
        });
        Ok(())
    }

    /// Batch admission for seal: stage + fsync each chunk file, rename all,
    /// then fsync each touched `cache/<hh>/` subdir ONCE (instead of after
    /// every rename). Correct for the seal commit rule as long as the caller
    /// does not reference the chunks from a manifest before this returns.
    /// Batch admission for seal: stage + rename each chunk, then ONE
    /// `syncfs` of the volume. Durability equivalent to fsync-per-chunk
    /// (syncfs flushes all dirty data + metadata on the filesystem), at a
    /// fraction of the cost — per-file fsync is a journal commit each and
    /// dominated seal time on small chunks (measured: 47 s → 1.5 s for a
    /// 256 MiB base at 64 KiB chunks on loop-XFS).
    /// Correct for the seal commit rule as long as the caller does not
    /// reference the chunks from a manifest before this returns.
    pub fn admit_batch(&self, chunks: &[(Hash, Vec<u8>)]) -> Result<()> {
        let mut wrote = false;
        for (hash, bytes) in chunks {
            let dst = self.chunk_path(hash);
            if dst.is_file() {
                let _ = self.tx.send(Msg::Touch { hash: *hash });
                continue;
            }
            if let Some(parent) = dst.parent() {
                fs::create_dir_all(parent)?;
            }
            let tmp = self.staging_dir().join(format!("{}.tmp", hash.to_hex()));
            fs::write(&tmp, bytes)?;
            fs::rename(&tmp, &dst)?;
            wrote = true;
            let _ = self.tx.send(Msg::Admit {
                hash: *hash,
                len: bytes.len() as u64,
            });
        }
        if wrote {
            syncfs(&self.dir())?;
        }
        Ok(())
    }

    /// Note an access (read hit) for LRU purposes.
    pub fn touch(&self, hash: &Hash) {
        let _ = self.tx.send(Msg::Touch { hash: *hash });
    }

    /// Drop a specific chunk from the LRU knowledge (used by GC after it
    /// unlinked the file).
    pub fn forget(&self, hash: &Hash) {
        let _ = self.tx.send(Msg::Remove { hash: *hash });
    }

    pub fn evict(&self, target: EvictTarget) -> Result<EvictStats> {
        self.evict_skip(target, &std::collections::HashSet::new())
    }

    /// Evict like [`Self::evict`], but chunks in `keep` are never removed
    /// (chunks of un-pushed layers and live devices are tenure-protected).
    pub fn evict_skip(
        &self,
        target: EvictTarget,
        keep: &std::collections::HashSet<Hash>,
    ) -> Result<EvictStats> {
        let (rtx, rrx) = mpsc::channel();
        self.tx
            .send(Msg::Evict {
                target,
                keep: keep.clone(),
                reply: rtx,
            })
            .map_err(|_| Error::Head("budget actor gone".into()))?;
        rrx.recv()
            .map_err(|_| Error::Head("budget actor gone".into()))
    }

    pub fn stats(&self) -> Result<CacheStats> {
        let (rtx, rrx) = mpsc::channel();
        self.tx
            .send(Msg::Stats { reply: rtx })
            .map_err(|_| Error::Head("budget actor gone".into()))?;
        rrx.recv()
            .map_err(|_| Error::Head("budget actor gone".into()))
    }
}

impl Drop for Cache {
    fn drop(&mut self) {
        let _ = self.tx.send(Msg::Shutdown);
        if let Some(h) = self.actor.take() {
            let _ = h.join();
        }
    }
}

fn actor_loop(
    cache_dir: PathBuf,
    budget: u64,
    rx: mpsc::Receiver<Msg>,
    mut lru: LinkedHashMap<Hash, u64>,
    mut bytes: u64,
) -> CacheStats {
    let mut evicted_entries = 0u64;
    let mut evicted_bytes = 0u64;

    fn drop_oldest(
        cache_dir: &Path,
        lru: &mut LinkedHashMap<Hash, u64>,
        bytes: &mut u64,
        keep: &std::collections::HashSet<Hash>,
    ) -> Option<u64> {
        // Skip over protected (protenured) entries without evicting them.
        if keep.is_empty() {
            let (h, len) = lru.pop_front()?;
            let p = cache_dir.join(chunk::cache_rel_path(&h));
            if let Err(e) = fs::remove_file(&p)
                && e.kind() != io::ErrorKind::NotFound
            {
                debug!(hash = %h.to_hex(), error = %e, "evict unlink failed");
            }
            *bytes = bytes.saturating_sub(len);
            return Some(len);
        }
        let mut skipped: Vec<(Hash, u64)> = Vec::new();
        let mut next: Option<(Hash, u64)> = None;
        while let Some((h, len)) = lru.pop_front() {
            if keep.contains(&h) {
                skipped.push((h, len));
            } else {
                next = Some((h, len));
                break;
            }
        }
        lru.extend(skipped);
        let (h, len) = next?;
        let p = cache_dir.join(chunk::cache_rel_path(&h));
        if let Err(e) = fs::remove_file(&p)
            && e.kind() != io::ErrorKind::NotFound
        {
            debug!(hash = %h.to_hex(), error = %e, "evict unlink failed");
        }
        *bytes = bytes.saturating_sub(len);
        Some(len)
    }

    while let Ok(msg) = rx.recv() {
        match msg {
            Msg::Admit { hash, len } => {
                if lru.contains_key(&hash) {
                    lru.remove(&hash);
                    bytes = bytes.saturating_sub(len);
                }
                lru.insert(hash, len);
                bytes += len;
                while bytes > budget {
                    match drop_oldest(
                        &cache_dir,
                        &mut lru,
                        &mut bytes,
                        &std::collections::HashSet::new(),
                    ) {
                        Some(l) => {
                            evicted_entries += 1;
                            evicted_bytes += l;
                        }
                        None => break,
                    }
                }
            }
            Msg::Touch { hash } => {
                if let Some(len) = lru.remove(&hash) {
                    lru.insert(hash, len);
                }
            }
            Msg::Remove { hash } => {
                if let Some(len) = lru.remove(&hash) {
                    bytes = bytes.saturating_sub(len);
                }
            }
            Msg::Evict {
                target,
                keep,
                reply,
            } => {
                let goal = match target {
                    EvictTarget::All => 0,
                    EvictTarget::Budget => budget,
                };
                let before_e = evicted_entries;
                let before_b = evicted_bytes;
                while bytes > goal {
                    match drop_oldest(&cache_dir, &mut lru, &mut bytes, &keep) {
                        Some(l) => {
                            evicted_entries += 1;
                            evicted_bytes += l;
                        }
                        None => break,
                    }
                }
                let _ = reply.send(EvictStats {
                    removed_entries: evicted_entries - before_e,
                    freed_bytes: evicted_bytes - before_b,
                });
            }
            Msg::Stats { reply } => {
                let _ = reply.send(CacheStats {
                    entries: lru.len() as u64,
                    bytes,
                    evicted_entries,
                    evicted_bytes,
                });
            }
            Msg::Shutdown => break,
        }
    }
    CacheStats {
        entries: lru.len() as u64,
        bytes,
        evicted_entries,
        evicted_bytes,
    }
}

/// Walk `cache/<hh>/<hash>` returning (hash, len, mtime-secs) sorted by
/// ascending mtime.
pub fn scan_chunks(cache_dir: &Path) -> Result<Vec<(Hash, u64, i64)>> {
    let mut out = Vec::new();
    if !cache_dir.is_dir() {
        return Ok(out);
    }
    for sub in fs::read_dir(cache_dir)? {
        let sub = sub?;
        if !sub.file_type()?.is_dir() {
            continue;
        }
        for e in fs::read_dir(sub.path())? {
            let e = e?;
            if !e.file_type()?.is_file() {
                continue;
            }
            let name = e.file_name();
            let Some(name) = name.to_str() else { continue };
            let Ok(hash) = name.parse::<Hash>() else {
                continue;
            };
            let md = e.metadata()?;
            let mtime = md
                .modified()
                .ok()
                .and_then(|t| t.duration_since(std::time::UNIX_EPOCH).ok())
                .map(|d| d.as_secs() as i64)
                .unwrap_or(0);
            out.push((hash, md.len(), mtime));
        }
    }
    out.sort_by_key(|&(_, _, m)| m);
    Ok(out)
}

pub fn fsync_dir(p: &Path) -> io::Result<()> {
    fs::File::open(p)?.sync_all()
}

/// syncfs(2) on the filesystem containing `p`. Production mounts a
/// dedicated XFS volume for diskd (ADR 0011), so flushing it flushes only
/// diskd's own data.
pub fn syncfs(p: &Path) -> io::Result<()> {
    let f = fs::File::open(p)?;
    let rc = unsafe { libc::syncfs(std::os::unix::io::AsRawFd::as_raw_fd(&f)) };
    if rc < 0 {
        return Err(io::Error::last_os_error());
    }
    Ok(())
}
