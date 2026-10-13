// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! WatchService — filesystem subscriptions over inotify.
//!
//! A platform client rendering a workspace has no way to learn that the
//! workspace changed; polling twenty open directories through four hops on
//! a timer is both too slow for a build burst and too expensive for a
//! directory set that changes rarely. So the guest watches what the client
//! displays and tells it when that changes.
//!
//! The shape is `PortForwardService::forward`'s, deliberately: one
//! bidirectional stream per subscriber, and **the stream is the lease**
//!. Nothing here times a watch out — [`Lease`]'s `Drop`
//! releases every watch the stream held, and the stream ends for every
//! reason the transport can notice, including the browser tab two hops up
//! going away. There is no unsubscribe RPC and no cleanup path to skip.
//!
//! The parts that are wrong by default, and how they are
//! handled here:
//!
//! * **One inotify fd for the whole guest, refcounted per `wd`.** A guest
//!   is a sandbox. Two streams watching one directory get the same watch
//!   descriptor back from the kernel (inotify deduplicates per inode), so
//!   the first stream to go away must not blind the second —
//!   [`WdEntry::refs`] counts holders, and only the last release calls
//!   `inotify_rm_watch`.
//! * **Register, then list.** Every `subscribe` is answered with exactly
//!   one `ack`, sent after the watches are in place, so a client can take
//!   its listing afterwards and miss nothing in between. The ack carries
//!   the event cursor the listing corresponds to.
//! * **A file is watched through its parent.** Editors and build tools
//!   replace a file by writing a temporary and renaming over it; a watch
//!   on the file's inode sees an unlink of something nobody looks at any
//!   more. [`resolve`] turns a file path into (parent directory, basename)
//!   and the basename becomes a server-side [`Filter`].
//! * **`IN_MOVE_SELF` and `IN_IGNORED` invalidate.** Watches bind to
//!   inodes, not paths. A renamed directory is reported stale rather than
//!   having its paths patched, and a deleted one — `IN_DELETE_SELF` then
//!   `IN_IGNORED`, with a directory recreated at the same path *not*
//!   watched — is reported stale too. FeOS never re-arms by itself: the
//!   client's next complete path set does that, which is the whole reason
//!   §2 reconciles instead of diffing. A `git checkout` that removes and
//!   recreates a directory is the ordinary case here, not an edge one.
//! * **A cap per sandbox.** 8192 is the kernel's *floor* for
//!   `max_user_watches` and it is one budget for every uid-0 process in
//!   the microVM, payload included; [`MAX_WATCHES`] keeps the platform's
//!   share well under it.
//!
//! Confinement is the one that already exists: paths resolve
//! beneath the container rootfs through `openat2`/`RESOLVE_BENEATH` exactly
//! as `feos_utils::container_fs` does it, and the watch is then placed through
//! `/proc/self/fd/<fd>`. `inotify_add_watch` takes a path, so the naive
//! `rootfs.join(client_path)` would reintroduce precisely the symlink
//! escape `container_fs` exists to prevent.
//!
//! The container rootfs is an overlay; the question was whether a watch
//! armed on a directory that exists only in the image's lower layer
//! survives the copy-up of the payload's first write into it. Measured
//! 2026-10-01: it does — the overlay inode keeps its `st_dev`/`st_ino`
//! across copy-up, so the fsnotify mark rides on it (deliberate kernel
//! behaviour since v4.16). Nothing here compensates for copy-up, because
//! there is nothing to compensate for. The one place that would have to
//! change if a patched overlayfs ever broke that is [`place`].

use std::collections::{HashMap, HashSet};
use std::fs::File;
use std::os::fd::{AsFd, AsRawFd, RawFd};
use std::pin::Pin;
use std::sync::{Arc, Mutex};
use std::time::Duration;

use feos_proto::watch_service::watch_request::Frame as ReqFrame;
use feos_proto::watch_service::watch_response::Frame as RespFrame;
use feos_proto::watch_service::watch_service_server::WatchService;
use feos_proto::watch_service::{
    Ack, Event, EventBatch, EventKind, PathError, WatchRequest, WatchResponse,
};
use nix::libc;
use nix::sys::inotify::{AddWatchFlags, InitFlags, Inotify, InotifyEvent, WatchDescriptor};
use tokio::io::unix::AsyncFd;
use tokio::sync::mpsc;
use tokio::time::Instant;
use tokio_stream::wrappers::UnboundedReceiverStream;
use tonic::{Request, Response, Status, Streaming};

use feos_utils::container_fs as toolfs;

/// Guest-wide watch cap. `fs.inotify.max_user_watches` is
/// 8192 in our guests — the kernel's floor, not a tuned value — and it is
/// a per-uid ucount, i.e. one budget shared with everything the payload
/// container runs as root. A quarter of it is the platform's share; a
/// client that wants more is told so per path rather than having its
/// subscription fail.
const MAX_WATCHES: usize = 2048;

/// Paths accepted in one `subscribe`. Bounds the work done under the hub
/// lock; a tree view with more than this many open directories is not the
/// access pattern this is designed for.
const MAX_PATHS: usize = 1024;

/// Coalescing window. An `npm install` must not become one
/// frame per file through vsock.
const COALESCE: Duration = Duration::from_millis(150);

/// Distinct entries one batch may carry before the batch itself becomes a
/// `stale` — the same answer the kernel gives us with `IN_Q_OVERFLOW`, for
/// the same reason: past some burst size, "re-read this" is both cheaper
/// and more honest than a list.
const MAX_BATCH: usize = 2048;

/// The mask. `IN_MODIFY` on the parent watch is what delivers
/// §3's file changes; `IN_EXCL_UNLINK` keeps an unlinked-but-still-open
/// file from reporting writes nobody can see any more; `IN_ONLYDIR` is
/// belt and braces, since [`place`] only ever opens directories.
fn watch_mask() -> AddWatchFlags {
    // nix 0.30 models only the flags it names, and names neither
    // IN_EXCL_UNLINK nor IN_MASK_*; those bits come from libc.
    AddWatchFlags::IN_CREATE
        | AddWatchFlags::IN_DELETE
        | AddWatchFlags::IN_MOVED_FROM
        | AddWatchFlags::IN_MOVED_TO
        | AddWatchFlags::IN_MODIFY
        | AddWatchFlags::IN_ATTRIB
        | AddWatchFlags::IN_DELETE_SELF
        | AddWatchFlags::IN_MOVE_SELF
        | AddWatchFlags::IN_ONLYDIR
        | AddWatchFlags::from_bits_retain(libc::IN_EXCL_UNLINK)
}

