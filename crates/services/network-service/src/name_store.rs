// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! The name tables the node has, kept across restarts of feosd: a file
//! per table, `<id>.pb`, the table as protobuf, replaced by a rename.

use crate::names::{self, Table};
use crate::store::write;
use feos_proto::network_service::{NameTable, UpdateNameTableRequest};
use log::warn;
use prost::Message;
use std::collections::{BTreeMap, BTreeSet};
use std::fs;
use std::io;
use std::net::IpAddr;
use std::path::{Path, PathBuf};
use tonic::Status;

pub struct NameStore {
    dir: PathBuf,
    tables: BTreeMap<String, Table>,
}

impl NameStore {
    /// The tables kept in `dir`. A file that does not read, or holds no
    /// valid table, is left alone, and logged.
    pub fn open(dir: impl Into<PathBuf>) -> io::Result<Self> {
        let dir = dir.into();
        fs::create_dir_all(&dir)?;
        let mut tables = BTreeMap::new();
        for entry in fs::read_dir(&dir)? {
            let path = entry?.path();
            let Some(id) = path
                .file_name()
                .and_then(|name| name.to_str())
                .and_then(|name| name.strip_suffix(".pb"))
            else {
                continue;
            };
            match load(&path)
                .and_then(|table| names::normalize(table).map_err(|e| e.message().to_string()))
            {
                Ok(table) if table.id == id => {
                    tables.insert(table.id.clone(), Table::new(table));
                }
                Ok(table) => warn!("Name tables: {} holds table {:?}", path.display(), table.id),
                Err(e) => warn!("Name tables: {}: {e}", path.display()),
            }
        }
        Ok(Self { dir, tables })
    }

    pub fn get(&self, id: &str) -> Option<&NameTable> {
        self.tables.get(id).map(|table| &table.table)
    }

    /// The tables, by ID.
    pub fn tables(&self) -> impl Iterator<Item = &NameTable> {
        self.tables.values().map(|table| &table.table)
    }

    /// The addresses of `name`, a label in lower case, in the first of
    /// `ids` that holds it: each table's own names first, then those of
    /// the tables it includes, in their order.
    pub fn lookup(&self, ids: &[String], name: &str) -> Option<&[IpAddr]> {
        for id in ids {
            let Some(table) = self.tables.get(id) else {
                continue;
            };
            if let Some(addresses) = table.names.get(name) {
                return Some(addresses);
            }
            for include in &table.table.includes {
                if let Some(addresses) = self
                    .tables
                    .get(include)
                    .and_then(|included| included.names.get(name))
                {
                    return Some(addresses);
                }
            }
        }
        None
    }

    /// Puts a table whole, whatever the version of the one there; whether
    /// that changed it.
    pub fn put(&mut self, table: NameTable) -> Result<bool, Status> {
        let table = names::normalize(table)?;
        if self.get(&table.id) == Some(&table) {
            return Ok(false);
        }
        self.save(&table)?;
        self.tables.insert(table.id.clone(), Table::new(table));
        Ok(true)
    }

    /// Puts and removes entries, if the table is at `from_version`.
    /// Removing a name it does not have changes nothing.
    pub fn update(&mut self, request: &UpdateNameTableRequest) -> Result<(), Status> {
        let id = &request.id;
        names::check_table_id(id)?;
        if request.version == request.from_version {
            return Err(Status::invalid_argument(format!(
                "version {} is the version the update starts from",
                request.version
            )));
        }
        let mut put = BTreeMap::new();
        for entry in &request.put_entries {
            let entry = names::normalize_entry(entry)?;
            if put.insert(entry.name.clone(), entry).is_some() {
                return Err(Status::invalid_argument(format!(
                    "the update puts a name twice in name table {id}"
                )));
            }
        }
        let remove = request
            .remove_names
            .iter()
            .map(|name| names::label(name))
            .collect::<Result<BTreeSet<String>, Status>>()?;
        if let Some(both) = remove.iter().find(|name| put.contains_key(*name)) {
            return Err(Status::invalid_argument(format!(
                "name {both} is both put and removed"
            )));
        }
        let Some(table) = self.get(id) else {
            return Err(Status::failed_precondition(format!(
                "name table {id} is not on the node"
            )));
        };
        if table.version != request.from_version {
            return Err(Status::failed_precondition(format!(
                "name table {id} is at version {}, not {}",
                table.version, request.from_version
            )));
        }
        let mut entries: BTreeMap<String, _> = table
            .entries
            .iter()
            .map(|entry| (entry.name.clone(), entry.clone()))
            .collect();
        entries.retain(|name, _| !remove.contains(name));
        entries.extend(put);
        let updated = NameTable {
            version: request.version,
            entries: entries.into_values().collect(),
            ..table.clone()
        };
        self.save(&updated)?;
        self.tables.insert(id.clone(), Table::new(updated));
        Ok(())
    }

