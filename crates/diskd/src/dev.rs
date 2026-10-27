// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! Device lifecycle: one thread per device owns its libublk queue
//! runtime, its top fd, its chain snapshot, and its fetch worker.
//! Shared-nothing: no lock on the request path (clippy disallowed-types;
//! the control plane's single compliance comment lives in svc.rs).

#![deny(clippy::disallowed_types)]
#![cfg_attr(not(test), deny(clippy::unwrap_used, clippy::expect_used))]

use std::cell::RefCell;
use std::collections::HashMap;
use std::fs;
use std::os::unix::io::AsRawFd;
use std::path::{Path, PathBuf};
use std::rc::Rc;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{mpsc, Arc};
use std::time::Instant;

use disk_store::chunk::Hash;
use disk_store::manifest::{Entry, EntryKind, Manifest};
use libublk::ctrl::UblkCtrl;
use libublk::ctrl_async::UblkCtrlAsync;
use libublk::io::{UblkDev, UblkQueue};
use libublk::ops::{self, TgtFd};
use libublk::{BufDesc, UblkError, UblkFlags, UblkRuntime};
use tracing::{debug, info, warn};

use crate::stats::DevStats;

/// Queue shape (ADR 0011).
pub const QUEUE_DEPTH: u16 = 128;
pub const MAX_IO_BYTES: u32 = 1 << 20;
/// Spare SQ slots beyond the armed FETCHes (see `tgt_init`).
const SQ_HEADROOM: u16 = 16;

/// Shared with the control plane — atomics only.
pub struct DevShared {
    pub stats: DevStats,
    /// Set by the control plane to mark a planned stop (device deleted
    /// instead of disowned on queue teardown).
    pub stop: AtomicBool,
}

impl Default for DevShared {
    fn default() -> Self {
        DevShared {
            stats: DevStats::default(),
            stop: AtomicBool::new(false),
        }
    }
}

pub struct DeviceCtx {
    pub pod_id: String,
    pub state_dir: PathBuf,
    /// Chain with L0 first; newest layer last. Immutable snapshot; seal
    /// swaps the Arc wholesale.
    pub chain: Arc<Vec<Manifest>>,
    pub top: Option<PathBuf>,
    pub logical_size: u64,
    pub cache: Arc<disk_store::cache::Cache>,
    /// None → fetches fail with EIO (tiering disabled).
    pub s3: Option<Arc<disk_store::s3::S3>>,
    pub shared: Arc<DevShared>,
    /// Bounded parallelism of the S3 miss-hydration worker (the daemon
    /// flag `--s3-concurrency`).
    pub fetch_concurrency: usize,
}

pub struct StartedDevice {
    pub dev_id: i32,
    pub bdev: PathBuf,
    pub join: std::thread::JoinHandle<()>,
    pub shared: Arc<DevShared>,
}

enum Ready {
    Up(i32),
    Failed(String),
}

/// Spawn the device thread; block until the device is up (or failed).
pub fn spawn(ctx: DeviceCtx, recover: bool) -> Result<StartedDevice, String> {
    let (tx, rx) = mpsc::channel::<Ready>();
    let shared = ctx.shared.clone();
    let name = format!("diskd-dev-{}", ctx.pod_id);
    let join = std::thread::Builder::new()
        .name(name)
        .spawn(move || dev_thread(ctx, recover, tx))
        .map_err(|e| e.to_string())?;
    match rx.recv() {
        Ok(Ready::Up(id)) => Ok(StartedDevice {
            dev_id: id,
            bdev: PathBuf::from(format!("/dev/ublkb{id}")),
            join,
            shared,
        }),
        Ok(Ready::Failed(e)) => {
            let _ = join.join();
            Err(e)
        }
        Err(_) => Err("device thread died before reporting".into()),
    }
}