// ----------------------------------------------------------------------
// The placement seam
// ----------------------------------------------------------------------

/// Place a watch on one directory beneath `anchor`.
///
/// This is the **only** place in the service that calls
/// `inotify_add_watch`, and the only one that knows how a path becomes a
/// watch descriptor. Everything downstream — the refcounts, the name
/// filters, the event fan-out — is keyed by `wd` and knows nothing about
/// paths, so a change in how a watch is placed (or re-placed) is confined
/// to this function.
///
/// The confinement rule lives here: `rel_dir` is resolved
/// with `openat2(RESOLVE_BENEATH)` from the rootfs anchor, and the watch
/// is added through the resulting fd's `/proc/self/fd/<n>` magic link, so
/// neither a lexical `..` nor a symlink the payload swaps in mid-walk can
/// move the watch outside the container.
///
/// Re-placing is simply calling this again: `inotify_add_watch` is
/// idempotent per inode, so a reconcile that re-places an unchanged path
/// gets the same `wd` back for free, and one that re-places a path whose
/// directory was deleted and recreated gets a watch on the new inode.
fn place(inotify: &Inotify, anchor: &File, rel_dir: &str) -> Result<WatchDescriptor, String> {
    let dirfd = toolfs::open_file(anchor, rel_dir, libc::O_PATH | libc::O_DIRECTORY, 0)?;
    // The fd is pinned for as long as `dirfd` lives; the kernel resolves
    // the magic link straight to the inode it already holds open.
    let pinned = toolfs::fd_walk_path(&dirfd);
    inotify
        .add_watch(pinned.as_str(), watch_mask())
        .map_err(|e| format!("inotify_add_watch {}: {e}", rooted(rel_dir)))
}

/// Resolve a client path to the directory that will actually be watched
/// and, for a file, the basename to filter on.
///
/// A path that does not exist resolves through its parent too, so
/// "tell me when this file appears" is a subscription a client can make.
///
/// The same fallback is what happens to a symlink whose target lies
/// outside the rootfs: `openat2(RESOLVE_BENEATH)` refuses to follow it, so
/// it is treated as the entry it is — the watch lands on the parent
/// directory inside the rootfs with the link's own name as the filter, and
/// the client hears about the link rather than about whatever it points
/// at. Nothing outside the container is ever watched.
fn resolve(anchor: &File, path: &str) -> Result<(String, Option<String>), String> {
    let rel = toolfs::to_rel(path)?;
    // O_DIRECTORY fails with ENOTDIR for a file and ENOENT for a path that
    // is not there yet; both mean "watch the parent".
    if toolfs::open_file(anchor, &rel, libc::O_PATH | libc::O_DIRECTORY, 0).is_ok() {
        return Ok((rel, None));
    }
    if rel.is_empty() {
        return Err(format!("{path}: container root is not a directory"));
    }
    let (parent, base) = match rel.rsplit_once('/') {
        Some((p, b)) => (p.to_string(), b.to_string()),
        None => (String::new(), rel.clone()),
    };
    toolfs::open_file(anchor, &parent, libc::O_PATH | libc::O_DIRECTORY, 0)
        .map_err(|e| format!("{path}: {e}"))?;
    Ok((parent, Some(base)))
}

/// A rootfs-relative path as the client sees it: container-rooted, with a
/// leading slash. The choice was between this and
/// paths relative to the subscribed directory; container-rooted wins
/// because a client holding a tree can use it unchanged, and it costs the
/// guest nothing (the path is already in the `wd → path` map).
fn rooted(rel: &str) -> String {
    if rel.is_empty() {
        "/".to_string()
    } else {
        format!("/{rel}")
    }
}

// ----------------------------------------------------------------------
// Hub: the one inotify fd, its wd → path map and its refcounts
// ----------------------------------------------------------------------

/// Which basenames a stream wants out of one watched directory.
#[derive(Clone, Debug)]
enum Filter {
    /// The stream subscribed to the directory itself: every name counts.
    All,
    /// The stream only subscribed to files in it.
    Names(HashSet<String>),
}

impl Filter {
    fn merge(&mut self, other: &Filter) {
        match (&mut *self, other) {
            (Filter::All, _) => {}
            (_, Filter::All) => *self = Filter::All,
            (Filter::Names(mine), Filter::Names(theirs)) => {
                mine.extend(theirs.iter().cloned());
            }
        }
    }

    fn accepts(&self, name: &str) -> bool {
        match self {
            Filter::All => true,
            Filter::Names(names) => names.contains(name),
        }
    }
}

/// What one stream holds on one watch descriptor.
#[derive(Clone, Debug)]
struct Held {
    /// Container-rooted path of the watched directory, as reported in
    /// events.
    dir: String,
    filter: Filter,
}

/// The `wd → path` map, plus the refcount that keeps one
/// stream's release from blinding another's.
struct WdEntry {
    refs: usize,
    /// Kept for diagnostics: which path first produced this descriptor.
    #[allow(dead_code)]
    dir: String,
}

/// One message on a stream's internal channel. The hub reader and the
/// inbound-request reader both feed it, so the pump task is the single
/// writer to the response stream and the single assigner of event ids.
enum Msg {
    /// An event that passed this stream's filters, already resolved to a
    /// container-rooted path.
    Fs {
        kind: EventKind,
        dir: String,
        name: String,
        is_dir: bool,
        cookie: u32,
    },
    /// Everything under these paths must be re-read.
    Stale { paths: Vec<String> },
    /// A complete, replacing path set.
    Subscribe { paths: Vec<String> },
    /// The request stream ended — the lease is over.
    End,
}

struct Sub {
    tx: mpsc::UnboundedSender<Msg>,
    held: HashMap<WatchDescriptor, Held>,
    /// The last acked client path set, verbatim. It is what a `stale` from
    /// an overflow names: the kernel does not say which events it dropped,
    /// so everything this stream subscribes to is suspect.
    paths: Vec<String>,
}

