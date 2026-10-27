// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! The node's security groups, shared: the store behind
//! `feos.network.v1.SecurityGroupService`, a counter of their changes for
//! what applies them, and the question whether a pod names a group.

use crate::store::Store;
use feos_proto::network_service::{SecurityGroup, UpdateSecurityGroupRequest};
use log::info;
use std::collections::BTreeMap;
use std::io;
use std::path::PathBuf;
use std::sync::{Arc, Mutex, MutexGuard};
use tokio::sync::watch;
use tonic::Status;

type InUse = Arc<dyn Fn(&str) -> bool + Send + Sync>;

#[derive(Clone)]
pub struct SecurityGroups {
    inner: Arc<Inner>,
}

struct Inner {
    store: Mutex<Store>,
    changes: watch::Sender<u64>,
    in_use: Mutex<InUse>,
}

impl SecurityGroups {
    /// The groups kept in `dir`.
    pub fn open(dir: impl Into<PathBuf>) -> io::Result<Self> {
        let store = Store::open(dir)?;
        info!("Security groups: {} kept", store.groups().len());
        Ok(Self {
            inner: Arc::new(Inner {
                store: Mutex::new(store),
                changes: watch::Sender::new(0),
                in_use: Mutex::new(Arc::new(|_| false)),
            }),
        })
    }

    /// Tells the groups whether a pod on the node names a group, which a
    /// delete then refuses. It is asked without a lock of the groups held,
    /// so it may call them. Until it is set, no group is in use.
    pub fn set_in_use(&self, in_use: impl Fn(&str) -> bool + Send + Sync + 'static) {
        *self.inner.in_use.lock().unwrap_or_else(|e| e.into_inner()) = Arc::new(in_use);
    }

    /// A counter that moves with every change of the groups, for what
    /// compiles and applies them again.
    pub fn subscribe(&self) -> watch::Receiver<u64> {
        self.inner.changes.subscribe()
    }

    /// The groups, by ID, for the compiler.
    pub fn snapshot(&self) -> BTreeMap<String, SecurityGroup> {
        self.store().groups().clone()
    }

    /// Of `ids`, those the node does not have, in their order: a pod that
    /// names one of them must not start (ADR 0012).
    pub fn missing(&self, ids: &[String]) -> Vec<String> {
        let store = self.store();
        ids.iter()
            .filter(|id| store.get(id).is_none())
            .cloned()
            .collect()
    }

    pub fn get(&self, id: &str) -> Result<SecurityGroup, Status> {
        self.store()
            .get(id)
            .cloned()
            .ok_or_else(|| Status::not_found(format!("security group {id} is not on the node")))
    }

    /// The groups, by ID.
    pub fn list(&self) -> Vec<SecurityGroup> {
        self.store().groups().values().cloned().collect()
    }

    pub fn put(&self, group: SecurityGroup) -> Result<(), Status> {
        let (id, version) = (group.id.clone(), group.version);
        if self.store().put(group)? {
            info!("Security groups: {id} put at version {version}");
            self.changed();
        }
        Ok(())
    }

    pub fn update(&self, request: &UpdateSecurityGroupRequest) -> Result<(), Status> {
        self.store().update(request)?;
        info!(
            "Security groups: {} updated to version {}, {} members added, {} removed",
            request.id,
            request.version,
            request.add_members.len(),
            request.remove_members.len()
        );
        self.changed();
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
        info!("Security groups: {id} deleted");
        self.changed();
        Ok(())
    }

    fn changed(&self) {
        self.inner.changes.send_modify(|n| *n = n.wrapping_add(1));
    }

    fn store(&self) -> MutexGuard<'_, Store> {
        self.inner.store.lock().unwrap_or_else(|e| e.into_inner())
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::atomic::{AtomicBool, Ordering};
    use tonic::Code;

    fn group(id: &str, version: u64, members: &[&str]) -> SecurityGroup {
        SecurityGroup {
            id: id.to_string(),
            version,
            rules: vec![],
            members: members.iter().map(|m| m.to_string()).collect(),
        }
    }

    #[test]
    fn every_change_moves_the_counter() {
        let dir = tempfile::tempdir().unwrap();
        let groups = SecurityGroups::open(dir.path()).unwrap();
        let mut changes = groups.subscribe();
        assert!(!changes.has_changed().unwrap());

        groups.put(group("web", 1, &["fd00::1"])).unwrap();
        assert!(changes.has_changed().unwrap());
        changes.mark_unchanged();
        groups.put(group("web", 1, &["fd00::1"])).unwrap();
        assert!(!changes.has_changed().unwrap(), "the same group again");

        assert!(groups.put(group("", 1, &[])).is_err());
        let wrong = UpdateSecurityGroupRequest {
            id: "web".to_string(),
            from_version: 7,
            version: 8,
            ..Default::default()
        };
        assert_eq!(
            groups.update(&wrong).unwrap_err().code(),
            Code::FailedPrecondition
        );
        assert!(
            !changes.has_changed().unwrap(),
            "failed calls change nothing"
        );

        let update = UpdateSecurityGroupRequest {
            id: "web".to_string(),
            from_version: 1,
            version: 2,
            add_members: vec!["fd00::2".to_string()],
            ..Default::default()
        };
        groups.update(&update).unwrap();
        assert!(changes.has_changed().unwrap());
        changes.mark_unchanged();
        assert_eq!(groups.get("web").unwrap().members, ["fd00::1", "fd00::2"]);

        groups.delete("web").unwrap();
        assert!(changes.has_changed().unwrap());
        assert_eq!(groups.get("web").unwrap_err().code(), Code::NotFound);
    }

    #[test]
    fn a_group_a_pod_names_stays() {
        let dir = tempfile::tempdir().unwrap();
        let groups = SecurityGroups::open(dir.path()).unwrap();
        groups.put(group("web", 1, &[])).unwrap();
        groups.put(group("db", 1, &[])).unwrap();
        let pod_exists = Arc::new(AtomicBool::new(true));
        let names = {
            let groups = groups.clone();
            let pod_exists = pod_exists.clone();
            // As the pods would: it may ask the groups.
            move |id: &str| {
                pod_exists.load(Ordering::SeqCst)
                    && groups.missing(&[id.to_string()]).is_empty()
                    && id == "web"
            }
        };
        groups.set_in_use(names);
        assert_eq!(
            groups.delete("web").unwrap_err().code(),
            Code::FailedPrecondition
        );
        groups.delete("db").unwrap();
        pod_exists.store(false, Ordering::SeqCst);
        groups.delete("web").unwrap();
        assert!(groups.list().is_empty());
    }

    #[test]
    fn missing_groups_are_named() {
        let dir = tempfile::tempdir().unwrap();
        let groups = SecurityGroups::open(dir.path()).unwrap();
        groups.put(group("web", 1, &[])).unwrap();
        let ids = ["db", "web", "cache"].map(String::from);
        assert_eq!(groups.missing(&ids), ["db", "cache"]);
        assert_eq!(groups.snapshot().keys().collect::<Vec<_>>(), ["web"]);
        assert_eq!(SecurityGroups::open(dir.path()).unwrap().list().len(), 1);
    }
}