/// Stop a device and delete it (StopDisk semantics: the top stays).
pub fn stop(started: StartedDevice) -> Result<(), String> {
    started.shared.stop.store(true, Ordering::SeqCst);
    if let Ok(ctrl) = UblkCtrl::new_simple(started.dev_id) {
        // Quiesce first where the driver can (UBLK_F_QUIESCE, Linux
        // 6.15): it cancels the pending uring_cmds, so that the io tasks
        // unwind on QueueIsDown before kill_dev. Quiesce can race VM
        // teardown (block file still close-serving on the host side) →
        // transient EBUSY. Retry a few times. A driver without it is not
        // asked: the device is killed at once, as after a quiesce that
        // failed. The wedge of ublk_ctrl_del_dev's iou worker that a
        // kill with FETCHes armed met on 6.12 is avoided by serve()'s
        // explicit del_dev and dev_thread's leaked runtime.
        if driver_quiesces(&ctrl) {
            for attempt in 0..5u32 {
                match ctrl.quiesce_dev(3000) {
                    Ok(_) => break,
                    Err(e) => {
                        if attempt == 4 {
                            warn!(error = %e, dev = started.dev_id,
                                  "quiesce failed after retries; killing");
                        }
                        std::thread::sleep(std::time::Duration::from_millis(400));
                    }
                }
            }
        } else {
            debug!(
                dev = started.dev_id,
                "the ublk driver cannot quiesce; killing"
            );
        }
        let _ = ctrl.kill_dev();
    }
    started
        .join
        .join()
        .map_err(|_| "device thread panicked".to_string())
}

/// Whether the ublk driver can quiesce a device (UBLK_F_QUIESCE, Linux
/// 6.15), as its features say, which libublk asked the driver for when it
/// opened `ctrl` (UBLK_U_CMD_GET_FEATURES, Linux 6.5). Without them the
/// device is taken for one that cannot: a driver before 6.5 cannot, and a
/// query that failed is asked again by the next stop's handle.
fn driver_quiesces(ctrl: &UblkCtrl) -> bool {
    quiesces(ctrl.get_driver_features())
}

/// Whether the driver's `features`, if it told them, include the quiesce.
fn quiesces(features: Option<u64>) -> bool {
    features.is_some_and(|features| features & libublk::sys::UBLK_F_QUIESCE as u64 != 0)
}

fn dev_thread(ctx: DeviceCtx, recover: bool, ready: mpsc::Sender<Ready>) {
    let ready_for_closure = ready.clone();
    // PR_SET_IO_FLUSHER: a userspace block server must be allowed to dip
    // into memory reserves during reclaim/writeback (ADR 0011).
    unsafe {
        libc::prctl(57 /* PR_SET_IO_FLUSHER */, 1, 0, 0, 0);
    }
    let result = std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
        let stop_flag = ctx.shared.clone();
        let rt = match UblkRuntime::new() {
            Ok(r) => r,
            Err(e) => {
                let _ = ready.send(Ready::Failed(format!("runtime: {e}")));
                return;
            }
        };
        rt.block_on(async move {
            if let Err(e) = run_device(ctx, recover, &ready_for_closure).await {
                let _ = ready_for_closure.send(Ready::Failed(e));
            }
        });
        if stop_flag.stop.load(Ordering::SeqCst) {
            // Planned stop: the ctrl's Drop deletes the kernel device
            // while the queue runtime tears down its io_uring — that pair
            // deadlocks on this kernel (iou worker wedged in
            // ublk_ctrl_del_dev; observed on 6.12). Leak the runtime and
            // let process teardown collect it. Bounded: one runtime per
            // planned stop; the crash path never takes it. TODO: track
            // the upstream ublk/fix proper.
            std::mem::forget(rt);
        }
    }));
    if let Err(e) = result {
        let _ = ready.send(Ready::Failed(format!("io task panic: {e:?}")));
    }
}

async fn run_device(
    ctx: DeviceCtx,
    recover: bool,
    ready: &mpsc::Sender<Ready>,
) -> Result<(), String> {
    let dev_flags = if recover {
        UblkFlags::UBLK_DEV_F_RECOVER_DEV
    } else {
        UblkFlags::UBLK_DEV_F_ADD_DEV
    } | UblkFlags::UBLK_DEV_F_MLOCK_IO_BUFFER;
    let ublk_flags = (libublk::sys::UBLK_F_USER_RECOVERY
        | libublk::sys::UBLK_F_USER_RECOVERY_REISSUE
        | libublk::sys::UBLK_F_QUIESCE) as u64;

    let id = if recover {
        let prev = crate::state::State::load(&ctx.state_dir)
            .ok()
            .and_then(|s| s.devices.get(&ctx.pod_id).and_then(|r| r.dev_id))
            .ok_or_else(|| format!("no previous device id for {}", ctx.pod_id))?;
        let ctrl = UblkCtrl::new_simple(prev).map_err(|e| e.to_string())?;

        ctrl.start_user_recover()
            .map_err(|e| format!("start_user_recover(dev {prev}): {e}"))?;
        prev
    } else {
        -1
    };

    let ctrl = UblkCtrlAsync::new_async(
        Some("diskd".into()),
        id,
        1,
        QUEUE_DEPTH as u32,
        MAX_IO_BYTES,
        ublk_flags,
        0,
        dev_flags,
    )
    .await
    .map_err(|e| format!("ctrl create: {e}"))?;

    serve(ctrl, ctx, ready).await
}

