// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! A minimal ublk recovery repro, kept for kernel and flag questions, mirroring
//! examples/ramdisk.rs from libublk main: `mini-ublk add`, `mini-ublk
//! kill <pid>`+, `mini-ublk recover <id> --shape {ramdisk|diskd}`.
//! Extra storage demo omitted: serves zeros with a small std vec data.

use libublk::ctrl::UblkCtrl;
use libublk::helpers::IoBuf;
use libublk::io::{BufDesc, UblkDev, UblkQueue};
use libublk::{ctrl_async::UblkCtrlAsync, sys, UblkError, UblkFlags, UblkRuntime};
use std::rc::Rc;
use std::sync::Arc;

const DEPTH: u16 = 128;

fn init_task_ring() {
    let _ = libublk::io::ublk_init_task_ring(|cell| {
        use std::cell::RefCell;
        if cell.get().is_none() {
            let ring = libublk::io_uring::IoUring::<
                libublk::io_uring::squeue::Entry,
                libublk::io_uring::cqueue::Entry,
            >::builder()
            .setup_cqsize(128)
            .setup_coop_taskrun()
            .build(128)
            .map_err(UblkError::IOError)?;
            cell.set(RefCell::new(ring))
                .map_err(|_| UblkError::OtherError(-libc::EEXIST))?;
        }
        Ok(())
    });
}
const BUFSZ: u64 = 1 << 20;

async fn io_task(q: &UblkQueue, tag: u16, _dev: &Arc<UblkDev>) -> Result<(), UblkError> {
    let buf = IoBuf::<u8>::new(BUFSZ as usize);
    q.submit_io_prep_cmd(tag, BufDesc::Slice(buf.as_slice()), 0, Some(&buf))
        .await?;
    loop {
        let iod = q.get_iod(tag);
        let len = (iod.nr_sectors << 9) as i32;
        q.submit_io_commit_cmd(tag, BufDesc::Slice(buf.as_slice()), len)
            .await?;
    }
}

async fn serve(ctrl: Rc<UblkCtrlAsync>, inside_recover: Option<i32>) -> Result<(), String> {
    let tgt_init = |dev: &mut UblkDev| {
        dev.set_default_params(1 << 30);
        Ok(())
    };
    let ctrl_m = ctrl;
    if let Some(_prev) = inside_recover {
        // shape "diskd": recover AFTER rt.block_on has started
    }
    let dev = UblkDev::new_async(ctrl_m.get_name(), tgt_init, &ctrl_m)
        .map_err(|e| format!("dev init: {e}"))?;
    let dev: Arc<UblkDev> = dev.into();
    let q = Rc::new(UblkQueue::new(0, &dev).map_err(|e| format!("queue init: {e}"))?);

    let mut handles = Vec::new();
    for tag in 0..DEPTH {
        let q = q.clone();
        let dev = dev.clone();
        handles.push(tokio::task::spawn_local(async move {
            match io_task(&q, tag, &dev).await {
                Err(UblkError::QueueIsDown) | Ok(()) => {}
                Err(e) => eprintln!("io task {tag} failed: {e}"),
            }
        }));
    }
    eprintln!("[mini] io tasks spawned");
    {
        let ctrl = ctrl_m.clone();
        let dev = dev.clone();
        handles.push(tokio::task::spawn_local(async move {
            ctrl.configure_queue_async(&dev, 0, unsafe { libc::gettid() })
                .await
                .unwrap();
            ctrl.start_dev_async(&dev).await.unwrap();
            eprintln!("[mini] device up");
        }));
    }
    for h in handles {
        let _ = h.await;
    }
    Ok(())
}

fn main() -> Result<(), String> {
    let argv: Vec<String> = std::env::args().collect();
    match argv.get(1).map(String::as_str) {
        Some("add") => {
            init_task_ring();
            let rt = UblkRuntime::new().map_err(|e| e.to_string())?;
            rt.block_on(async move {
                let ctrl = Rc::new(
                    UblkCtrlAsync::new_async(
                        Some("mini".to_string()),
                        -1,
                        1,
                        DEPTH.into(),
                        (BUFSZ * DEPTH as u64 / DEPTH as u64) as u32,
                        (sys::UBLK_F_USER_RECOVERY | sys::UBLK_F_USER_RECOVERY_REISSUE) as u64,
                        0,
                        UblkFlags::UBLK_DEV_F_ADD_DEV,
                    )
                    .await
                    .map_err(|e| format!("ctrl create {e}"))?,
                );
                serve(ctrl, None).await
            })
        }
        Some("recover") => {
            let id: i32 = argv
                .get(2)
                .ok_or("recover <id> [shape]")?
                .parse()
                .map_err(|_| "bad id")?;
            let shape = argv.get(3).map(String::as_str).unwrap_or("ramdisk");
            match shape {
                "ramdisk" => {
                    // shape ramdisk: start_user_recover BEFORE the runtime
                    init_task_ring();
                    let ctrl = UblkCtrl::new_simple(id).map_err(|e| e.to_string())?;
                    ctrl.start_user_recover()
                        .map_err(|e| format!("start_user_recover: {e}"))?;
                    eprintln!("[mini] start_user_recover ok (outside rt)");
                    let rt = UblkRuntime::new().map_err(|e| e.to_string())?;
                    rt.block_on(async move {
                        let ctrl = Rc::new(
                            UblkCtrlAsync::new_async(
                                Some("mini".to_string()),
                                id,
                                1,
                                DEPTH.into(),
                                BUFSZ as u32,
                                0,
                                0,
                                UblkFlags::UBLK_DEV_F_RECOVER_DEV,
                            )
                            .await
                            .map_err(|e| format!("ctrl create {e}"))?,
                        );
                        serve(ctrl, None).await
                    })
                }
                "diskd" => {
                    // shape diskd: start_user_recover INSIDE rt.block_on
                    let rt = UblkRuntime::new().map_err(|e| e.to_string())?;
                    rt.block_on(async move {
                        let ctrl0 = UblkCtrl::new_simple(id).map_err(|e| e.to_string())?;
                        ctrl0
                            .start_user_recover()
                            .map_err(|e| format!("start_user_recover: {e}"))?;
                        eprintln!("[mini] start_user_recover ok (inside rt)");
                        let ctrl = Rc::new(
                            UblkCtrlAsync::new_async(
                                Some("mini".to_string()),
                                id,
                                1,
                                DEPTH.into(),
                                BUFSZ as u32,
                                0,
                                0,
                                UblkFlags::UBLK_DEV_F_RECOVER_DEV,
                            )
                            .await
                            .map_err(|e| format!("ctrl create {e}"))?,
                        );
                        serve(ctrl, Some(id)).await
                    })
                }
                _ => Err("bad shape".to_string()),
            }
        }
        _ => Err("usage: mini-ublk add | recover <id> [ramdisk|diskd]".to_string()),
    }
}