    /// Removes a table, unless `in_use` says a pod on the node names it.
    pub fn delete(&mut self, id: &str, in_use: impl FnOnce(&str) -> bool) -> Result<(), Status> {
        if !self.tables.contains_key(id) {
            return Err(Status::not_found(format!(
                "name table {id} is not on the node"
            )));
        }
        if in_use(id) {
            return Err(Status::failed_precondition(format!(
                "a pod on the node names name table {id}"
            )));
        }
        match fs::remove_file(self.path(id)) {
            Err(e) if e.kind() != io::ErrorKind::NotFound => {
                return Err(Status::internal(format!(
                    "cannot remove name table {id}: {e}"
                )));
            }
            _ => {}
        }
        self.tables.remove(id);
        Ok(())
    }

    fn save(&self, table: &NameTable) -> Result<(), Status> {
        write(&self.dir, &self.path(&table.id), &table.encode_to_vec())
            .map_err(|e| Status::internal(format!("cannot keep name table {}: {e}", table.id)))
    }

    fn path(&self, id: &str) -> PathBuf {
        self.dir.join(format!("{id}.pb"))
    }
}

fn load(path: &Path) -> Result<NameTable, String> {
    let bytes = fs::read(path).map_err(|e| format!("does not read: {e}"))?;
    NameTable::decode(bytes.as_slice()).map_err(|e| format!("does not decode: {e}"))
}

#[cfg(test)]
mod tests {
    use super::*;
    use feos_proto::network_service::NameEntry;
    use tonic::Code;

    fn entry(name: &str, addresses: &[&str]) -> NameEntry {
        NameEntry {
            name: name.to_string(),
            addresses: addresses.iter().map(|a| a.to_string()).collect(),
        }
    }

    fn table(id: &str, version: u64, entries: Vec<NameEntry>, includes: &[&str]) -> NameTable {
        NameTable {
            id: id.to_string(),
            version,
            entries,
            includes: includes.iter().map(|i| i.to_string()).collect(),
        }
    }

    fn update(
        id: &str,
        from_version: u64,
        version: u64,
        put: Vec<NameEntry>,
        remove: &[&str],
    ) -> UpdateNameTableRequest {
        UpdateNameTableRequest {
            id: id.to_string(),
            from_version,
            version,
            put_entries: put,
            remove_names: remove.iter().map(|n| n.to_string()).collect(),
        }
    }

    fn ips(addresses: &[&str]) -> Vec<IpAddr> {
        addresses.iter().map(|a| a.parse().unwrap()).collect()
    }

    #[test]
    fn tables_are_kept_across_restarts() {
        let dir = tempfile::tempdir().unwrap();
        let mut store = NameStore::open(dir.path()).unwrap();
        let web = || entry("web", &["fd00::2", "fd00::1"]);
        assert!(store.put(table("env-1", 1, vec![web()], &[])).unwrap());
        assert!(store
            .put(table(
                "sbx-1",
                9,
                vec![entry("vite", &["127.0.0.1", "::1"])],
                &["env-1"]
            ))
            .unwrap());
        assert!(
            !store
                .put(table(
                    "env-1",
                    1,
                    vec![entry("WEB", &["fd00::1", "fd00::2"])],
                    &[]
                ))
                .unwrap(),
            "the same table"
        );
        store
            .update(&update("env-1", 1, 5, vec![entry("db", &["fd00::9"])], &[]))
            .unwrap();
        fs::write(dir.path().join("junk.pb"), b"\xff\xff").unwrap();
        fs::write(dir.path().join("README"), b"not a table").unwrap();
        fs::write(
            dir.path().join("other.pb"),
            table("env-1", 9, vec![], &[]).encode_to_vec(),
        )
        .unwrap();

        let store = NameStore::open(dir.path()).unwrap();
        let ids: Vec<&str> = store.tables().map(|t| t.id.as_str()).collect();
        assert_eq!(ids, ["env-1", "sbx-1"]);
        let env = store.get("env-1").unwrap();
        assert_eq!(env.version, 5);
        assert_eq!(
            env.entries,
            [
                entry("db", &["fd00::9"]),
                entry("web", &["fd00::1", "fd00::2"])
            ]
        );
        let sbx = [String::from("sbx-1")];
        assert_eq!(
            store.lookup(&sbx, "web").unwrap(),
            ips(&["fd00::1", "fd00::2"])
        );
    }