async fn serve(
    ctrl: UblkCtrlAsync,
    ctx: DeviceCtx,
    ready: &mpsc::Sender<Ready>,
) -> Result<(), String> {
    let logical = ctx.logical_size;
    let pod_id = ctx.pod_id.clone();

    // The top fd is registered as fixed file 1 (queue-slot fd table).
    let top_file = match &ctx.top {
        Some(p) => Some(
            fs::OpenOptions::new()
                .read(true)
                .write(true)
                .open(p)
                .map_err(|e| e.to_string())?,
        ),
        None => None,
    };

    let tgt_init = |dev: &mut UblkDev| -> Result<(), UblkError> {
        if let Some(tf) = &top_file {
            let n = dev.tgt.nr_fds;
            dev.tgt.fds[n as usize] = tf.as_raw_fd();
            dev.tgt.nr_fds = n + 1;
        }
        dev.tgt.dev_size = logical;
        // SQ headroom over the tag count. libublk defaults the queue
        // ring to exactly `queue_depth` SQEs, and our QUEUE_DEPTH armed
        // FETCHes fill it to the last slot before the thread ever parks.
        // The reactor bridges control-ring completions into the queue
        // ring by pushing a PollAdd on the ctrl fd at park time; with a
        // full SQ that push fails silently and the park is bounded only
        // by PARK_SAFETY_SECS (1 s) — every control round trip taken
        // from the queue thread (configure_queue, start_dev, quiesce)
        // then costs a flat second. Headroom keeps a slot free.
        dev.tgt.sq_depth = QUEUE_DEPTH + SQ_HEADROOM;
        dev.tgt.cq_depth = QUEUE_DEPTH + SQ_HEADROOM;
        let info = dev.dev_info;
        dev.tgt.params = libublk::sys::ublk_params {
            types: libublk::sys::UBLK_PARAM_TYPE_BASIC | libublk::sys::UBLK_PARAM_TYPE_DISCARD,
            basic: libublk::sys::ublk_param_basic {
                // VOLATILE_CACHE: guest/VMM flushes become FLUSH requests
                // reaching us (ADR 0011). Logical sectors are 512 B
                // (hypervisors/GPT); physical stays 4 KiB.
                attrs: libublk::sys::UBLK_ATTR_VOLATILE_CACHE,
                logical_bs_shift: 9,
                physical_bs_shift: 12,
                io_opt_shift: 12,
                io_min_shift: 9,
                max_sectors: info.max_io_buf_bytes >> 9,
                dev_sectors: logical >> 9,
                ..Default::default()
            },
            discard: libublk::sys::ublk_param_discard {
                // Advertised so fstrim(1) reaches us; we
                // write explicit zeros into the top (never punch).
                discard_alignment: 4096,
                discard_granularity: 4096,
                max_discard_sectors: (MAX_IO_BYTES >> 9),
                max_write_zeroes_sectors: (MAX_IO_BYTES >> 9),
                max_discard_segments: 1,
                ..Default::default()
            },
            ..Default::default()
        };
        dev.set_target_json(serde_json::json!({"diskd": {"pod_id": pod_id}}));
        Ok(())
    };

    let dev = UblkDev::new_async(ctrl.get_name(), tgt_init, &ctrl)
        .map_err(|e| format!("dev init: {e}"))?;
    let dev = Arc::new(dev);
    let q = Rc::new(UblkQueue::new(0, &dev).map_err(|e| format!("queue init: {e}"))?);

    let t_bring = Instant::now();
    let fetch = FetchBridge::spawn(&ctx);
    let io = Rc::new(IoCtx {
        zero_fd: open_zero(&ctx.state_dir)?,
        pending: RefCell::new(HashMap::new()),
        resp: RefCell::new(fetch.resp_rx.take().ok_or("fetch resp taken")?),
        fetch,
        ctx,
    });

    let n_tags = ctrl.dev_info().queue_depth;
    let mut handles = Vec::new();
    for tag in 0..n_tags {
        let q = q.clone();
        let io = io.clone();
        handles.push(tokio::task::spawn_local(async move {
            match io_task(&q, tag, io).await {
                Err(UblkError::QueueIsDown) | Ok(()) => {}
                Err(e) => warn!(tag, error = %e, "io task failed"),
            }
        }));
    }

    // Drive configure+start from a sibling task (proven shape from
    // examples/ramdisk): END_USER_RECOVERY completes only once every
    // tag armed its FETCH; awaiting it from the root future wedges the
    // device thread — io tasks never get LocalSet ticks.
    let ctrl = Rc::new(ctrl);
    let (start_tx, start_rx) = tokio::sync::oneshot::channel::<Result<i32, String>>();
    {
        let ctrl = ctrl.clone();
        let dev = dev.clone();
        handles.push(tokio::task::spawn_local(async move {
            let r = async {
                ctrl.configure_queue_async(&dev, 0, unsafe { libc::gettid() })
                    .await
                    .map_err(|e| format!("configure queue: {e}"))?;
                ctrl.start_dev_async(&dev)
                    .await
                    .map_err(|e| format!("start_dev: {e}"))?;
                Ok(ctrl.dev_info().dev_id as i32)
            }
            .await;
            let _ = start_tx.send(r);
        }));
    }

    let dev_id = match start_rx.await {
        Ok(Ok(id)) => id,
        Ok(Err(e)) => {
            warn!(error = %e, "device start failed; leaving quiesced for a successor");
            let _ = ready.send(Ready::Failed(e));
            ctrl.disown();
            return Ok(());
        }
        Err(_) => {
            let _ = ready.send(Ready::Failed("start task vanished".into()));
            ctrl.disown();
            return Ok(());
        }
    };

    info!(pod = %io.ctx.pod_id, dev_id,
          bring_up_ms = t_bring.elapsed().as_millis() as u64, "device up");
    let _ = ready.send(Ready::Up(dev_id));

    for h in handles {
        let _ = h.await;
    }
    info!(pod = %io.ctx.pod_id, dev_id, "device queue down");

    if io.ctx.shared.stop.load(Ordering::SeqCst) {
        // Planned stop: delete the device explicitly while the queue
        // rings are still alive and this thread is sane. Relying on
        // ctrl's Drop (running as the block_on future drops AFTER
        // queue-down) wedged ublk_ctrl_del_dev's iou worker on this
        // 6.12 host; explicit-early delete avoids it.
        if let Err(e) = ctrl.del_dev_async_await().await {
            warn!(error = %e, dev_id, "explicit del_dev failed; leaving to drop");
        }
        ctrl.disown();
    } else {
        // Unplanned teardown (shutdown/crash handoff): leave the device
        // for a successor daemon to recover.
        ctrl.disown();
    }
    Ok(())
}

