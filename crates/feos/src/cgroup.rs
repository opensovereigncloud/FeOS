// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! feosd's place in the cgroup hierarchy. VMs go into `vms/<id>` below the
//! cgroup feosd is given, the root under feos-host-init, or below the one
//! `--vms-cgroup` names: under systemd, feos-vms.service's, since a service
//! whose cgroup has controllers on for its children cannot start from
//! systemd 258 on. A cgroup whose children use controllers may hold no
//! processes itself, so feosd moves into a leaf of its own, `feosd`, unless
//! it is in the root, which may, or systemd put it there
//! (DelegateSubgroup=feosd).

use std::io;
use std::path::Path;

const CGROUP_ROOT: &str = "/sys/fs/cgroup";
const LEAF: &str = "feosd";
/// Where systemd runs a delegated service's commands besides the main one.
const CONTROL: &str = ".control";

/// Moves this process into its leaf. Returns the cgroup feosd is given,
/// relative to the root of the hierarchy: empty for the root.
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

/// The cgroup feosd is given, relative to the root of the hierarchy,
/// without moving: for a feosd in its leaf, or in the subgroup `.control`,
/// where systemd runs a delegated service's ExecStopPost=, their parent.
pub fn given() -> io::Result<String> {
    let own = own_cgroup(&std::fs::read_to_string("/proc/self/cgroup")?)
        .ok_or_else(|| io::Error::other("no cgroup v2 entry in /proc/self/cgroup"))?;
    Ok(given_by(&own))
}

fn given_by(own: &str) -> String {
    let path = Path::new(own);
    match path.file_name() {
        Some(name) if name == LEAF || name == CONTROL => path
            .parent()
            .map(|p| p.to_string_lossy().into_owned())
            .unwrap_or_default(),
        _ => own.to_string(),
    }
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
    fn the_given_cgroup_is_the_leafs_parent() {
        assert_eq!(
            given_by("system.slice/feosd.service/feosd"),
            "system.slice/feosd.service"
        );
        assert_eq!(
            given_by("system.slice/feosd.service/.control"),
            "system.slice/feosd.service"
        );
        assert_eq!(
            given_by("system.slice/feosd.service"),
            "system.slice/feosd.service"
        );
        assert_eq!(given_by(""), "");
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