    #[test]
    fn a_tables_own_names_come_before_its_includes() {
        let dir = tempfile::tempdir().unwrap();
        let mut store = NameStore::open(dir.path()).unwrap();
        store
            .put(table(
                "env-a",
                1,
                vec![entry("db", &["fd00::a"]), entry("web", &["fd00::b"])],
                &["env-c"],
            ))
            .unwrap();
        store
            .put(table("env-c", 1, vec![entry("deep", &["fd00::c"])], &[]))
            .unwrap();
        store
            .put(table(
                "sbx-1",
                1,
                vec![entry("web", &["127.0.0.1", "::1"])],
                &["env-missing", "env-a"],
            ))
            .unwrap();
        let pod = |ids: &[&str]| ids.iter().map(|id| id.to_string()).collect::<Vec<_>>();
        let sbx = pod(&["sbx-1"]);
        assert_eq!(
            store.lookup(&sbx, "web").unwrap(),
            ips(&["127.0.0.1", "::1"])
        );
        assert_eq!(store.lookup(&sbx, "db").unwrap(), ips(&["fd00::a"]));
        assert_eq!(store.lookup(&sbx, "deep"), None, "includes of includes");
        assert_eq!(store.lookup(&sbx, "missing"), None);
        assert_eq!(
            store.lookup(&pod(&["env-a"]), "deep").unwrap(),
            ips(&["fd00::c"])
        );
        assert_eq!(
            store
                .lookup(&pod(&["nothing", "env-a", "sbx-1"]), "web")
                .unwrap(),
            ips(&["fd00::b"]),
            "the pod's tables in order"
        );
        assert_eq!(store.lookup(&[], "web"), None);
    }

    #[test]
    fn an_update_starts_at_the_version_there() {
        let dir = tempfile::tempdir().unwrap();
        let mut store = NameStore::open(dir.path()).unwrap();
        let error = store
            .update(&update(
                "env-1",
                1,
                2,
                vec![entry("web", &["fd00::1"])],
                &[],
            ))
            .unwrap_err();
        assert_eq!(error.code(), Code::FailedPrecondition, "no table");

        store
            .put(table(
                "env-1",
                10,
                vec![entry("web", &["fd00::1"])],
                &["env-2"],
            ))
            .unwrap();
        let error = store
            .update(&update("env-1", 2, 3, vec![], &["web"]))
            .unwrap_err();
        assert_eq!(error.code(), Code::FailedPrecondition, "wrong version");

        // Versions are the control plane's: they need not grow.
        store
            .update(&update(
                "env-1",
                10,
                4,
                vec![
                    entry("web", &["fd00::2", "fd00::3"]),
                    entry("db", &["fd00::9"]),
                ],
                &["gone"],
            ))
            .unwrap();
        let got = store.get("env-1").unwrap();
        assert_eq!(got.version, 4);
        assert_eq!(got.includes, ["env-2"]);
        assert_eq!(
            got.entries,
            [
                entry("db", &["fd00::9"]),
                entry("web", &["fd00::2", "fd00::3"])
            ]
        );
        store
            .update(&update("env-1", 4, 5, vec![], &["WEB"]))
            .unwrap();
        assert_eq!(
            store.get("env-1").unwrap().entries,
            [entry("db", &["fd00::9"])]
        );

        for (request, why) in [
            (update("env-1", 5, 5, vec![], &[]), "the same version"),
            (
                update("env-1", 5, 6, vec![entry("a.b", &["fd00::1"])], &[]),
                "bad name",
            ),
            (update("env-1", 5, 6, vec![], &["-x"]), "bad removal"),
            (
                update("env-1", 5, 6, vec![entry("x", &[])], &[]),
                "no address",
            ),
            (
                update(
                    "env-1",
                    5,
                    6,
                    vec![entry("x", &["fd00::1"]), entry("X", &["fd00::2"])],
                    &[],
                ),
                "put twice",
            ),
            (
                update("env-1", 5, 6, vec![entry("x", &["fd00::1"])], &["x"]),
                "put and removed",
            ),
            (update("", 5, 6, vec![], &[]), "no ID"),
        ] {
            let error = store.update(&request).unwrap_err();
            assert_eq!(error.code(), Code::InvalidArgument, "{why}");
        }
        assert_eq!(store.get("env-1").unwrap().version, 5);
    }

    #[test]
    fn a_table_in_use_stays() {
        let dir = tempfile::tempdir().unwrap();
        let mut store = NameStore::open(dir.path()).unwrap();
        store.put(table("env-1", 1, vec![], &[])).unwrap();
        let error = store.delete("env-1", |id| id == "env-1").unwrap_err();
        assert_eq!(error.code(), Code::FailedPrecondition);
        assert!(dir.path().join("env-1.pb").exists());

        store.delete("env-1", |_| false).unwrap();
        assert!(store.get("env-1").is_none());
        assert!(!dir.path().join("env-1.pb").exists());
        let error = store.delete("env-1", |_| false).unwrap_err();
        assert_eq!(error.code(), Code::NotFound);
        assert_eq!(NameStore::open(dir.path()).unwrap().tables().count(), 0);
        let error = store.put(table("a/b", 1, vec![], &[])).unwrap_err();
        assert_eq!(error.code(), Code::InvalidArgument);
    }
}
