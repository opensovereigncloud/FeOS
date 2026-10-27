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
//!
//! The budget is one of reconstructible bytes. A chunk the node holds the
//! only durable copy of is pinned instead: never evicted, and outside the
//! budget. The caller names those ([`Cache::pin`], which replaces the set);
//! a seal holds the chunks of its new layer from their admission on
//! ([`Cache::hold`]), until the caller has pinned them, so that no admit in
//! between can evict them.

use std::collections::{HashMap, HashSet};
use std::fs;
use std::io;
use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::mpsc;
use std::thread::JoinHandle;

use linked_hash_map::LinkedHashMap;
use tracing::{debug, instrument};

use crate::chunk::{self, Hash};
use crate::error::{Error, Result};

#[derive(Debug, Clone, Copy, Default)]
pub struct CacheStats {
    /// Chunks in the cache, pinned ones included.
    pub entries: u64,
    pub bytes: u64,
    pub evicted_entries: u64,
    pub evicted_bytes: u64,
    /// Of `entries` and `bytes`: the pinned and held chunks, which the
    /// budget does not count.
    pub pinned_entries: u64,
    pub pinned_bytes: u64,
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
    /// Replaces the pinned set.
    Pin {
        keep: HashSet<Hash>,
    },
    /// Adds chunks to a hold; answers once they are held.
    Hold {
        id: u64,
        hashes: Vec<Hash>,
        reply: mpsc::Sender<()>,
    },
    /// Ends a hold.
    Release {
        id: u64,
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
    next_hold: AtomicU64,
}

/// Chunks admitted under a hold ([`Cache::admit_batch`]) stay in the cache,
/// outside the budget, until the hold is dropped. A seal holds the chunks
/// of its new layer so, from their admission until the caller has pinned
/// them ([`Cache::pin`]) or given the layer up.
pub struct Hold {
    id: u64,
    tx: mpsc::Sender<Msg>,
}

impl Drop for Hold {
    fn drop(&mut self) {
        let _ = self.tx.send(Msg::Release { id: self.id });
    }
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
            next_hold: AtomicU64::new(1),
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
        let tmp = staging_tmp(&self.staging_dir(), &hash.to_hex());
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
    ///
    /// With a hold, every chunk of the batch is held, those the cache had
    /// already too: they are held before the files are looked at, so a
    /// chunk is either evicted before, and written again here, or kept.
    pub fn admit_batch(&self, chunks: &[(Hash, Vec<u8>)], hold: Option<&Hold>) -> Result<()> {
        if let Some(hold) = hold {
            let (rtx, rrx) = mpsc::channel();
            self.tx
                .send(Msg::Hold {
                    id: hold.id,
                    hashes: chunks.iter().map(|(h, _)| *h).collect(),
                    reply: rtx,
                })
                .map_err(|_| Error::Head("budget actor gone".into()))?;
            rrx.recv()
                .map_err(|_| Error::Head("budget actor gone".into()))?;
        }
        let mut wrote = false;
        for (hash, bytes) in chunks {
            let dst = self.chunk_path(hash);
            if dst.is_file() {
                let _ = self.tx.send(match hold {
                    // Accounted as held, also if its own admit is still
                    // on the way (a racing admitter's).
                    Some(_) => Msg::Admit {
                        hash: *hash,
                        len: bytes.len() as u64,
                    },
                    None => Msg::Touch { hash: *hash },
                });
                continue;
            }
            if let Some(parent) = dst.parent() {
                fs::create_dir_all(parent)?;
            }
            let tmp = staging_tmp(&self.staging_dir(), &hash.to_hex());
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

    /// Pins `keep`, in place of what was pinned: these chunks are never
    /// evicted, and the budget does not count them. A chunk in `keep` that
    /// is not in the cache is pinned when it comes. The chunks that lose
    /// their pin are the most recently used ones then.
    pub fn pin(&self, keep: HashSet<Hash>) {
        let _ = self.tx.send(Msg::Pin { keep });
    }

    /// A new hold, for one seal's admits.
    pub fn hold(&self) -> Hold {
        Hold {
            id: self.next_hold.fetch_add(1, Ordering::Relaxed),
            tx: self.tx.clone(),
        }
    }

    /// Evicts down to `target`. Pinned and held chunks are never evicted.
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

/// The budget actor's state. `lru` and `bytes` are the evictable chunks,
/// which the budget is about; `pinned` are the chunks in the cache that
/// are pinned or held, outside both.
struct Budget {
    cache_dir: PathBuf,
    budget: u64,
    lru: LinkedHashMap<Hash, u64>,
    bytes: u64,
    pinned: HashMap<Hash, u64>,
    pinned_bytes: u64,
    /// What the last `Pin` pinned, in the cache or not.
    pins: HashSet<Hash>,
    /// The open holds, with the chunks each of them holds.
    holds: HashMap<u64, HashSet<Hash>>,
    evicted_entries: u64,
    evicted_bytes: u64,
}

impl Budget {
    fn wanted(&self, h: &Hash) -> bool {
        self.pins.contains(h) || self.holds.values().any(|held| held.contains(h))
    }

    /// A chunk that came into the cache: pinned, or the most recently used.
    fn place(&mut self, hash: Hash, len: u64) {
        if let Some(old) = self.lru.remove(&hash) {
            self.bytes = self.bytes.saturating_sub(old);
        }
        if let Some(old) = self.pinned.remove(&hash) {
            self.pinned_bytes = self.pinned_bytes.saturating_sub(old);
        }
        if self.wanted(&hash) {
            self.pinned.insert(hash, len);
            self.pinned_bytes += len;
        } else {
            self.lru.insert(hash, len);
            self.bytes += len;
        }
    }

    /// Takes these chunks out of the LRU, if they are in it, and pins them.
    fn pin_from_lru<'a>(&mut self, hashes: impl IntoIterator<Item = &'a Hash>) {
        for h in hashes {
            if let Some(len) = self.lru.remove(h) {
                self.bytes = self.bytes.saturating_sub(len);
                self.pinned.insert(*h, len);
                self.pinned_bytes += len;
            }
        }
    }

    /// Puts the pinned chunks that are no longer wanted back into the
    /// LRU, as its most recently used.
    fn unpin_unwanted(&mut self) {
        let unwanted: Vec<Hash> = self
            .pinned
            .keys()
            .filter(|h| !self.wanted(h))
            .copied()
            .collect();
        for h in unwanted {
            if let Some(len) = self.pinned.remove(&h) {
                self.pinned_bytes = self.pinned_bytes.saturating_sub(len);
                self.lru.insert(h, len);
                self.bytes += len;
            }
        }
    }

    /// Evicts the least recently used chunk that is not in `keep`, and
    /// returns its length.
    fn drop_oldest(&mut self, keep: &HashSet<Hash>) -> Option<u64> {
        // Skip over protected (protenured) entries without evicting them.
        let mut skipped: Vec<(Hash, u64)> = Vec::new();
        let mut next: Option<(Hash, u64)> = None;
        while let Some((h, len)) = self.lru.pop_front() {
            if keep.contains(&h) {
                skipped.push((h, len));
            } else {
                next = Some((h, len));
                break;
            }
        }
        self.lru.extend(skipped);
        let (h, len) = next?;
        let p = self.cache_dir.join(chunk::cache_rel_path(&h));
        if let Err(e) = fs::remove_file(&p)
            && e.kind() != io::ErrorKind::NotFound
        {
            debug!(hash = %h.to_hex(), error = %e, "evict unlink failed");
        }
        self.bytes = self.bytes.saturating_sub(len);
        Some(len)
    }

    /// Evicts until the evictable bytes are within `goal`, never a chunk
    /// in `keep`, and returns what it freed.
    fn evict_to(&mut self, goal: u64, keep: &HashSet<Hash>) -> EvictStats {
        let before_e = self.evicted_entries;
        let before_b = self.evicted_bytes;
        while self.bytes > goal {
            match self.drop_oldest(keep) {
                Some(l) => {
                    self.evicted_entries += 1;
                    self.evicted_bytes += l;
                }
                None => break,
            }
        }
        EvictStats {
            removed_entries: self.evicted_entries - before_e,
            freed_bytes: self.evicted_bytes - before_b,
        }
    }

    fn stats(&self) -> CacheStats {
        CacheStats {
            entries: (self.lru.len() + self.pinned.len()) as u64,
            bytes: self.bytes + self.pinned_bytes,
            evicted_entries: self.evicted_entries,
            evicted_bytes: self.evicted_bytes,
            pinned_entries: self.pinned.len() as u64,
            pinned_bytes: self.pinned_bytes,
        }
    }
}

fn actor_loop(
    cache_dir: PathBuf,
    budget: u64,
    rx: mpsc::Receiver<Msg>,
    lru: LinkedHashMap<Hash, u64>,
    bytes: u64,
) -> CacheStats {
    let mut b = Budget {
        cache_dir,
        budget,
        lru,
        bytes,
        pinned: HashMap::new(),
        pinned_bytes: 0,
        pins: HashSet::new(),
        holds: HashMap::new(),
        evicted_entries: 0,
        evicted_bytes: 0,
    };
    let nothing = HashSet::new();

    while let Ok(msg) = rx.recv() {
        match msg {
            Msg::Admit { hash, len } => {
                b.place(hash, len);
                b.evict_to(b.budget, &nothing);
            }
            Msg::Touch { hash } => {
                if let Some(len) = b.lru.remove(&hash) {
                    b.lru.insert(hash, len);
                }
            }
            Msg::Remove { hash } => {
                if let Some(len) = b.lru.remove(&hash) {
                    b.bytes = b.bytes.saturating_sub(len);
                }
                if let Some(len) = b.pinned.remove(&hash) {
                    b.pinned_bytes = b.pinned_bytes.saturating_sub(len);
                }
            }
            Msg::Pin { keep } => {
                b.pin_from_lru(&keep);
                b.pins = keep;
                b.unpin_unwanted();
                b.evict_to(b.budget, &nothing);
            }
            Msg::Hold { id, hashes, reply } => {
                b.pin_from_lru(&hashes);
                b.holds.entry(id).or_default().extend(hashes);
                let _ = reply.send(());
            }
            Msg::Release { id } => {
                if b.holds.remove(&id).is_some() {
                    b.unpin_unwanted();
                    b.evict_to(b.budget, &nothing);
                }
            }
            Msg::Evict {
                target,
                keep,
                reply,
            } => {
                let goal = match target {
                    EvictTarget::All => 0,
                    EvictTarget::Budget => b.budget,
                };
                let _ = reply.send(b.evict_to(goal, &keep));
            }
            Msg::Stats { reply } => {
                let _ = reply.send(b.stats());
            }
            Msg::Shutdown => break,
        }
    }
    b.stats()
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

/// A staging file of its own for one write of `name` (a chunk's hash, a
/// layer's manifest) in `staging`. Two seals that write the same chunk at
/// once must not share one: the first rename would take it from under the
/// second. The process id keeps a second process on the same state dir
/// apart; [`Cache::open`] removes what an interrupted write left.
pub fn staging_tmp(staging: &Path, name: &str) -> PathBuf {
    static WRITES: AtomicU64 = AtomicU64::new(0);
    staging.join(format!(
        "{name}.{}-{}.tmp",
        std::process::id(),
        WRITES.fetch_add(1, Ordering::Relaxed)
    ))
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

#[cfg(test)]
mod tests {
    use super::*;

    const LEN: usize = 4096;

    /// A chunk of its own for each `i`.
    fn data(i: u32) -> Vec<u8> {
        let mut v = vec![0x5a; LEN];
        v[..4].copy_from_slice(&i.to_le_bytes());
        v
    }

    fn admit(cache: &Cache, i: u32) -> Hash {
        let d = data(i);
        let h = chunk::hash(&d);
        cache.admit(&h, &d).unwrap();
        h
    }

    /// The actor's state once it has handled every message sent so far.
    fn settled(cache: &Cache) -> CacheStats {
        cache.stats().unwrap()
    }

    fn three_chunks(dir: &Path) -> Cache {
        Cache::open(dir, 3 * LEN as u64).unwrap()
    }

    #[test]
    fn pinned_chunks_survive_the_budget() {
        let dir = tempfile::tempdir().unwrap();
        let cache = three_chunks(dir.path());
        let first: Vec<Hash> = (0..3).map(|i| admit(&cache, i)).collect();
        cache.pin(first[..2].iter().copied().collect());
        let more: Vec<Hash> = (10..15).map(|i| admit(&cache, i)).collect();
        let st = settled(&cache);
        assert!(cache.contains(&first[0]) && cache.contains(&first[1]));
        assert!(!cache.contains(&first[2]), "the unpinned one is evicted");
        assert!(!cache.contains(&more[0]) && cache.contains(&more[4]));
        assert_eq!(st.bytes, 5 * LEN as u64, "2 pinned + 3 in the budget");
        assert_eq!(st.entries, 5);
        assert_eq!(st.pinned_entries, 2);
        assert_eq!(st.pinned_bytes, 2 * LEN as u64);
    }

    #[test]
    fn pinned_admits_survive_unpinned_admits() {
        let dir = tempfile::tempdir().unwrap();
        let cache = three_chunks(dir.path());
        let hold = cache.hold();
        let held: Vec<(Hash, Vec<u8>)> = (0..5)
            .map(|i| {
                let d = data(i);
                (chunk::hash(&d), d)
            })
            .collect();
        cache.admit_batch(&held, Some(&hold)).unwrap();
        for i in 100..110 {
            admit(&cache, i);
        }
        let st = settled(&cache);
        for (h, _) in &held {
            assert!(cache.contains(h), "a held chunk was evicted");
        }
        assert_eq!(st.pinned_entries, 5);
        assert_eq!(st.bytes, 8 * LEN as u64);
    }

    #[test]
    fn pin_replace_unpins() {
        let dir = tempfile::tempdir().unwrap();
        let cache = three_chunks(dir.path());
        let a = admit(&cache, 0);
        cache.pin([a].into_iter().collect());
        for i in 1..4 {
            admit(&cache, i);
        }
        assert!(cache.contains(&a));
        cache.pin(HashSet::new());
        assert_eq!(settled(&cache).pinned_entries, 0);
        for i in 10..13 {
            admit(&cache, i);
        }
        settled(&cache);
        assert!(!cache.contains(&a), "an unpinned chunk is evicted again");

        // A held chunk keeps its place while its hold lives, whatever is
        // pinned, and is evictable once the hold is gone.
        let hold = cache.hold();
        let d = data(20);
        let c = chunk::hash(&d);
        cache.admit_batch(&[(c, d)], Some(&hold)).unwrap();
        cache.pin(HashSet::new());
        for i in 30..35 {
            admit(&cache, i);
        }
        settled(&cache);
        assert!(cache.contains(&c), "a held chunk lost its hold to a pin");
        drop(hold);
        assert_eq!(settled(&cache).pinned_entries, 0);
        for i in 40..43 {
            admit(&cache, i);
        }
        settled(&cache);
        assert!(!cache.contains(&c), "a released chunk is evicted again");
    }

    #[test]
    fn a_hold_takes_chunks_the_cache_had() {
        let dir = tempfile::tempdir().unwrap();
        let cache = three_chunks(dir.path());
        let a = admit(&cache, 0);
        let hold = cache.hold();
        cache.admit_batch(&[(a, data(0))], Some(&hold)).unwrap();
        for i in 1..6 {
            admit(&cache, i);
        }
        let st = settled(&cache);
        assert!(cache.contains(&a));
        assert_eq!(st.pinned_entries, 1);
    }

    #[test]
    fn evict_all_keeps_pinned() {
        let dir = tempfile::tempdir().unwrap();
        let cache = Cache::open(dir.path(), 1 << 30).unwrap();
        let hs: Vec<Hash> = (0..4).map(|i| admit(&cache, i)).collect();
        cache.pin(hs[..2].iter().copied().collect());
        let freed = cache.evict(EvictTarget::All).unwrap();
        assert_eq!(freed.removed_entries, 2);
        assert_eq!(freed.freed_bytes, 2 * LEN as u64);
        assert!(cache.contains(&hs[0]) && cache.contains(&hs[1]));
        assert!(!cache.contains(&hs[2]) && !cache.contains(&hs[3]));
        let st = settled(&cache);
        assert_eq!(st.entries, 2);
        assert_eq!(st.bytes, 2 * LEN as u64);
        assert_eq!(st.pinned_bytes, 2 * LEN as u64);
    }

    #[test]
    fn admit_of_a_pinned_hash_is_held() {
        let dir = tempfile::tempdir().unwrap();
        let cache = Cache::open(dir.path(), LEN as u64).unwrap();
        let h = chunk::hash(&data(0));
        cache.pin([h].into_iter().collect());
        assert_eq!(settled(&cache).pinned_entries, 0, "not in the cache yet");
        admit(&cache, 0);
        for i in 1..4 {
            admit(&cache, i);
        }
        let st = settled(&cache);
        assert!(cache.contains(&h));
        assert_eq!(st.pinned_entries, 1);
        assert_eq!(st.bytes, 2 * LEN as u64, "the pinned one and the budget's");
    }

    #[test]
    fn remove_of_a_pinned_chunk() {
        let dir = tempfile::tempdir().unwrap();
        let cache = three_chunks(dir.path());
        let a = admit(&cache, 0);
        cache.pin([a].into_iter().collect());
        assert_eq!(settled(&cache).pinned_entries, 1);
        // GC unlinks a chunk, then makes the cache forget it.
        fs::remove_file(cache.chunk_path(&a)).unwrap();
        cache.forget(&a);
        let st = settled(&cache);
        assert_eq!((st.entries, st.bytes, st.pinned_entries), (0, 0, 0));
        // Still pinned: it is held again when it comes back.
        admit(&cache, 0);
        assert_eq!(settled(&cache).pinned_entries, 1);
    }

    /// Two seals of like disks at once admit the same new chunks: each
    /// stages its own file, so that neither renames the other's away.
    #[test]
    fn racing_admitters_of_a_chunk_both_succeed() {
        let dir = tempfile::tempdir().unwrap();
        let cache = Cache::open(dir.path(), 1 << 30).unwrap();
        for round in 0..50u32 {
            let chunks: Vec<(Hash, Vec<u8>)> = (0..8)
                .map(|i| {
                    let d = data(round * 8 + i);
                    (chunk::hash(&d), d)
                })
                .collect();
            let start = std::sync::Barrier::new(4);
            std::thread::scope(|s| {
                for t in 0..4 {
                    let (cache, chunks, start) = (&cache, &chunks, &start);
                    s.spawn(move || {
                        start.wait();
                        if t % 2 == 0 {
                            cache.admit_batch(chunks, None).unwrap();
                        } else {
                            for (h, d) in chunks {
                                cache.admit(h, d).unwrap();
                            }
                        }
                    });
                }
            });
            for (h, d) in &chunks {
                assert_eq!(&fs::read(cache.chunk_path(h)).unwrap(), d);
            }
        }
        assert_eq!(settled(&cache).entries, 50 * 8);
        let staged = fs::read_dir(cache.staging_dir()).unwrap().count();
        assert_eq!(staged, 0, "staging files left");
    }
}