// ---- request path ------------------------------------------------------

enum FetchState {
    InFlight,
    Failed,
}

struct IoCtx {
    ctx: DeviceCtx,
    zero_fd: fs::File,
    /// hash → fetch state. RefCell: single-threaded queue runtime —
    /// device-local, not shared mutation.
    pending: RefCell<HashMap<Hash, FetchState>>,
    /// Fetch worker responses, drained on eventfd wakeups.
    resp: RefCell<mpsc::Receiver<(Hash, bool)>>,
    fetch: FetchBridge,
}

/// One io task per tag: prep → handle → commit, forever.
async fn io_task(q: &UblkQueue, tag: u16, io: Rc<IoCtx>) -> Result<(), UblkError> {
    let mut buf = libublk::helpers::IoBuf::<u8>::new(MAX_IO_BYTES as usize);
    q.submit_io_prep_cmd(tag, BufDesc::Slice(buf.as_slice()), 0, Some(&buf))
        .await?;

    loop {
        let iod = q.get_iod(tag);
        let op = iod.op_flags & 0xff;
        let off = iod.start_sector << 9;
        let bytes = iod.nr_sectors << 9;

        let t0 = Instant::now();
        let res = handle_io(&io, op, off, buf.as_mut_slice(), bytes as usize).await;
        let st = &io.ctx.shared.stats;
        match op {
            libublk::sys::UBLK_IO_OP_READ => {
                DevStats::bump(&st.reads);
                DevStats::add(&st.read_bytes, bytes as u64);
                st.read_hist.record(t0.elapsed());
            }
            libublk::sys::UBLK_IO_OP_WRITE => {
                st.write_hist.record(t0.elapsed());
            }
            libublk::sys::UBLK_IO_OP_FLUSH => {
                DevStats::bump(&st.flushes);
                st.flush_hist.record(t0.elapsed());
            }
            _ => {}
        }
        if res < 0 && res != -libc::EROFS {
            DevStats::bump(&st.errors);
        }

        q.submit_io_commit_cmd(tag, BufDesc::Slice(buf.as_slice()), res)
            .await?;
    }
}