#[derive(Default)]
struct HubState {
    wds: HashMap<WatchDescriptor, WdEntry>,
    subs: HashMap<u64, Sub>,
    next_sub: u64,
}

/// Outcome of one reconcile, before the pump stamps a cursor on it.
struct Reconciled {
    watching: Vec<String>,
    failed: Vec<PathError>,
    watches: u32,
}

pub struct Hub {
    inotify: Arc<Inotify>,
    state: Mutex<HubState>,
}

impl Hub {
    /// Create the guest's inotify instance and start its reader. Lazy (see
    /// [`WatchApiHandler::hub`]): a guest nobody subscribes to spends no fd.
    pub fn start() -> Result<Arc<Hub>, String> {
        let inotify = Inotify::init(InitFlags::IN_NONBLOCK | InitFlags::IN_CLOEXEC)
            .map_err(|e| format!("inotify_init1: {e}"))?;
        let hub = Arc::new(Hub {
            inotify: Arc::new(inotify),
            state: Mutex::new(HubState::default()),
        });
        tokio::spawn(read_loop(hub.clone()));
        Ok(hub)
    }

    /// Register a stream. The returned sender is cloned to whoever feeds
    /// the pump; the hub keeps its own clone to deliver events.
    fn attach(
        &self,
    ) -> (
        u64,
        mpsc::UnboundedSender<Msg>,
        mpsc::UnboundedReceiver<Msg>,
    ) {
        let (tx, rx) = mpsc::unbounded_channel();
        let mut st = self.state.lock().unwrap_or_else(|e| e.into_inner());
        st.next_sub += 1;
        let id = st.next_sub;
        st.subs.insert(
            id,
            Sub {
                tx: tx.clone(),
                held: HashMap::new(),
                paths: Vec::new(),
            },
        );
        (id, tx, rx)
    }

    /// Release everything a stream holds. This is the whole of the lease
    /// §5's guest side: no TTL, no sweeper, no timeout.
    fn detach(&self, id: u64) {
        let mut st = self.state.lock().unwrap_or_else(|e| e.into_inner());
        let Some(sub) = st.subs.remove(&id) else {
            return;
        };
        for wd in sub.held.keys() {
            self.release_locked(&mut st, *wd);
        }
    }

    fn release_locked(&self, st: &mut HubState, wd: WatchDescriptor) {
        let Some(entry) = st.wds.get_mut(&wd) else {
            return;
        };
        entry.refs = entry.refs.saturating_sub(1);
        if entry.refs == 0 {
            st.wds.remove(&wd);
            // EINVAL here just means the kernel dropped the watch first
            // (the directory went away); nothing to do about it.
            let _ = self.inotify.rm_watch(wd);
        }
    }

    /// Apply a complete path set to one stream, replacing the previous one
    ///.
    ///
    /// Placement happens with the hub lock held. The syscalls under it are
    /// a bounded `openat2` + `inotify_add_watch` per path, and holding the
    /// lock across them is what keeps another stream's release from
    /// deleting a descriptor between the moment this one takes it and the
    /// moment it is counted.
    fn reconcile(&self, id: u64, anchor: &File, paths: &[String]) -> Reconciled {
        let mut failed: Vec<PathError> = Vec::new();
        let mut watching: Vec<String> = Vec::new();

        // Resolve first (no lock needed): client path → watched directory
        // plus an optional name filter, merged per directory so that
        // subscribing to a directory and to a file inside it costs one
        // watch and keeps the directory's "every name" semantics.
        let mut wanted: HashMap<String, (Filter, Vec<String>)> = HashMap::new();
        for p in paths.iter().take(MAX_PATHS) {
            match resolve(anchor, p) {
                Ok((dir, name)) => {
                    let filter = match name {
                        Some(n) => Filter::Names(HashSet::from([n])),
                        None => Filter::All,
                    };
                    let slot = wanted
                        .entry(dir)
                        .or_insert_with(|| (filter.clone(), Vec::new()));
                    slot.0.merge(&filter);
                    slot.1.push(p.clone());
                }
                Err(e) => failed.push(PathError {
                    path: p.clone(),
                    error: e,
                }),
            }
        }
        for p in paths.iter().skip(MAX_PATHS) {
            failed.push(PathError {
                path: p.clone(),
                error: format!("more than {MAX_PATHS} paths in one subscription"),
            });
        }

        let mut st = self.state.lock().unwrap_or_else(|e| e.into_inner());
        let old_held = match st.subs.get_mut(&id) {
            Some(sub) => std::mem::take(&mut sub.held),
            // The stream is gone (released while this was in flight);
            // placing watches nobody holds would leak them.
            None => {
                return Reconciled {
                    watching,
                    failed,
                    watches: 0,
                }
            }
        };
        let mut new_held: HashMap<WatchDescriptor, Held> = HashMap::new();

        for (rel, (filter, clients)) in wanted {
            let dir = rooted(&rel);
            let wd = match place(&self.inotify, anchor, &rel) {
                Ok(w) => w,
                Err(e) => {
                    for c in clients {
                        failed.push(PathError {
                            path: c,
                            error: e.clone(),
                        });
                    }
                    continue;
                }
            };

            if !st.wds.contains_key(&wd) {
                // A descriptor nobody held before. The cap can only be
                // judged here, because whether `place` creates a watch or
                // hands back one that already exists is the kernel's call
                // (it deduplicates per inode) — so an over-cap watch is
                // taken back immediately, which is safe precisely because
                // no other stream holds it.
                if st.wds.len() >= MAX_WATCHES {
                    let _ = self.inotify.rm_watch(wd);
                    for c in clients {
                        failed.push(PathError {
                            path: c,
                            error: format!("watch cap reached ({MAX_WATCHES} per sandbox)"),
                        });
                    }
                    continue;
                }
                st.wds.insert(
                    wd,
                    WdEntry {
                        refs: 0,
                        dir: dir.clone(),
                    },
                );
            }

            // One ref per stream per descriptor: a stream that subscribes
            // to one directory under two names must not count twice, or
            // releasing it once would never free it.
            if !old_held.contains_key(&wd)
                && !new_held.contains_key(&wd)
                && let Some(e) = st.wds.get_mut(&wd)
            {
                e.refs += 1;
            }
            new_held
                .entry(wd)
                .and_modify(|h| h.filter.merge(&filter))
                .or_insert_with(|| Held {
                    dir: dir.clone(),
                    filter: filter.clone(),
                });
            watching.extend(clients);
        }

        // Add before release, so a descriptor kept across the reconcile
        // never drops to zero refs and gets removed from the kernel only
        // to be re-added.
        for wd in old_held.keys() {
            if !new_held.contains_key(wd) {
                self.release_locked(&mut st, *wd);
            }
        }

        let watches = st.wds.len() as u32;
        if let Some(sub) = st.subs.get_mut(&id) {
            sub.held = new_held;
            sub.paths = watching.clone();
        }
        Reconciled {
            watching,
            failed,
            watches,
        }
    }

