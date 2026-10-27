// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! Guest memory accounting and the pre-pause purge.
//!
//! Two jobs, both in service of the snapshot path:
//!
//! 1. **Introspection** — answer "who holds the RAM that Firecracker is
//!    about to write out?". /proc/meminfo alone cannot: it only describes
//!    the memory the page allocator manages. The `struct page` array
//!    (memmap — 64 B per 4 KiB page, so 64 MiB for a 4 GiB VM) and the
//!    kernel image itself sit *below* `MemTotal` and are invisible there,
//!    yet they occupy the guest's address space and land in every snapshot.
//!    We reconstruct them from /proc/iomem.
//!
//! 2. **Purge** — `sync` + `drop_caches` + optional compaction need
//!    CAP_SYS_ADMIN in the initial user namespace, which a payload
//!    container does not have. FeOS is PID 1 and does, so the host asks
//!    us instead of exec'ing into the container.
//!
//! Everything here is /proc and /sys parsing plus syscalls (nix).

use std::collections::HashMap;
use std::fs;
use std::io::Write;
use std::time::Instant;

use crate::error::HostError;
use feos_proto::host_service::GetMemoryBreakdownRequest;
use log::error;
use tokio::sync::oneshot;

use feos_proto::host_service::{
    MemoryBreakdown, PreparePauseRequest, PreparePauseResponse, ProcessMemory, SlabCache,
};

const PAGE_KB: u64 = 4;
/// Never let a free-page zeroing run push MemFree below this — PID 1 must
/// not be the process that discovers the OOM killer.
const ZERO_FLOOR_KB: u64 = 96 * 1024;
/// Touch this many bytes between MemFree re-checks during zeroing.
const ZERO_CHECK_CHUNK: usize = 32 * 1024 * 1024;

// ───────────────────────────── introspection ─────────────────────────────

/// Parse /proc/meminfo into its raw `key -> KiB` map. Values without a `kB`
/// unit (HugePages_* counts) are kept verbatim; nothing downstream uses
/// them as sizes.
fn read_meminfo() -> HashMap<String, u64> {
    let mut out = HashMap::new();
    let Ok(text) = fs::read_to_string("/proc/meminfo") else {
        return out;
    };
    for line in text.lines() {
        let Some((name, rest)) = line.split_once(':') else {
            continue;
        };
        let val = rest.split_whitespace().next().and_then(|v| v.parse().ok());
        if let Some(v) = val {
            out.insert(name.to_string(), v);
        }
    }
    out
}

/// Sum of "System RAM" ranges and the kernel image sections in /proc/iomem.
/// Returns (system_ram_kb, kernel_image_kb). Readable only as root — FeOS
/// is PID 1, so the addresses are real and not zeroed out.
fn read_iomem() -> (u64, u64) {
    let Ok(text) = fs::read_to_string("/proc/iomem") else {
        return (0, 0);
    };
    let (mut ram, mut kernel) = (0u64, 0u64);
    for line in text.lines() {
        let Some((range, label)) = line.split_once(" : ") else {
            continue;
        };
        let label = label.trim();
        let range = range.trim();
        let Some((start, end)) = range.split_once('-') else {
            continue;
        };
        let (Ok(start), Ok(end)) = (
            u64::from_str_radix(start.trim(), 16),
            u64::from_str_radix(end.trim(), 16),
        ) else {
            continue;
        };
        // /proc/iomem ranges are inclusive on both ends.
        let size_kb = (end.saturating_sub(start) + 1) / 1024;
        match label {
            // Top-level "System RAM" lines are not indented; the kernel
            // sections are nested inside them, so only count the outer ones.
            "System RAM" if !line.starts_with(' ') => ram += size_kb,
            "Kernel code" | "Kernel rodata" | "Kernel data" | "Kernel bss" => kernel += size_kb,
            _ => {}
        }
    }
    (ram, kernel)
}

