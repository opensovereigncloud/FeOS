// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! A logger that writes to the kernel's log, /dev/kmsg, and to the console
//! until /dev is mounted.

use log::{Level, LevelFilter, Log, Metadata, Record};
use std::fs::File;
use std::io::Write;
use std::sync::Mutex;

struct Kmsg {
    device: Mutex<Option<File>>,
}

pub fn init() {
    let logger = Kmsg {
        device: Mutex::new(None),
    };
    if log::set_boxed_logger(Box::new(logger)).is_ok() {
        log::set_max_level(LevelFilter::Info);
    }
}

/// Lifts the kernel's limit on writes to /dev/kmsg, ten records in five
/// seconds per open file, past which it drops the rest and says so only
/// when the file is closed, which this logger never does. A
/// `printk.devkmsg=` on the kernel's command line locks the setting and
/// keeps the limit.
pub fn lift_limit() {
    // The kernel wants the newline, and refuses "on" without it.
    if let Err(e) = std::fs::write("/proc/sys/kernel/printk_devkmsg", "on\n") {
        log::warn!("The kernel's log keeps its limit on our writes: {e}");
    }
}

/// The syslog priority of a level.
fn priority(level: Level) -> u8 {
    match level {
        Level::Error => 3,
        Level::Warn => 4,
        Level::Info => 6,
        Level::Debug | Level::Trace => 7,
    }
}

impl Log for Kmsg {
    fn enabled(&self, metadata: &Metadata) -> bool {
        metadata.level() <= log::max_level()
    }

    fn log(&self, record: &Record) {
        if !self.enabled(record.metadata()) {
            return;
        }
        let line = format!(
            "<{}>feos-host-init: {}\n",
            priority(record.level()),
            record.args()
        );
        let mut device = self.device.lock().unwrap_or_else(|e| e.into_inner());
        if device.is_none() {
            *device = File::options().write(true).open("/dev/kmsg").ok();
        }
        // The kernel takes one record per write.
        let written = device
            .as_mut()
            .is_some_and(|kmsg| kmsg.write_all(line.as_bytes()).is_ok());
        if !written {
            let _ = std::io::stderr().write_all(line.as_bytes());
        }
    }

    fn flush(&self) {}
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn levels_map_to_syslog_priorities() {
        assert_eq!(priority(Level::Error), 3);
        assert_eq!(priority(Level::Info), 6);
        assert_eq!(priority(Level::Trace), 7);
    }
}