    /// Fan one read's worth of inotify events out to the streams that
    /// asked for them.
    fn dispatch(&self, events: Vec<InotifyEvent>) {
        let mut st = self.state.lock().unwrap_or_else(|e| e.into_inner());
        for ev in events {
            if ev.mask.contains(AddWatchFlags::IN_Q_OVERFLOW) {
                // The kernel dropped events and will not say which
                //: every subscriber must re-read everything
                // it holds. This is the failure mode a build burst makes
                // ordinary, not an edge case.
                for sub in st.subs.values() {
                    let _ = sub.tx.send(Msg::Stale {
                        paths: sub.paths.clone(),
                    });
                }
                continue;
            }
            if ev.mask.contains(AddWatchFlags::IN_IGNORED)
                || ev.mask.contains(AddWatchFlags::IN_UNMOUNT)
            {
                // The kernel has already removed the watch: the directory
                // was deleted (IN_DELETE_SELF, then this) or its
                // filesystem went away. A directory recreated at the same
                // path is NOT watched, and `git checkout` does exactly
                // that — so this must reach the client as `stale` rather
                // than being swallowed, and FeOS must not re-arm behind
                // the client's back. The next complete path
                // set re-establishes the watch on the new inode.
                self.invalidate(&mut st, ev.wd, Removal::ByKernel);
                continue;
            }
            if ev.mask.contains(AddWatchFlags::IN_MOVE_SELF) {
                // Watches bind to inodes, not paths: every path this
                // descriptor can name is now a lie. Report stale and
                // invalidate rather than patching paths; the
                // subscriber's next subscribe re-places whatever is at the
                // path now.
                self.invalidate(&mut st, ev.wd, Removal::ByUs);
                continue;
            }
            let Some(kind) = classify(ev.mask) else {
                continue;
            };
            let name = ev
                .name
                .as_ref()
                .map(|n| n.to_string_lossy().into_owned())
                .unwrap_or_default();
            let is_dir = ev.mask.contains(AddWatchFlags::IN_ISDIR);
            for sub in st.subs.values() {
                let Some(held) = sub.held.get(&ev.wd) else {
                    continue;
                };
                // Events about the directory itself carry no name and go
                // to every holder: a stream watching one file in it still
                // needs to know its parent is gone.
                if !name.is_empty() && !held.filter.accepts(&name) {
                    continue;
                }
                let _ = sub.tx.send(Msg::Fs {
                    kind,
                    dir: held.dir.clone(),
                    name: name.clone(),
                    is_dir,
                    cookie: ev.cookie,
                });
            }
        }
    }

    /// Tell every holder the descriptor's subtree is stale, then drop it
    /// from the maps — leaving no `wd → path` entry that would answer a
    /// later event with a path that no longer means what it says.
    fn invalidate(&self, st: &mut HubState, wd: WatchDescriptor, removal: Removal) {
        for sub in st.subs.values_mut() {
            if let Some(held) = sub.held.remove(&wd) {
                let _ = sub.tx.send(Msg::Stale {
                    paths: vec![held.dir],
                });
            }
        }
        let known = st.wds.remove(&wd).is_some();
        if known && removal == Removal::ByUs {
            let _ = self.inotify.rm_watch(wd);
        }
    }

    /// Watches currently registered with the kernel, guest-wide.
    #[cfg(test)]
    fn watch_count(&self) -> usize {
        self.state
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .wds
            .len()
    }
}

/// Who takes the watch out of the kernel when a descriptor is invalidated.
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
enum Removal {
    /// The watch still exists and we must call `inotify_rm_watch`.
    ByUs,
    /// The kernel already dropped it (`IN_IGNORED`/`IN_UNMOUNT`); an
    /// `inotify_rm_watch` would only EINVAL.
    ByKernel,
}

/// One inotify event bit → one event kind. Returns `None` for the bits
/// handled elsewhere (overflow, ignored, move-self) and for anything the
/// mask did not ask for.
fn classify(mask: AddWatchFlags) -> Option<EventKind> {
    if mask.contains(AddWatchFlags::IN_CREATE) {
        Some(EventKind::Created)
    } else if mask.contains(AddWatchFlags::IN_DELETE)
        || mask.contains(AddWatchFlags::IN_DELETE_SELF)
    {
        Some(EventKind::Deleted)
    } else if mask.contains(AddWatchFlags::IN_MOVED_FROM) {
        Some(EventKind::MovedFrom)
    } else if mask.contains(AddWatchFlags::IN_MOVED_TO) {
        Some(EventKind::MovedTo)
    } else if mask.contains(AddWatchFlags::IN_MODIFY) {
        Some(EventKind::Modified)
    } else if mask.contains(AddWatchFlags::IN_ATTRIB) {
        Some(EventKind::Attrib)
    } else {
        None
    }
}

/// Releases a stream's watches when the stream ends — for any reason,
/// including the task being dropped. The stream is the lease, in one `Drop`.
struct Lease {
    hub: Arc<Hub>,
    id: u64,
}

impl Drop for Lease {
    fn drop(&mut self) {
        self.hub.detach(self.id);
    }
}