async fn handle_io(io: &Rc<IoCtx>, op: u32, off: u64, buf: &mut [u8], bytes: usize) -> i32 {
    match op {
        libublk::sys::UBLK_IO_OP_READ => read_io(io, off, &mut buf[..bytes]).await,
        libublk::sys::UBLK_IO_OP_WRITE => write_io(io, off, &buf[..bytes]).await,
        libublk::sys::UBLK_IO_OP_FLUSH => flush_io(io).await,
        // discard/write-zeroes = explicit zero writes into the top
        // (punching holes would resurrect parent data — ADR 0011).
        libublk::sys::UBLK_IO_OP_DISCARD | libublk::sys::UBLK_IO_OP_WRITE_ZEROES => {
            zero_write_io(io, off, buf, bytes).await
        }
        _ => -libc::EOPNOTSUPP,
    }
}

/// Write (ADR 0011): goes to `top.img` only, at the same offset, from
/// the request's pages. Sealed layers are never written; no runtime CoW.
async fn write_io(io: &Rc<IoCtx>, off: u64, buf: &[u8]) -> i32 {
    let st = &io.ctx.shared.stats;
    DevStats::bump(&st.writes);
    DevStats::add(&st.write_bytes, buf.len() as u64);
    if off + buf.len() as u64 > io.ctx.logical_size {
        return -libc::EINVAL;
    }
    // The top fd is registered as fixed file 1 whenever a top exists.
    // SAFETY: `buf` is this tag's queue-slot buffer; the kernel copied
    // the request into it at fetch (6.12) and it outlives the await.
    let r = match unsafe { ops::write_at_raw(TgtFd::Fixed(1), buf.as_ptr(), buf.len() as u32, off) }
    {
        Ok(fut) => fut.await,
        Err(_) => -libc::EIO,
    };
    tracing::debug!(pod = io.ctx.pod_id, off, len = buf.len(), r, "write_io");
    r
}

async fn read_io(io: &Rc<IoCtx>, off: u64, buf: &mut [u8]) -> i32 {
    let logical = io.ctx.logical_size;
    if off >= logical {
        return -libc::EINVAL;
    }
    let end = (off + buf.len() as u64).min(logical);
    let bytes = end - off;

    // The top's block-granular data extents override the chain (holes
    // inherit). Split the request into top-covered and chain spans.
    let top_ranges: Vec<(u64, u64)> = match &io.ctx.top {
        Some(p) => match fs::File::open(p) {
            Ok(f) => match disk_store::seal::data_extents(&f, logical) {
                Ok(extents) => extents
                    .into_iter()
                    .filter_map(|(s, e)| {
                        let s = s.max(off);
                        let e = e.min(end);
                        (s < e).then_some((s, e))
                    })
                    .collect(),
                Err(_) => return -libc::EIO,
            },
            Err(_) => return -libc::EIO,
        },
        None => Vec::new(),
    };

    enum Src {
        Top,
        Chain,
    }
    let mut spans: Vec<(u64, u64, Src)> = Vec::new();
    let mut cursor = off;
    for (s, e) in top_ranges {
        if s > cursor {
            spans.push((cursor, s, Src::Chain));
        }
        spans.push((s, e, Src::Top));
        cursor = e;
    }
    if cursor < end {
        spans.push((cursor, end, Src::Chain));
    }

    let chain = io.ctx.chain.clone();
    for (s, e, src) in spans {
        let rel = (s - off) as usize;
        let sub = &mut buf[rel..rel + (e - s) as usize];
        let r = match src {
            Src::Top => read_top(io, s, sub).await,
            Src::Chain => read_chain(io, &chain, s, sub).await,
        };
        if r < 0 {
            return r;
        }
    }
    bytes as i32
}