/// Per-process RSS from /proc/<pid>/status. Kernel threads report no VmRSS
/// and are skipped — their stacks are accounted in meminfo's KernelStack.
fn read_processes() -> Vec<ProcessMemory> {
    let Ok(dir) = fs::read_dir("/proc") else {
        return Vec::new();
    };
    let mut out = Vec::new();
    for entry in dir.flatten() {
        let name = entry.file_name();
        let Some(pid) = name.to_str().and_then(|s| s.parse::<u32>().ok()) else {
            continue;
        };
        let Ok(status) = fs::read_to_string(format!("/proc/{pid}/status")) else {
            continue;
        };
        let mut p = ProcessMemory {
            pid,
            ..Default::default()
        };
        let mut has_rss = false;
        for line in status.lines() {
            let Some((key, rest)) = line.split_once(':') else {
                continue;
            };
            let val = || -> u64 {
                rest.split_whitespace()
                    .next()
                    .and_then(|v| v.parse().ok())
                    .unwrap_or(0)
            };
            match key {
                "Name" => p.name = rest.trim().to_string(),
                "VmRSS" => {
                    p.rss_kb = val();
                    has_rss = true;
                }
                "RssAnon" => p.rss_anon_kb = val(),
                "RssFile" => p.rss_file_kb = val(),
                "RssShmem" => p.rss_shmem_kb = val(),
                _ => {}
            }
        }
        if has_rss {
            out.push(p);
        }
    }
    out.sort_by_key(|p| std::cmp::Reverse(p.rss_kb));
    out
}

/// Largest slab caches from /proc/slabinfo (v2.1 column layout:
/// name active_objs num_objs objsize objperslab pagesperslab).
fn read_slabs(limit: usize) -> Vec<SlabCache> {
    let Ok(text) = fs::read_to_string("/proc/slabinfo") else {
        return Vec::new();
    };
    let mut out = Vec::new();
    for line in text.lines() {
        if line.starts_with('#') || line.starts_with("slabinfo") {
            continue;
        }
        let f: Vec<&str> = line.split_whitespace().collect();
        if f.len() < 6 {
            continue;
        }
        let (Ok(num_objs), Ok(objperslab), Ok(pagesperslab)) = (
            f[2].parse::<u64>(),
            f[4].parse::<u64>(),
            f[5].parse::<u64>(),
        ) else {
            continue;
        };
        if objperslab == 0 {
            continue;
        }
        let slabs = num_objs.div_ceil(objperslab);
        out.push(SlabCache {
            name: f[0].to_string(),
            size_kb: slabs * pagesperslab * PAGE_KB,
            objects: num_objs,
        });
    }
    out.sort_by_key(|s| std::cmp::Reverse(s.size_kb));
    out.truncate(limit);
    out
}

/// Full memory picture. `with_*` gate the expensive walks.
pub fn memory_breakdown(with_processes: bool, with_slabs: bool, with_raw: bool) -> MemoryBreakdown {
    let m = read_meminfo();
    let g = |k: &str| -> u64 { m.get(k).copied().unwrap_or(0) };
    let (system_ram_kb, kernel_image_kb) = read_iomem();
    let mem_total_kb = g("MemTotal");

    // Everything the kernel never gave the page allocator: memmap, early
    // reservations, the kernel image. Present in the snapshot, absent from
    // meminfo. Guard the subtraction — /proc/iomem may be unreadable.
    let memmap_reserved_kb = system_ram_kb
        .saturating_sub(mem_total_kb)
        .saturating_sub(kernel_image_kb);

    // Pages holding content that no purge can drop: total minus free minus
    // everything reclaimable (page cache, buffers, reclaimable slab).
    let unreclaimable_kb = mem_total_kb
        .saturating_sub(g("MemFree"))
        .saturating_sub(g("Cached"))
        .saturating_sub(g("Buffers"))
        .saturating_sub(g("SReclaimable"));

    MemoryBreakdown {
        mem_total_kb,
        mem_free_kb: g("MemFree"),
        mem_available_kb: g("MemAvailable"),
        buffers_kb: g("Buffers"),
        cached_kb: g("Cached"),
        anon_pages_kb: g("AnonPages"),
        mapped_kb: g("Mapped"),
        shmem_kb: g("Shmem"),
        slab_kb: g("Slab"),
        s_reclaimable_kb: g("SReclaimable"),
        s_unreclaim_kb: g("SUnreclaim"),
        kernel_stack_kb: g("KernelStack"),
        page_tables_kb: g("PageTables"),
        percpu_kb: g("Percpu"),
        vmalloc_used_kb: g("VmallocUsed"),
        dirty_kb: g("Dirty"),
        writeback_kb: g("Writeback"),
        system_ram_kb,
        kernel_image_kb,
        memmap_reserved_kb,
        unreclaimable_kb,
        processes: if with_processes {
            read_processes()
        } else {
            Vec::new()
        },
        top_slabs: if with_slabs {
            read_slabs(20)
        } else {
            Vec::new()
        },
        meminfo_kb: if with_raw { m } else { HashMap::new() },
    }
}