/// The guest's single inotify reader. `AsyncFd` registers the fd with the
/// tokio reactor, so the watch service costs no runtime thread while
/// nothing is happening.
async fn read_loop(hub: Arc<Hub>) {
    let afd = match AsyncFd::new(InotifyFd(hub.inotify.clone())) {
        Ok(a) => a,
        Err(e) => {
            log::warn!("watch: inotify not pollable: {e}");
            return;
        }
    };
    let mut complained = false;
    loop {
        let mut guard = match afd.readable().await {
            Ok(g) => g,
            Err(e) => {
                // PID 1 never exits and neither does this: back off and
                // keep trying rather than leaving every subscriber silent.
                if !complained {
                    log::warn!("watch: readable: {e}");
                    complained = true;
                }
                tokio::time::sleep(Duration::from_millis(100)).await;
                continue;
            }
        };
        match guard.try_io(|afd| {
            afd.get_ref()
                .0
                .read_events()
                .map_err(|e| std::io::Error::from_raw_os_error(e as i32))
        }) {
            Ok(Ok(events)) => hub.dispatch(events),
            Ok(Err(e)) => {
                if !complained {
                    log::warn!("watch: read: {e}");
                    complained = true;
                }
                tokio::time::sleep(Duration::from_millis(100)).await;
            }
            // Spurious readiness: the guard cleared it, go back to sleep.
            Err(_would_block) => {}
        }
    }
}

/// `AsyncFd` wants `AsRawFd`; nix's `Inotify` offers only `AsFd` and the
/// orphan rule keeps us from adding the impl — the same newtype dance
/// `server.rs` does for `VsockStream`. The `Arc` keeps the fd open for as
/// long as the reactor registration refers to it.
struct InotifyFd(Arc<Inotify>);

impl AsRawFd for InotifyFd {
    fn as_raw_fd(&self) -> RawFd {
        self.0.as_fd().as_raw_fd()
    }
}

// ----------------------------------------------------------------------
// Coalescing
// ----------------------------------------------------------------------

/// The ~150 ms accumulator. It deduplicates while it fills — a build
/// writing one file a hundred times is one `modified` — and stamps ids
/// only on the way out, so ids are dense, monotone per stream and usable
/// as the replay cursor the layer above turns into an SSE `Last-Event-ID`.
#[derive(Default)]
struct Batch {
    pending: Vec<Event>,
    seen: HashSet<(String, String, i32)>,
    next_id: u64,
}

impl Batch {
    fn is_empty(&self) -> bool {
        self.pending.is_empty()
    }

    /// Add one event. Returns true if the batch wants flushing now rather
    /// than at the end of the window.
    fn push(
        &mut self,
        kind: EventKind,
        dir: String,
        name: String,
        is_dir: bool,
        cookie: u32,
    ) -> bool {
        let key = (dir.clone(), name.clone(), kind as i32);
        if !self.seen.insert(key) {
            return false;
        }
        self.pending.push(Event {
            id: 0,
            kind: kind as i32,
            dir,
            name,
            is_dir,
            cookie,
            stale_paths: Vec::new(),
        });
        if self.pending.len() >= MAX_BATCH {
            // Too much to describe: collapse the whole batch into the
            // same signal an IN_Q_OVERFLOW produces.
            let dirs: Vec<String> = self
                .pending
                .iter()
                .map(|e| e.dir.clone())
                .collect::<HashSet<_>>()
                .into_iter()
                .collect();
            self.pending.clear();
            self.seen.clear();
            self.stale(dirs);
            return true;
        }
        false
    }

    /// Append a stale marker. Pending events stay: they are still true,
    /// and a client that re-reads the stale paths is not harmed by them.
    fn stale(&mut self, paths: Vec<String>) {
        self.pending.push(Event {
            id: 0,
            kind: EventKind::Stale as i32,
            dir: String::new(),
            name: String::new(),
            is_dir: false,
            cookie: 0,
            stale_paths: paths,
        });
    }

    /// Stamp ids and hand the batch over.
    fn take(&mut self) -> Vec<Event> {
        self.seen.clear();
        let mut out = std::mem::take(&mut self.pending);
        for ev in &mut out {
            self.next_id += 1;
            ev.id = self.next_id;
        }
        out
    }

    /// The id the next event will get minus one: what an ack reports as
    /// the cursor its listing corresponds to.
    fn cursor(&self) -> u64 {
        self.next_id
    }
}

// ----------------------------------------------------------------------
// The service
// ----------------------------------------------------------------------

/// The hub is created on first use: a guest where nobody ever subscribes
/// never spends an fd on inotify, and `WatchApiHandler::default()` stays
/// infallible.
#[derive(Default)]
pub struct WatchApiHandler {
    hub: Mutex<Option<Arc<Hub>>>,
}

impl WatchApiHandler {
    fn hub(&self) -> Result<Arc<Hub>, Status> {
        let mut slot = self.hub.lock().unwrap_or_else(|e| e.into_inner());
        if let Some(h) = slot.as_ref() {
            return Ok(h.clone());
        }
        let h = Hub::start().map_err(|e| Status::unavailable(format!("watch: {e}")))?;
        *slot = Some(h.clone());
        Ok(h)
    }
}

#[tonic::async_trait]
impl WatchService for WatchApiHandler {
    type WatchStream =
        Pin<Box<dyn tokio_stream::Stream<Item = Result<WatchResponse, Status>> + Send>>;

    async fn watch(
        &self,
        req: Request<Streaming<WatchRequest>>,
    ) -> Result<Response<Self::WatchStream>, Status> {
        let mut inbound = req.into_inner();
        let container = match inbound.message().await? {
            Some(WatchRequest {
                frame: Some(ReqFrame::Open(open)),
            }) => open.container,
            Some(_) => return Err(Status::invalid_argument("first frame must be `open`")),
            None => return Err(Status::invalid_argument("stream ended before `open`")),
        };
        // Resolve the anchor once before answering, so "no such container"
        // reaches the caller as a failed RPC rather than as a stream that
        // never says anything.
        toolfs::open_root(&container).map_err(Status::not_found)?;

        let hub = self.hub()?;
        let (id, tx, rx) = hub.attach();
        let (out, out_rx) = mpsc::unbounded_channel();

        // Inbound reader. Its only job is to hand complete path sets to
        // the pump; ending it ends the lease.
        let feed = tx.clone();
        tokio::spawn(async move {
            loop {
                match inbound.message().await {
                    Ok(Some(WatchRequest {
                        frame: Some(ReqFrame::Subscribe(sub)),
                    })) => {
                        if feed.send(Msg::Subscribe { paths: sub.paths }).is_err() {
                            break;
                        }
                    }
                    // A second `open`, or an empty frame: ignore rather
                    // than tear down a working subscription.
                    Ok(Some(_)) => continue,
                    Ok(None) | Err(_) => break,
                }
            }
            let _ = feed.send(Msg::End);
        });

        tokio::spawn(pump(hub, id, container, rx, out));
        Ok(Response::new(Box::pin(UnboundedReceiverStream::new(
            out_rx,
        ))))
    }
}

