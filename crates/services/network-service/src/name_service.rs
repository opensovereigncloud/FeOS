// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! The node's name tables, shared: the store behind
//! `feos.network.v1.NameService`, the resolver's lookups in it, and the
//! question whether a pod names a table.

use crate::kept::Kept;
use crate::name_store::NameStore;
use feos_proto::network_service::{NameTable, UpdateNameTableRequest};
use log::info;
use std::io;
use std::net::IpAddr;
use std::path::PathBuf;
use std::sync::{Arc, Mutex};
use tonic::Status;

type InUse = Arc<dyn Fn(&str) -> bool + Send + Sync>;

#[derive(Clone)]
pub struct NameTables {
    inner: Arc<Inner>,
}

struct Inner {
    store: Kept<NameStore>,
    in_use: Mutex<InUse>,
}

impl NameTables {
    /// The tables kept in `dir`.
    pub fn open(dir: impl Into<PathBuf>) -> io::Result<Self> {
        let store = NameStore::open(dir)?;
        info!("Name tables: {} kept", store.tables().count());
        Ok(Self {
            inner: Arc::new(Inner {
                store: Kept::new(store),
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

    pub async fn put(&self, table: NameTable) -> Result<(), Status> {
        let (id, version, names) = (table.id.clone(), table.version, table.entries.len());
        if self
            .inner
            .store
            .change(move |store| store.put(table))
            .await?
        {
            info!("Name tables: {id} put at version {version}, {names} names");
        }
        Ok(())
    }

    pub async fn update(&self, request: UpdateNameTableRequest) -> Result<(), Status> {
        let changed = request.clone();
        self.inner
            .store
            .change(move |store| store.update(&changed))
            .await?;
        info!(
            "Name tables: {} updated to version {}, {} names put, {} removed",
            request.id,
            request.version,
            request.put_entries.len(),
            request.remove_names.len()
        );
        Ok(())
    }

    pub async fn delete(&self, id: &str) -> Result<(), Status> {
        let in_use = self
            .inner
            .in_use
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .clone();
        let in_use = in_use(id);
        let deleted = id.to_string();
        self.inner
            .store
            .change(move |store| store.delete(&deleted, |_| in_use))
            .await?;
        info!("Name tables: {id} deleted");
        Ok(())
    }

    /// The tables as last kept.
    fn store(&self) -> Arc<NameStore> {
        self.inner.store.read()
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

    #[tokio::test]
    async fn lookups_take_any_case() {
        let dir = tempfile::tempdir().unwrap();
        let tables = NameTables::open(dir.path()).unwrap();
        tables
            .put(table("env-1", &[("web", "fd00::1")]))
            .await
            .unwrap();
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

    #[tokio::test]
    async fn a_table_a_pod_names_stays() {
        let dir = tempfile::tempdir().unwrap();
        let tables = NameTables::open(dir.path()).unwrap();
        tables.put(table("env-1", &[])).await.unwrap();
        tables.put(table("env-2", &[])).await.unwrap();
        let names = {
            let tables = tables.clone();
            // As the pods would: it may ask the tables.
            move |id: &str| tables.missing(&[id.to_string()]).is_empty() && id == "env-1"
        };
        tables.set_in_use(names);
        assert_eq!(
            tables.delete("env-1").await.unwrap_err().code(),
            Code::FailedPrecondition
        );
        tables.delete("env-2").await.unwrap();
        assert_eq!(tables.get("env-2").unwrap_err().code(), Code::NotFound);
        assert_eq!(tables.list().len(), 1);
    }

    /// A change writes its file on the blocking pool and is seen once it
    /// is kept: the resolver's lookups do not wait for its fsync, which
    /// can take seconds, and the runtime, of one thread, moves meanwhile.
    #[tokio::test]
    async fn lookups_answer_while_a_change_is_written() {
        let dir = tempfile::tempdir().unwrap();
        let tables = NameTables::open(dir.path()).unwrap();
        let ids = vec!["env-1".to_string()];
        tables
            .put(table("env-1", &[("web", "fd00::1")]))
            .await
            .unwrap();
        let hung = crate::kept::Hung::at(dir.path().join("env-1.pb.new"));
        let (moved, watchdog) = hung.watchdog();
        let put = tokio::spawn({
            let tables = tables.clone();
            async move { tables.put(table("env-1", &[("web", "fd00::2")])).await }
        });
        tokio::time::sleep(std::time::Duration::from_millis(100)).await;
        // As the resolver asks, on a thread of its own.
        let (answer, answered) = std::sync::mpsc::channel();
        std::thread::spawn({
            let (tables, ids) = (tables.clone(), ids.clone());
            move || answer.send(tables.lookup(&ids, "web"))
        });
        let looked_up = answered.recv_timeout(std::time::Duration::from_secs(2));
        let _ = moved.send(());
        assert!(!watchdog.join().unwrap(), "the write held the runtime");
        let old = Some(vec!["fd00::1".parse::<IpAddr>().unwrap()]);
        assert_eq!(
            looked_up,
            Ok(old.clone()),
            "the lookup waited for the write"
        );
        assert!(put.await.unwrap().is_err(), "a FIFO does not sync");
        // What was not kept is not seen.
        assert_eq!(tables.lookup(&ids, "web"), old);
    }
}