// ─────────────────────────────── the purge ───────────────────────────────

fn write_proc(path: &str, value: &str) -> std::io::Result<()> {
    let mut f = fs::OpenOptions::new().write(true).open(path)?;
    f.write_all(value.as_bytes())
}

fn mem_free_kb() -> u64 {
    read_meminfo().get("MemFree").copied().unwrap_or(0)
}

/// Hand every free page to the kernel once and give it straight back, so
/// the page returns to the free list holding zeroes instead of whatever the
/// last owner left there.
///
/// Why this exists: a stock-Firecracker full snapshot writes the whole
/// guest memory region, free pages included. A freed page keeps its old
/// content, so the dump carries megabytes of dead data that compresses
/// badly and cannot be punched out as holes. The kernel zeroes every page
/// it hands to userspace (anon faults are `__GFP_ZERO`), so allocating and
/// releasing the free pool rewrites it to zeroes.
///
/// Prefer balloon inflation or free page reporting when they are available
/// — those make the host drop the page outright (MADV_DONTNEED), which is
/// strictly better and does not dirty the page for diff snapshots. This is
/// the fallback for a VM without a working balloon.
///
/// Returns the number of KiB actually touched.
fn zero_free_pages(steps: &mut Vec<String>) -> u64 {
    use nix::sys::mman::{mmap_anonymous, munmap, MapFlags, ProtFlags};
    use std::num::NonZeroUsize;

    let free_kb = mem_free_kb();
    let budget_kb = free_kb.saturating_sub(ZERO_FLOOR_KB);
    if budget_kb == 0 {
        steps.push(format!(
            "zero_free_pages: skipped (MemFree {free_kb} kB at or below the {ZERO_FLOOR_KB} kB floor)"
        ));
        return 0;
    }
    let Some(len) = NonZeroUsize::new((budget_kb * 1024) as usize) else {
        return 0;
    };

    // MAP_NORESERVE: we are deliberately claiming almost all of free
    // memory, and we bail out ourselves below rather than letting the
    // overcommit heuristic decide.
    let ptr = match unsafe {
        mmap_anonymous(
            None,
            len,
            ProtFlags::PROT_READ | ProtFlags::PROT_WRITE,
            MapFlags::MAP_PRIVATE | MapFlags::MAP_NORESERVE,
        )
    } {
        Ok(p) => p,
        Err(e) => {
            steps.push(format!("zero_free_pages: mmap {budget_kb} kB failed: {e}"));
            return 0;
        }
    };

    // Touch one byte per page. The byte we write is zero, so the page the
    // kernel just zeroed for us stays all-zero.
    let base = ptr.as_ptr() as *mut u8;
    let page = PAGE_KB as usize * 1024;
    let mut done = 0usize;
    while done < len.get() {
        let chunk_end = (done + ZERO_CHECK_CHUNK).min(len.get());
        let mut off = done;
        while off < chunk_end {
            // SAFETY: off < len, inside the mapping we just created.
            unsafe { base.add(off).write_volatile(0) };
            off += page;
        }
        done = chunk_end;
        // Re-check after every chunk: reclaim or another allocator could
        // have eaten into the headroom we measured up front.
        if mem_free_kb() < ZERO_FLOOR_KB {
            steps.push(format!(
                "zero_free_pages: stopped early at {} kB (MemFree hit the floor)",
                done / 1024
            ));
            break;
        }
    }

    // SAFETY: ptr/len are exactly what mmap_anonymous returned.
    if let Err(e) = unsafe { munmap(ptr, len.get()) } {
        steps.push(format!("zero_free_pages: munmap failed: {e}"));
    }
    (done / 1024) as u64
}