async fn read_top(io: &Rc<IoCtx>, off: u64, buf: &mut [u8]) -> i32 {
    let Some(top) = &io.ctx.top else {
        return -libc::EIO;
    };
    let Ok(f) = fs::File::open(top) else {
        return -libc::EIO;
    };
    // SAFETY: `buf` is this tag's registered queue-slot buffer; it
    // outlives the await and is not used concurrently.
    match unsafe {
        ops::read_at_raw(
            TgtFd::Raw(f.as_raw_fd()),
            buf.as_mut_ptr(),
            buf.len() as u32,
            off,
        )
    } {
        Ok(fut) => fut.await,
        Err(_) => -libc::EIO,
    }
}

/// Chain-covered span, chunk by chunk: cache hit → raw-file read into
/// the request buffer; `zero`/absent → shared zero file; miss → S3.
async fn read_chain(io: &Rc<IoCtx>, chain: &[Manifest], off: u64, buf: &mut [u8]) -> i32 {
    let Some(head) = chain.last() else {
        return -libc::EIO;
    };
    let cs = head.chunk_size as u64;
    let logical = head.logical_size;
    let end = off + buf.len() as u64;
    let mut pos = off;
    while pos < end {
        let chunk_off = pos - (pos % cs);
        let in_chunk = (pos - chunk_off) as usize;
        let want = ((chunk_off + cs).min(end) - pos) as usize;
        let dst = &mut buf[(pos - off) as usize..(pos - off) as usize + want];

        if chunk_off >= logical {
            // Beyond the last logical chunk: zeros.
            let r = read_zero(io, dst).await;
            if r < 0 {
                return r;
            }
            pos += want as u64;
            continue;
        }

        let parents: Vec<&Manifest> = chain[..chain.len() - 1].iter().collect();
        match Manifest::resolve(head, &parents, chunk_off) {
            None
            | Some(Entry {
                kind: EntryKind::Zero,
                ..
            }) => {
                let r = read_zero(io, dst).await;
                if r < 0 {
                    return r;
                }
            }
            Some(Entry {
                kind: EntryKind::Data,
                hash,
                raw_len,
                ..
            }) => {
                let hash = Hash::from_bytes(*hash);
                let r = read_chunk(io, &hash, *raw_len, in_chunk, dst).await;
                if r < 0 {
                    return r;
                }
            }
        }
        pos += want as u64;
    }
    buf.len() as i32
}

async fn read_zero(io: &Rc<IoCtx>, buf: &mut [u8]) -> i32 {
    // SAFETY: queue-slot buffer; outlives the await.
    match unsafe {
        ops::read_at_raw(
            TgtFd::Raw(io.zero_fd.as_raw_fd()),
            buf.as_mut_ptr(),
            buf.len() as u32,
            0,
        )
    } {
        Ok(fut) => fut.await,
        Err(_) => -libc::EIO,
    }
}

async fn read_chunk(
    io: &Rc<IoCtx>,
    hash: &Hash,
    raw_len: u32,
    in_chunk: usize,
    dst: &mut [u8],
) -> i32 {
    let st = &io.ctx.shared.stats;
    if let Ok(f) = fs::File::open(io.ctx.cache.chunk_path(hash)) {
        io.ctx.cache.touch(hash);
        DevStats::bump(&st.cache_hits);
        // SAFETY: queue-slot buffer; outlives the await.
        return match unsafe {
            ops::read_at_raw(
                TgtFd::Raw(f.as_raw_fd()),
                dst.as_mut_ptr(),
                dst.len() as u32,
                in_chunk as u64,
            )
        } {
            Ok(fut) => fut.await,
            Err(_) => -libc::EIO,
        };
    }

    DevStats::bump(&st.cache_misses);
    match fetch_chunk(io, hash, raw_len).await {
        Ok(()) => {}
        Err(()) => {
            DevStats::bump(&st.errors);
            return -libc::EIO;
        }
    }
    match fs::File::open(io.ctx.cache.chunk_path(hash)) {
        Ok(f) => {
            // SAFETY: queue-slot buffer; outlives the await.
            match unsafe {
                ops::read_at_raw(
                    TgtFd::Raw(f.as_raw_fd()),
                    dst.as_mut_ptr(),
                    dst.len() as u32,
                    in_chunk as u64,
                )
            } {
                Ok(fut) => fut.await,
                Err(_) => -libc::EIO,
            }
        }
        Err(_) => {
            DevStats::bump(&st.errors);
            -libc::EIO
        }
    }
}