/// The single writer to one response stream: it holds the lease, owns the
/// coalescing window, and assigns every event id.
async fn pump(
    hub: Arc<Hub>,
    id: u64,
    container: String,
    mut rx: mpsc::UnboundedReceiver<Msg>,
    out: mpsc::UnboundedSender<Result<WatchResponse, Status>>,
) {
    let _lease = Lease {
        hub: hub.clone(),
        id,
    };
    let mut batch = Batch::default();
    let mut deadline: Option<Instant> = None;

    loop {
        tokio::select! {
            msg = rx.recv() => {
                let Some(msg) = msg else { break };
                match msg {
                    Msg::Fs { kind, dir, name, is_dir, cookie } => {
                        let now = batch.push(kind, dir, name, is_dir, cookie);
                        if now {
                            if !flush(&mut batch, &out) { break }
                            deadline = None;
                        } else if deadline.is_none() && !batch.is_empty() {
                            deadline = Some(Instant::now() + COALESCE);
                        }
                    }
                    Msg::Stale { paths } => {
                        // Latency matters more than batching here: the
                        // client is out of sync until it re-reads.
                        batch.stale(paths);
                        if !flush(&mut batch, &out) { break }
                        deadline = None;
                    }
                    Msg::Subscribe { paths } => {
                        // Flush first: the cursor in the ack must mean
                        // "everything before the new set is already sent",
                        // which is what lets the client list afterwards
                        // and miss nothing.
                        if !flush(&mut batch, &out) { break }
                        deadline = None;
                        let anchor = match toolfs::open_root(&container) {
                            Ok(a) => a,
                            Err(e) => {
                                let _ = out.send(Err(Status::not_found(e)));
                                break;
                            }
                        };
                        let done = hub.reconcile(id, &anchor, &paths);
                        let ack = WatchResponse {
                            frame: Some(RespFrame::Ack(Ack {
                                cursor: batch.cursor(),
                                watching: done.watching,
                                failed: done.failed,
                                watches: done.watches,
                                watch_limit: MAX_WATCHES as u32,
                            })),
                        };
                        if out.send(Ok(ack)).is_err() { break }
                    }
                    Msg::End => {
                        let _ = flush(&mut batch, &out);
                        break;
                    }
                }
            }
            _ = tokio::time::sleep_until(deadline.unwrap_or_else(Instant::now)), if deadline.is_some() => {
                deadline = None;
                if !flush(&mut batch, &out) { break }
            }
        }
    }
}

