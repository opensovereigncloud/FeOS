// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! Device table (`diskd.state`): pod ↔ device rows. Single writer
//! (control plane), atomic commit; the recovery path at startup is the
//! reader.

use std::collections::BTreeMap;
use std::fs;
use std::path::{Path, PathBuf};

use serde::{Deserialize, Serialize};

#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct State {
    #[serde(default)]
    pub devices: BTreeMap<String, DeviceRow>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct DeviceRow {
    /// None when the device was created but the id is not yet known, or
    /// after a planned stop.
    pub dev_id: Option<i32>,
    /// Layer ids, L0 first.
    pub chain: Vec<String>,
    pub logical_size: u64,
    pub chunk_size: u32,
    pub has_top: bool,
    /// Number of leading chain entries that are shared/immutable
    /// (flatten-on-seal never merges below this depth; ADR 0011: fixed
    /// chain shape `<shared…, S, top>`). Rows without the field are [L0]
    /// or [L0, S], so the correct default is 1.
    #[serde(default = "default_shared_depth")]
    pub shared_depth: usize,
}

fn default_shared_depth() -> usize {
    1
}

impl State {
    pub fn path(state_dir: &Path) -> PathBuf {
        state_dir.join("diskd.state")
    }

    pub fn load(state_dir: &Path) -> std::io::Result<State> {
        match fs::read(Self::path(state_dir)) {
            Ok(buf) => serde_json::from_slice(&buf)
                .map_err(|e| std::io::Error::new(std::io::ErrorKind::InvalidData, e)),
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => Ok(State::default()),
            Err(e) => Err(e),
        }
    }

    pub fn store(state_dir: &Path, st: &State) -> std::io::Result<()> {
        let p = Self::path(state_dir);
        let tmp = p.with_extension("state.tmp");
        let buf = serde_json::to_vec_pretty(st)
            .map_err(|e| std::io::Error::new(std::io::ErrorKind::InvalidData, e))?;
        fs::write(&tmp, &buf)?;
        fs::File::open(&tmp)?.sync_all()?;
        fs::rename(&tmp, &p)?;
        Ok(())
    }
}