/// Ensure `hash` is in the local cache, fetching via the worker if not.
/// Concurrent tags coalesce on one fetch (`pending` map); wakeups arrive
/// as eventfd readiness polled inside the ring.
async fn fetch_chunk(io: &Rc<IoCtx>, hash: &Hash, raw_len: u32) -> Result<(), ()> {
    {
        let mut pending = io.pending.borrow_mut();
        match pending.get(hash) {
            None => {
                pending.insert(*hash, FetchState::InFlight);
                io.fetch.request(*hash, raw_len).map_err(|_| ())?;
            }
            Some(FetchState::InFlight) => {}
            Some(FetchState::Failed) => {
                pending.remove(hash);
                return Err(());
            }
        }
    }
    loop {
        // SAFETY: plain poll, no buffer.
        match ops::poll_add(TgtFd::Raw(io.fetch.eventfd_raw()), libc::POLLIN as u32) {
            Ok(fut) => {
                let _ = fut.await;
            }
            Err(_) => return Err(()),
        }
        // Drain worker responses; mark failures so waiters can EIO.
        while let Ok((h, ok)) = io.resp.borrow_mut().try_recv() {
            if !ok {
                io.pending.borrow_mut().insert(h, FetchState::Failed);
            }
        }
        if io.ctx.cache.contains(hash) {
            io.pending.borrow_mut().remove(hash);
            return Ok(());
        }
        if matches!(io.pending.borrow().get(hash), Some(FetchState::Failed)) {
            io.pending.borrow_mut().remove(hash);
            return Err(());
        }
    }
}

/// Discard/write-zeroes: zero-write the range into `top.img` —
/// never punch, so sealed-parent data cannot resurrect (ADR 0011,
/// store's `discard_no_resurrection` documents the trap). The range is
/// ≥ block-aligned by construction; writes are chunked at MAX_IO_BYTES.
async fn zero_write_io(io: &Rc<IoCtx>, off: u64, buf: &mut [u8], len: usize) -> i32 {
    let st = &io.ctx.shared.stats;
    DevStats::bump(&st.discards);
    if off + len as u64 > io.ctx.logical_size {
        return -libc::EINVAL;
    }
    let mut cursor = off;
    for chunk in buf[..len].chunks_mut(MAX_IO_BYTES as usize) {
        chunk.fill(0);
        // SAFETY: the zeroed slot buffer outlives each await.
        match unsafe {
            ops::write_at_raw(TgtFd::Fixed(1), chunk.as_ptr(), chunk.len() as u32, cursor)
        } {
            Ok(fut) => {
                let r = fut.await;
                if r < 0 {
                    return r;
                }
            }
            Err(_) => return -libc::EIO,
        }
        cursor += chunk.len() as u64;
    }
    len as i32
}

async fn flush_io(io: &Rc<IoCtx>) -> i32 {
    // The mutable top is the only runtime-writable file; a read-only
    // chain has nothing to flush.
    match &io.ctx.top {
        Some(p) => match fs::File::open(p) {
            Ok(f) => match ops::fsync(TgtFd::Raw(f.as_raw_fd()), true) {
                Ok(fut) => fut.await,
                Err(_) => -libc::EIO,
            },
            Err(_) => -libc::EIO,
        },
        None => 0,
    }
}

// ---- fetch bridge ------------------------------------------------------

/// One S3 fetch worker per device: a std thread with a tiny tokio
/// runtime (reqwest needs IO+time drivers), fed by an unbounded channel;
/// misses hydrate at bounded parallelism (`fetch_concurrency`) while the device thread never awaits more than one miss
/// per request (it coalesces on `pending`). Completions respond on a
/// std mpsc AND write an eventfd that io tasks poll via `poll_add` —
/// the wakeup stays ring-native.
struct FetchBridge {
    req: tokio::sync::mpsc::UnboundedSender<(Hash, u32)>,
    resp_rx: RefCell<Option<mpsc::Receiver<(Hash, bool)>>>,
    eventfd: Arc<std::os::unix::io::OwnedFd>,
}

fn eventfd_new() -> Arc<std::os::unix::io::OwnedFd> {
    use std::os::unix::io::FromRawFd;
    // SAFETY: eventfd() either returns a valid fd or -1 (asserted).
    let fd = unsafe { libc::eventfd(0, libc::EFD_CLOEXEC) };
    assert!(fd >= 0, "eventfd");
    Arc::new(unsafe { std::os::unix::io::OwnedFd::from_raw_fd(fd) })
}