/// Send what the batch holds. Returns false when the caller is gone, which
/// is how the pump learns the lease is over.
fn flush(batch: &mut Batch, out: &mpsc::UnboundedSender<Result<WatchResponse, Status>>) -> bool {
    if batch.is_empty() {
        return true;
    }
    let events = batch.take();
    out.send(Ok(WatchResponse {
        frame: Some(RespFrame::Events(EventBatch { events })),
    }))
    .is_ok()
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::atomic::{AtomicU32, Ordering};

    static SEQ: AtomicU32 = AtomicU32::new(0);

    /// A scratch directory standing in for a container rootfs. The watch
    /// code only ever needs an anchor fd, never the payload layout, so
    /// these tests exercise the real syscalls without root and without
    /// redirecting the process-global payload root.
    fn scratch() -> std::path::PathBuf {
        let d = std::env::temp_dir().join(format!(
            "feos-watch-{}-{}",
            std::process::id(),
            SEQ.fetch_add(1, Ordering::Relaxed)
        ));
        let _ = std::fs::remove_dir_all(&d);
        std::fs::create_dir_all(&d).expect("scratch dir");
        d
    }

    fn anchor(p: &std::path::Path) -> File {
        File::open(p).expect("anchor")
    }

    /// Wait for the next message a stream gets, or give up. 2 s is far
    /// beyond inotify's own latency; a failure here is a lost event, not a
    /// slow one.
    async fn next(rx: &mut mpsc::UnboundedReceiver<Msg>) -> Msg {
        tokio::time::timeout(Duration::from_secs(2), rx.recv())
            .await
            .expect("no event within 2s")
            .expect("channel closed")
    }

    /// Drain until an event names `want`, skipping events for other
    /// entries. One `fs::write` is several inotify events (a create, then
    /// a modify, …) and this is the raw hub channel — the window that
    /// deduplicates them is one layer up, in [`Batch`].
    async fn expect_named(rx: &mut mpsc::UnboundedReceiver<Msg>, want: &str) {
        for _ in 0..64 {
            let m = next(rx).await;
            match &m {
                Msg::Fs { name, .. } if name == want => return,
                Msg::Fs { .. } => continue,
                other => panic!("expected an event for {want}, got {}", kind_of(other)),
            }
        }
        panic!("no event for {want} in 64 messages");
    }

    #[test]
    fn a_directory_resolves_to_itself_a_file_to_its_parent() {
        let dir = scratch();
        std::fs::create_dir_all(dir.join("workspace/src")).unwrap();
        std::fs::write(dir.join("workspace/src/main.rs"), b"fn main() {}").unwrap();
        let root = anchor(&dir);

        assert_eq!(
            resolve(&root, "/workspace/src").unwrap(),
            ("workspace/src".to_string(), None)
        );
        // A file is watched through its parent plus a name.
        assert_eq!(
            resolve(&root, "/workspace/src/main.rs").unwrap(),
            ("workspace/src".to_string(), Some("main.rs".to_string()))
        );
        // "Tell me when this appears" is a legal subscription.
        assert_eq!(
            resolve(&root, "/workspace/src/not-yet.rs").unwrap(),
            ("workspace/src".to_string(), Some("not-yet.rs".to_string()))
        );
        assert_eq!(resolve(&root, "/").unwrap(), (String::new(), None));
    }

    /// Confinement: the naive `rootfs.join(client_path)` would hand
    /// inotify_add_watch a path the payload can redirect with a symlink.
    /// Going through toolfs means no watch can ever land outside the
    /// container rootfs.
    #[test]
    fn resolution_stays_beneath_the_rootfs() {
        let dir = scratch();
        std::fs::create_dir_all(dir.join("w")).unwrap();
        let root = anchor(&dir);

        // Lexical escape, rejected before any syscall.
        assert!(resolve(&root, "../../etc").is_err());

        // A symlink out of the rootfs is not followed: it degrades to
        // "the entry named `escape` inside /w", which is a watch on /w —
        // never one on /etc.
        std::os::unix::fs::symlink("/etc", dir.join("w/escape")).unwrap();
        assert_eq!(
            resolve(&root, "/w/escape").unwrap(),
            ("w".to_string(), Some("escape".to_string()))
        );
        // And nothing can be reached *through* it.
        assert!(resolve(&root, "/w/escape/hostname").is_err());

        // The seam itself refuses the same escapes, since it is the seam
        // and not the caller that places the watch.
        let inotify = Inotify::init(InitFlags::IN_NONBLOCK | InitFlags::IN_CLOEXEC).unwrap();
        assert!(place(&inotify, &root, "../..").is_err());
        assert!(place(&inotify, &root, "w/escape").is_err());
        assert!(place(&inotify, &root, "w").is_ok());
    }

    #[tokio::test]
    async fn events_reach_a_subscriber() {
        let dir = scratch();
        std::fs::create_dir_all(dir.join("w")).unwrap();
        let hub = Hub::start().unwrap();
        let (id, _tx, mut rx) = hub.attach();
        let done = hub.reconcile(id, &anchor(&dir), &["/w".to_string()]);
        assert_eq!(done.watching, vec!["/w".to_string()]);
        assert!(done.failed.is_empty());
        assert_eq!(hub.watch_count(), 1);

        // Register, then act: the watch is in place before anything
        // happens, which is the ordering register-then-list insists on.
        std::fs::write(dir.join("w/a.txt"), b"hello").unwrap();
        match next(&mut rx).await {
            Msg::Fs {
                kind, dir, name, ..
            } => {
                assert_eq!(kind, EventKind::Created);
                assert_eq!(dir, "/w");
                assert_eq!(name, "a.txt");
            }
            _ => panic!("expected a create"),
        }
    }

    #[tokio::test]
    async fn two_streams_share_one_watch_and_the_first_release_does_not_blind_the_second() {
        let dir = scratch();
        std::fs::create_dir_all(dir.join("w")).unwrap();
        let hub = Hub::start().unwrap();

        let (a, _txa, mut rxa) = hub.attach();
        let (b, _txb, mut rxb) = hub.attach();
        hub.reconcile(a, &anchor(&dir), &["/w".to_string()]);
        hub.reconcile(b, &anchor(&dir), &["/w".to_string()]);
        // inotify deduplicates per inode: one watch, two holders.
        assert_eq!(hub.watch_count(), 1);

        std::fs::write(dir.join("w/a.txt"), b"1").unwrap();
        assert!(matches!(next(&mut rxa).await, Msg::Fs { .. }));
        assert!(matches!(next(&mut rxb).await, Msg::Fs { .. }));

        // Without the refcount this release would blind b.
        hub.detach(a);
        assert_eq!(hub.watch_count(), 1);
        std::fs::write(dir.join("w/b.txt"), b"2").unwrap();
        expect_named(&mut rxb, "b.txt").await;

        hub.detach(b);
        assert_eq!(hub.watch_count(), 0);
    }

    #[tokio::test]
    async fn a_file_subscription_filters_by_name() {
        let dir = scratch();
        std::fs::create_dir_all(dir.join("w")).unwrap();
        std::fs::write(dir.join("w/watched.txt"), b"x").unwrap();
        let hub = Hub::start().unwrap();
        let (id, _tx, mut rx) = hub.attach();
        let done = hub.reconcile(id, &anchor(&dir), &["/w/watched.txt".to_string()]);
        assert!(done.failed.is_empty());
        // The parent directory, not the file: one watch.
        assert_eq!(hub.watch_count(), 1);

        std::fs::write(dir.join("w/ignored.txt"), b"y").unwrap();
        std::fs::write(dir.join("w/watched.txt"), b"xx").unwrap();
        match next(&mut rx).await {
            Msg::Fs { name, dir, .. } => {
                assert_eq!(name, "watched.txt");
                assert_eq!(dir, "/w");
            }
            _ => panic!("expected the filtered file"),
        }

        // Subscribing to the directory as well widens the filter back to
        // every name, without taking a second watch.
        hub.reconcile(
            id,
            &anchor(&dir),
            &["/w/watched.txt".to_string(), "/w".to_string()],
        );
        assert_eq!(hub.watch_count(), 1);
        std::fs::write(dir.join("w/ignored.txt"), b"yy").unwrap();
        expect_named(&mut rx, "ignored.txt").await;
    }

    #[tokio::test]
    async fn a_subscription_replaces_the_previous_set() {
        let dir = scratch();
        std::fs::create_dir_all(dir.join("a")).unwrap();
        std::fs::create_dir_all(dir.join("b")).unwrap();
        let hub = Hub::start().unwrap();
        let (id, _tx, mut rx) = hub.attach();

        hub.reconcile(id, &anchor(&dir), &["/a".to_string(), "/b".to_string()]);
        assert_eq!(hub.watch_count(), 2);

        // The set is complete, not a delta — dropping /a from
        // it releases /a's watch and keeps /b's.
        let done = hub.reconcile(id, &anchor(&dir), &["/b".to_string()]);
        assert_eq!(done.watching, vec!["/b".to_string()]);
        assert_eq!(hub.watch_count(), 1);

        std::fs::write(dir.join("a/gone.txt"), b"x").unwrap();
        std::fs::write(dir.join("b/here.txt"), b"x").unwrap();
        match next(&mut rx).await {
            Msg::Fs { dir, name, .. } => {
                assert_eq!(dir, "/b");
                assert_eq!(name, "here.txt");
            }
            _ => panic!("expected only the still-subscribed directory"),
        }
    }

    #[tokio::test]
    async fn a_renamed_directory_goes_stale_instead_of_being_patched() {
        let dir = scratch();
        std::fs::create_dir_all(dir.join("w")).unwrap();
        let hub = Hub::start().unwrap();
        let (id, _tx, mut rx) = hub.attach();
        hub.reconcile(id, &anchor(&dir), &["/w".to_string()]);

        std::fs::rename(dir.join("w"), dir.join("moved")).unwrap();
        match next(&mut rx).await {
            Msg::Stale { paths } => assert_eq!(paths, vec!["/w".to_string()]),
            other => panic!("expected stale, got {}", kind_of(&other)),
        }
        // The descriptor is gone: a path it could name is a lie now
        //.
        assert_eq!(hub.watch_count(), 0);
    }

    /// The case `git checkout` makes ordinary: the directory
    /// is removed and recreated under the same name, and the watch does
    /// NOT follow it to the new inode. FeOS must say so instead of
    /// quietly re-arming — and must not keep a `wd → path` entry that
    /// would answer a later event with a path it no longer watches.
    #[tokio::test]
    async fn a_deleted_and_recreated_directory_goes_stale() {
        let dir = scratch();
        std::fs::create_dir_all(dir.join("w")).unwrap();
        let hub = Hub::start().unwrap();
        let (id, _tx, mut rx) = hub.attach();
        hub.reconcile(id, &anchor(&dir), &["/w".to_string()]);
        assert_eq!(hub.watch_count(), 1);

        std::fs::remove_dir(dir.join("w")).unwrap();
        // IN_DELETE_SELF reports the directory itself, IN_IGNORED then
        // says the watch is gone.
        let mut saw_stale = false;
        for _ in 0..4 {
            match next(&mut rx).await {
                Msg::Stale { paths } => {
                    assert_eq!(paths, vec!["/w".to_string()]);
                    saw_stale = true;
                    break;
                }
                Msg::Fs { kind, dir, .. } => {
                    assert_eq!(kind, EventKind::Deleted);
                    assert_eq!(dir, "/w");
                }
                other => panic!("unexpected {}", kind_of(&other)),
            }
        }
        assert!(saw_stale, "IN_IGNORED must reach the client as stale");
        assert_eq!(hub.watch_count(), 0, "no dangling wd → path entry");

        // Recreating the directory does not re-arm anything by itself;
        // the client's next complete path set does.
        std::fs::create_dir(dir.join("w")).unwrap();
        std::fs::write(dir.join("w/new.txt"), b"x").unwrap();
        assert_eq!(hub.watch_count(), 0);

        hub.reconcile(id, &anchor(&dir), &["/w".to_string()]);
        assert_eq!(hub.watch_count(), 1);
        std::fs::write(dir.join("w/after.txt"), b"x").unwrap();
        expect_named(&mut rx, "after.txt").await;
    }

    #[tokio::test]
    async fn unresolvable_paths_fail_individually() {
        let dir = scratch();
        std::fs::create_dir_all(dir.join("w")).unwrap();
        let hub = Hub::start().unwrap();
        let (id, _tx, _rx) = hub.attach();
        let done = hub.reconcile(
            id,
            &anchor(&dir),
            &["/w".to_string(), "../escape".to_string()],
        );
        // One bad path in a tree view must not take the subscription down.
        assert_eq!(done.watching, vec!["/w".to_string()]);
        assert_eq!(done.failed.len(), 1);
        assert_eq!(done.failed[0].path, "../escape");
    }

    fn kind_of(m: &Msg) -> &'static str {
        match m {
            Msg::Fs { .. } => "fs",
            Msg::Stale { .. } => "stale",
            Msg::Subscribe { .. } => "subscribe",
            Msg::End => "end",
        }
    }

    #[test]
    fn a_batch_deduplicates_and_numbers_its_events() {
        let mut b = Batch::default();
        assert!(!b.push(EventKind::Modified, "/w".into(), "a".into(), false, 0));
        // A build writing one file a hundred times is one `modified`.
        assert!(!b.push(EventKind::Modified, "/w".into(), "a".into(), false, 0));
        assert!(!b.push(EventKind::Created, "/w".into(), "b".into(), false, 0));
        let first = b.take();
        assert_eq!(first.len(), 2);
        assert_eq!(first[0].id, 1);
        assert_eq!(first[1].id, 2);
        assert_eq!(b.cursor(), 2);

        // Ids keep climbing across batches: they are a replay cursor, not
        // an index into one frame.
        b.push(EventKind::Created, "/w".into(), "c".into(), false, 0);
        let second = b.take();
        assert_eq!(second[0].id, 3);
        assert!(b.is_empty());
    }

    #[test]
    fn a_batch_too_large_to_describe_becomes_stale() {
        let mut b = Batch::default();
        let mut flushed_now = false;
        for i in 0..(MAX_BATCH + 1) {
            flushed_now = b.push(EventKind::Created, "/w".into(), format!("f{i}"), false, 0);
            if flushed_now {
                break;
            }
        }
        assert!(flushed_now, "the cap must ask for an immediate flush");
        let out = b.take();
        assert_eq!(out.len(), 1);
        assert_eq!(out[0].kind, EventKind::Stale as i32);
        assert_eq!(out[0].stale_paths, vec!["/w".to_string()]);
    }

    #[test]
    fn stale_keeps_the_events_already_collected() {
        let mut b = Batch::default();
        b.push(EventKind::Created, "/w".into(), "a".into(), false, 0);
        b.stale(vec!["/other".into()]);
        let out = b.take();
        assert_eq!(out.len(), 2);
        assert_eq!(out[1].kind, EventKind::Stale as i32);
        assert_eq!(out[1].stale_paths, vec!["/other".to_string()]);
    }
}
