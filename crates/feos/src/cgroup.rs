// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! feosd's place in the cgroup hierarchy. VMs go into `vms/<id>` below the
//! cgroup feosd is given: the root under feos-host-init, the cgroup systemd
//! delegates to feosd.service. A cgroup whose children use controllers may
//! hold no processes itself, so feosd moves into a leaf of its own,
//! `feosd`, unless it is in the root, which may, or systemd put it there
//! (DelegateSubgroup=feosd).

use std::io;
use std::path::Path;

const CGROUP_ROOT: &str = "/sys/fs/cgroup";
const LEAF: &str = "feosd";

/// Moves this process into its leaf. Returns the cgroup below which VMs
/// go, relative to the root of the hierarchy: empty for the root.
pub fn enter_leaf() -> io::Result<String> {
    let own = own_cgroup(&std::fs::read_to_string("/proc/self/cgroup")?)
        .ok_or_else(|| io::Error::other("no cgroup v2 entry in /proc/self/cgroup"))?;
    let (base, leaf) = place(&own);
    if let Some(leaf) = leaf {
        let leaf = Path::new(CGROUP_ROOT).join(leaf);
        std::fs::create_dir_all(&leaf)?;
        // "0" is the writing process, with all its threads.
        std::fs::write(leaf.join("cgroup.procs"), "0")?;
    }
    Ok(base)
}

/// The cgroup v2 path in /proc/self/cgroup, without its leading slash.
fn own_cgroup(proc_cgroup: &str) -> Option<String> {
    proc_cgroup
        .lines()
        .find_map(|line| line.strip_prefix("0::"))
        .map(|path| path.trim_start_matches('/').to_string())
}

/// For a process in `own`: the cgroup VMs go below, and the leaf it has to
/// move into, if any.
fn place(own: &str) -> (String, Option<String>) {
    if own.is_empty() {
        return (String::new(), None);
    }
    let path = Path::new(own);
    if path.file_name().is_some_and(|name| name == LEAF) {
        let base = path.parent().map(|p| p.to_string_lossy().into_owned());
        return (base.unwrap_or_default(), None);
    }
    (own.to_string(), Some(format!("{own}/{LEAF}")))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_v2_entry_names_the_cgroup() {
        let proc_cgroup = "0::/system.slice/feosd.service/feosd\n";
        assert_eq!(
            own_cgroup(proc_cgroup).as_deref(),
            Some("system.slice/feosd.service/feosd")
        );
        assert_eq!(own_cgroup("0::/\n").as_deref(), Some(""));
        assert_eq!(own_cgroup("1:name=systemd:/\n"), None);
    }

    #[test]
    fn vms_go_below_the_cgroup_feosd_is_given() {
        // Under feos-host-init.
        assert_eq!(place(""), (String::new(), None));
        // Under systemd with DelegateSubgroup=feosd.
        assert_eq!(
            place("system.slice/feosd.service/feosd"),
            ("system.slice/feosd.service".to_string(), None)
        );
        // Anywhere else, feosd moves into a leaf first.
        assert_eq!(
            place("system.slice/feosd.service"),
            (
                "system.slice/feosd.service".to_string(),
                Some("system.slice/feosd.service/feosd".to_string())
            )
        );
    }
}