impl FetchBridge {
    fn spawn(ctx: &DeviceCtx) -> FetchBridge {
        let (req, mut req_rx) = tokio::sync::mpsc::unbounded_channel::<(Hash, u32)>();
        let (resp_tx, resp_rx) = mpsc::channel::<(Hash, bool)>();
        let eventfd = eventfd_new();
        let cache = ctx.cache.clone();
        let s3 = ctx.s3.clone();
        let stats = ctx.shared.clone();
        let efd = eventfd.clone();
        let concurrency = ctx.fetch_concurrency.max(1);

        let name = format!("diskd-fetch-{}", ctx.pod_id);
        if let Err(e) = std::thread::Builder::new().name(name).spawn(move || {
            let rt = match tokio::runtime::Builder::new_current_thread()
                .enable_all()
                .build()
            {
                Ok(r) => r,
                Err(_) => return,
            };
            rt.block_on(async move {
                let local = tokio::task::LocalSet::new();
                local.spawn_local(async move {
                    let sem = Arc::new(tokio::sync::Semaphore::new(concurrency));
                    while let Some((hash, raw_len)) = req_rx.recv().await {
                        let cache = cache.clone();
                        let s3 = s3.clone();
                        let stats = stats.clone();
                        let resp_tx = resp_tx.clone();
                        let efd = efd.clone();
                        // Bounded parallelism: when the pool is full the
                        // loop awaits a permit; requests queue.
                        let Ok(permit) = sem.clone().acquire_owned().await else {
                            break;
                        };
                        tokio::task::spawn_local(async move {
                            let outcome = if cache.contains(&hash) {
                                true
                            } else {
                                match &s3 {
                                    Some(s3) => match s3.get_chunk(&hash, raw_len).await {
                                        Ok(raw) => cache
                                            .admit(&hash, &raw)
                                            .map(|_| {
                                                DevStats::bump(&stats.stats.s3_fetches);
                                                DevStats::add(
                                                    &stats.stats.s3_fetch_bytes,
                                                    raw.len() as u64,
                                                );
                                                true
                                            })
                                            .unwrap_or(false),
                                        Err(_) => false,
                                    },
                                    None => false,
                                }
                            };
                            drop(permit);
                            let _ = resp_tx.send((hash, outcome));
                            notify(&efd);
                        });
                    }
                });
                local.await;
            });
        }) {
            warn!(error = %e, "fetch worker spawn failed; fetches will EIO");
        }

        FetchBridge {
            req,
            resp_rx: RefCell::new(Some(resp_rx)),
            eventfd,
        }
    }

    fn request(&self, hash: Hash, raw_len: u32) -> Result<(), ()> {
        self.req.send((hash, raw_len)).map_err(|_| ())
    }

    fn eventfd_raw(&self) -> std::os::unix::io::RawFd {
        self.eventfd.as_raw_fd()
    }
}

fn notify(efd: &std::os::unix::io::OwnedFd) {
    let buf = 1u64.to_ne_bytes();
    // SAFETY: valid fd, valid buffer of 8 bytes.
    unsafe {
        libc::write(efd.as_raw_fd(), buf.as_ptr() as *const libc::c_void, 8);
    }
}

/// Shared zero file (ADR 0011): `zero` entries are served by reading
/// it, no chunk I/O. MAX_IO_BYTES covers any request.
fn open_zero(state_dir: &Path) -> Result<fs::File, String> {
    let p = state_dir.join("zero.bin");
    if !p.is_file() {
        let f = fs::File::create(&p).map_err(|e| e.to_string())?;
        f.set_len(MAX_IO_BYTES as u64).map_err(|e| e.to_string())?;
    }
    fs::File::open(&p).map_err(|e| e.to_string())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn a_stop_quiesces_as_its_handle_says() {
        let quiesce = libublk::sys::UBLK_F_QUIESCE as u64;
        let recovery = libublk::sys::UBLK_F_USER_RECOVERY as u64;
        // A query that failed decides nothing for the next stop.
        assert!(!quiesces(None), "a driver that did not tell");
        assert!(quiesces(Some(recovery | quiesce)));
        assert!(!quiesces(Some(recovery)), "a driver before 6.15");
    }
}
