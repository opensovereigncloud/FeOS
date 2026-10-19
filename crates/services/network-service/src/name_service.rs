// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! The node's name tables, shared: the store behind
//! `feos.network.v1.NameService`, the resolver's lookups in it, and the
//! question whether a pod names a table.

use crate::name_store::NameStore;
use feos_proto::network_service::{NameTable, UpdateNameTableRequest};
use log::info;
use std::io;
use std::net::IpAddr;
use std::path::PathBuf;
use std::sync::{Arc, Mutex, MutexGuard};
use tonic::Status;

type InUse = Arc<dyn Fn(&str) -> bool + Send + Sync>;

#[derive(Clone)]
pub struct NameTables {
    inner: Arc<Inner>,
}

struct Inner {
    store: Mutex<NameStore>,
    in_use: Mutex<InUse>,
}

impl NameTables {
    /// The tables kept in `dir`.
    pub fn open(dir: impl Into<PathBuf>) -> io::Result<Self> {
        let store = NameStore::open(dir)?;
        info!("Name tables: {} kept", store.tables().count());
        Ok(Self {
            inner: Arc::new(Inner {
                store: Mutex::new(store),
                in_use: Mutex::new(Arc::new(|_| false)),
            }),
        })
    }

    /// Tells the tables whether a pod on the node names a table, which a
    /// delete then refuses. It is asked without a lock of the tables held,
    /// so it may call them. Until it is set, no table is in use.
    pub fn set_in_use(&self, in_use: impl Fn(&str) -> bool + Send + Sync + 'static) {
        *self.inner.in_use.lock().unwrap_or_else(|e| e.into_inner()) = Arc::new(in_use);
    }

    /// The addresses of `name`, one label in any case, for a pod that
    /// names the tables `ids`: from the first of them that holds it, its
    /// own names before those of the tables it includes. None when none
    /// holds it.
    pub fn lookup(&self, ids: &[String], name: &str) -> Option<Vec<IpAddr>> {
        if ids.is_empty() {
            return None;
        }
        let name = name.to_ascii_lowercase();
        self.store().lookup(ids, &name).map(<[IpAddr]>::to_vec)
    }

    /// Of `ids`, those the node does not have, in their order: a pod that
    /// names one of them must not start.
    pub fn missing(&self, ids: &[String]) -> Vec<String> {
        let store = self.store();
        ids.iter()
            .filter(|id| store.get(id).is_none())
            .cloned()
            .collect()
    }

    pub fn get(&self, id: &str) -> Result<NameTable, Status> {
        self.store()
            .get(id)
            .cloned()
            .ok_or_else(|| Status::not_found(format!("name table {id} is not on the node")))
    }

    /// The tables, by ID.
    pub fn list(&self) -> Vec<NameTable> {
        self.store().tables().cloned().collect()
    }

    pub fn put(&self, table: NameTable) -> Result<(), Status> {
        let (id, version, names) = (table.id.clone(), table.version, table.entries.len());
        if self.store().put(table)? {
            info!("Name tables: {id} put at version {version}, {names} names");
        }
        Ok(())
    }

    pub fn update(&self, request: &UpdateNameTableRequest) -> Result<(), Status> {
        self.store().update(request)?;
        info!(
            "Name tables: {} updated to version {}, {} names put, {} removed",
            request.id,
            request.version,
            request.put_entries.len(),
            request.remove_names.len()
        );
        Ok(())
    }

    pub fn delete(&self, id: &str) -> Result<(), Status> {
        let in_use = self
            .inner
            .in_use
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .clone();
        let in_use = in_use(id);
        self.store().delete(id, |_| in_use)?;
        info!("Name tables: {id} deleted");
        Ok(())
    }

    fn store(&self) -> MutexGuard<'_, NameStore> {
        self.inner.store.lock().unwrap_or_else(|e| e.into_inner())
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use feos_proto::network_service::NameEntry;
    use tonic::Code;

    fn table(id: &str, names: &[(&str, &str)]) -> NameTable {
        NameTable {
            id: id.to_string(),
            version: 1,
            entries: names
                .iter()
                .map(|(name, address)| NameEntry {
                    name: name.to_string(),
                    addresses: vec![address.to_string()],
                })
                .collect(),
            includes: vec![],
        }
    }

    #[test]
    fn lookups_take_any_case() {
        let dir = tempfile::tempdir().unwrap();
        let tables = NameTables::open(dir.path()).unwrap();
        tables.put(table("env-1", &[("web", "fd00::1")])).unwrap();
        let ids = ["env-1".to_string()];
        let web = vec!["fd00::1".parse::<IpAddr>().unwrap()];
        assert_eq!(tables.lookup(&ids, "WeB"), Some(web.clone()));
        assert_eq!(tables.lookup(&ids, "db"), None);
        assert_eq!(tables.lookup(&[], "web"), None);
        assert_eq!(tables.missing(&["env-2".into(), "env-1".into()]), ["env-2"]);
        assert_eq!(
            NameTables::open(dir.path()).unwrap().lookup(&ids, "web"),
            Some(web)
        );
    }

    #[test]
    fn a_table_a_pod_names_stays() {
        let dir = tempfile::tempdir().unwrap();
        let tables = NameTables::open(dir.path()).unwrap();
        tables.put(table("env-1", &[])).unwrap();
        tables.put(table("env-2", &[])).unwrap();
        let names = {
            let tables = tables.clone();
            // As the pods would: it may ask the tables.
            move |id: &str| tables.missing(&[id.to_string()]).is_empty() && id == "env-1"
        };
        tables.set_in_use(names);
        assert_eq!(
            tables.delete("env-1").unwrap_err().code(),
            Code::FailedPrecondition
        );
        tables.delete("env-2").unwrap();
        assert_eq!(tables.get("env-2").unwrap_err().code(), Code::NotFound);
        assert_eq!(tables.list().len(), 1);
    }
}