/// The pre-pause purge. Every step is best-effort and reported in
/// `steps`; a failure never fails the RPC, because a partially purged
/// guest is still snapshot-able and the caller wants the before/after
/// numbers either way.
pub fn prepare_pause(req: &PreparePauseRequest) -> PreparePauseResponse {
    let t0 = Instant::now();
    let before = memory_breakdown(false, false, false);
    let mut steps: Vec<String> = Vec::new();
    let mut zeroed_kb = 0;

    if !req.skip_sync {
        let t = Instant::now();
        nix::unistd::sync();
        steps.push(format!("sync ok ({} ms)", t.elapsed().as_millis()));
    }

    // Lower the reporting order *before* freeing, so the pages we are about
    // to release are walked at the new granularity. The default is
    // pageblock_order (2 MiB on x86_64 with THP), which skips every
    // smaller free fragment — most of what drop_caches produces.
    if let Some(order) = req.page_reporting_order {
        const PRO: &str = "/sys/module/page_reporting/parameters/page_reporting_order";
        match write_proc(PRO, &order.to_string()) {
            Ok(()) => steps.push(format!("page_reporting_order = {order}")),
            Err(e) => steps.push(format!("page_reporting_order: {e}")),
        }
    }

    if !req.skip_drop_caches {
        let t = Instant::now();
        match write_proc("/proc/sys/vm/drop_caches", "3") {
            Ok(()) => steps.push(format!("drop_caches=3 ok ({} ms)", t.elapsed().as_millis())),
            Err(e) => steps.push(format!("drop_caches: {e}")),
        }
    }

    if req.compact {
        let t = Instant::now();
        match write_proc("/proc/sys/vm/compact_memory", "1") {
            Ok(()) => steps.push(format!(
                "compact_memory ok ({} ms)",
                t.elapsed().as_millis()
            )),
            Err(e) => steps.push(format!("compact_memory: {e}")),
        }
    }

    if req.zero_free_pages {
        let t = Instant::now();
        zeroed_kb = zero_free_pages(&mut steps);
        steps.push(format!(
            "zero_free_pages: {} kB in {} ms",
            zeroed_kb,
            t.elapsed().as_millis()
        ));
    }

    if req.settle_seconds > 0 {
        // Free page reporting is not synchronous: the guest arms a work
        // item and PAGE_REPORTING_DELAY (2 s) later walks the free lists.
        std::thread::sleep(std::time::Duration::from_secs(req.settle_seconds as u64));
        steps.push(format!("settled {} s", req.settle_seconds));
    }

    PreparePauseResponse {
        before: Some(before),
        after: Some(memory_breakdown(false, false, false)),
        steps,
        duration_ms: t0.elapsed().as_millis() as u64,
        zeroed_kb,
    }
}

pub async fn handle_get_memory_breakdown(
    req: GetMemoryBreakdownRequest,
    responder: oneshot::Sender<Result<MemoryBreakdown, HostError>>,
) {
    // Walking /proc is blocking file I/O; keep it off the async workers.
    let result = tokio::task::spawn_blocking(move || {
        memory_breakdown(req.with_processes, req.with_slabs, req.with_raw_meminfo)
    })
    .await
    .map_err(|e| HostError::Failed(format!("memory breakdown task: {e}")));
    if responder.send(result).is_err() {
        error!("HostWorker: Failed to send response for GetMemoryBreakdown.");
    }
}

pub async fn handle_prepare_pause(
    req: PreparePauseRequest,
    responder: oneshot::Sender<Result<PreparePauseResponse, HostError>>,
) {
    // sync(2) can take seconds and the settle window sleeps.
    let result = tokio::task::spawn_blocking(move || prepare_pause(&req))
        .await
        .map_err(|e| HostError::Failed(format!("prepare pause task: {e}")));
    if responder.send(result).is_err() {
        error!("HostWorker: Failed to send response for PreparePause.");
    }
}
