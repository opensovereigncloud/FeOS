// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! The security groups the node has, kept across restarts of feosd: a file
//! per group, `<id>.pb`, the group as protobuf, replaced by a rename.

use crate::group;
use feos_proto::network_service::{SecurityGroup, UpdateSecurityGroupRequest};
use log::warn;
use prost::Message;
use std::collections::BTreeMap;
use std::fs;
use std::io::{self, Write};
use std::path::{Path, PathBuf};
use tonic::Status;

pub struct Store {
    dir: PathBuf,
    groups: BTreeMap<String, SecurityGroup>,
}

impl Store {
    /// The groups kept in `dir`. A file that does not read, or holds no
    /// valid group, is left alone, and logged.
    pub fn open(dir: impl Into<PathBuf>) -> io::Result<Self> {
        let dir = dir.into();
        fs::create_dir_all(&dir)?;
        let mut groups = BTreeMap::new();
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
                .and_then(|group| group::normalize(group).map_err(|e| e.message().to_string()))
            {
                Ok(group) if group.id == id => {
                    groups.insert(group.id.clone(), group);
                }
                Ok(group) => warn!(
                    "Security groups: {} holds group {:?}",
                    path.display(),
                    group.id
                ),
                Err(e) => warn!("Security groups: {}: {e}", path.display()),
            }
        }
        Ok(Self { dir, groups })
    }

    pub fn get(&self, id: &str) -> Option<&SecurityGroup> {
        self.groups.get(id)
    }

    /// The groups, by ID.
    pub fn groups(&self) -> &BTreeMap<String, SecurityGroup> {
        &self.groups
    }

    /// Puts a group whole, whatever the version of the one there; whether
    /// that changed it.
    pub fn put(&mut self, group: SecurityGroup) -> Result<bool, Status> {
        let group = group::normalize(group)?;
        if self.groups.get(&group.id) == Some(&group) {
            return Ok(false);
        }
        self.save(&group)?;
        self.groups.insert(group.id.clone(), group);
        Ok(true)
    }

    /// Adds and removes members, if the group is at `from_version`.
    /// Adding a member it has, or removing one it does not have, changes
    /// nothing.
    pub fn update(&mut self, request: &UpdateSecurityGroupRequest) -> Result<(), Status> {
        let id = &request.id;
        group::check_id(id)?;
        if request.version <= request.from_version {
            return Err(Status::invalid_argument(format!(
                "version {} does not come after {}",
                request.version, request.from_version
            )));
        }
        let add = group::members(&request.add_members)?;
        let remove = group::members(&request.remove_members)?;
        if let Some(both) = add.intersection(&remove).next() {
            return Err(Status::invalid_argument(format!(
                "member {both} is both added and removed"
            )));
        }
        let Some(group) = self.groups.get(id) else {
            return Err(Status::failed_precondition(format!(
                "security group {id} is not on the node"
            )));
        };
        if group.version != request.from_version {
            return Err(Status::failed_precondition(format!(
                "security group {id} is at version {}, not {}",
                group.version, request.from_version
            )));
        }
        let mut members = group::members(&group.members)?;
        members.retain(|member| !remove.contains(member));
        members.extend(add);
        let updated = SecurityGroup {
            version: request.version,
            members: members.iter().map(|member| member.to_string()).collect(),
            ..group.clone()
        };
        self.save(&updated)?;
        self.groups.insert(id.clone(), updated);
        Ok(())
    }

    /// Removes a group, unless `in_use` says a pod on the node names it.
    pub fn delete(&mut self, id: &str, in_use: impl FnOnce(&str) -> bool) -> Result<(), Status> {
        if !self.groups.contains_key(id) {
            return Err(Status::not_found(format!(
                "security group {id} is not on the node"
            )));
        }
        if in_use(id) {
            return Err(Status::failed_precondition(format!(
                "a pod on the node names security group {id}"
            )));
        }
        match fs::remove_file(self.path(id)) {
            Err(e) if e.kind() != io::ErrorKind::NotFound => {
                return Err(Status::internal(format!(
                    "cannot remove security group {id}: {e}"
                )));
            }
            _ => {}
        }
        self.groups.remove(id);
        Ok(())
    }

    fn save(&self, group: &SecurityGroup) -> Result<(), Status> {
        write(&self.dir, &self.path(&group.id), &group.encode_to_vec())
            .map_err(|e| Status::internal(format!("cannot keep security group {}: {e}", group.id)))
    }

    fn path(&self, id: &str) -> PathBuf {
        self.dir.join(format!("{id}.pb"))
    }
}

/// Writes a file whole or not at all, and makes it last.
pub(crate) fn write(dir: &Path, path: &Path, bytes: &[u8]) -> io::Result<()> {
    let temporary = path.with_extension("pb.new");
    let mut file = fs::File::create(&temporary)?;
    file.write_all(bytes)?;
    file.sync_all()?;
    fs::rename(&temporary, path)?;
    fs::File::open(dir)?.sync_all()
}

fn load(path: &Path) -> Result<SecurityGroup, String> {
    let bytes = fs::read(path).map_err(|e| format!("does not read: {e}"))?;
    SecurityGroup::decode(bytes.as_slice()).map_err(|e| format!("does not decode: {e}"))
}

#[cfg(test)]
mod tests {
    use super::*;
    use feos_proto::network_service::{rule, AddressBlock, Direction, Protocol, Rule};
    use tonic::Code;

    fn group(id: &str, version: u64, members: &[&str]) -> SecurityGroup {
        SecurityGroup {
            id: id.to_string(),
            version,
            rules: vec![Rule {
                direction: Direction::Ingress as i32,
                protocol: Protocol::Tcp as i32,
                ports: vec![],
                peer: Some(rule::Peer::Block(AddressBlock {
                    cidr: "::/0".to_string(),
                    except: vec![],
                })),
            }],
            members: members.iter().map(|m| m.to_string()).collect(),
        }
    }

    fn update(
        id: &str,
        from_version: u64,
        version: u64,
        add: &[&str],
        remove: &[&str],
    ) -> UpdateSecurityGroupRequest {
        UpdateSecurityGroupRequest {
            id: id.to_string(),
            from_version,
            version,
            add_members: add.iter().map(|m| m.to_string()).collect(),
            remove_members: remove.iter().map(|m| m.to_string()).collect(),
        }
    }

    #[test]
    fn groups_are_kept_across_restarts() {
        let dir = tempfile::tempdir().unwrap();
        let mut store = Store::open(dir.path()).unwrap();
        assert!(store.put(group("web", 1, &["fd00::2", "fd00::1"])).unwrap());
        assert!(store.put(group("db.eu", 3, &[])).unwrap());
        assert!(
            !store.put(group("web", 1, &["fd00::1", "fd00::2"])).unwrap(),
            "same group"
        );
        store
            .update(&update("web", 1, 2, &["fd00::3"], &["fd00::1"]))
            .unwrap();
        fs::write(dir.path().join("junk.pb"), b"\xff\xff").unwrap();
        fs::write(dir.path().join("README"), b"not a group").unwrap();
        fs::write(
            dir.path().join("other.pb"),
            group("web", 9, &[]).encode_to_vec(),
        )
        .unwrap();
        let mut invalid = group("bad", 1, &[]);
        invalid.members.push("10.0.0.1".to_string());
        fs::write(dir.path().join("bad.pb"), invalid.encode_to_vec()).unwrap();

        let store = Store::open(dir.path()).unwrap();
        assert_eq!(store.groups().keys().collect::<Vec<_>>(), ["db.eu", "web"]);
        let web = store.get("web").unwrap();
        assert_eq!(web.version, 2);
        assert_eq!(web.members, ["fd00::2", "fd00::3"]);
        assert_eq!(web.rules, group("web", 1, &[]).rules);
    }

    #[test]
    fn a_put_replaces_whatever_version() {
        let dir = tempfile::tempdir().unwrap();
        let mut store = Store::open(dir.path()).unwrap();
        store.put(group("web", 5, &["fd00::1"])).unwrap();
        store.put(group("web", 2, &["fd00::2"])).unwrap();
        assert_eq!(store.get("web").unwrap().version, 2);
        assert_eq!(store.get("web").unwrap().members, ["fd00::2"]);
        let error = store.put(group("a/b", 1, &[])).unwrap_err();
        assert_eq!(error.code(), Code::InvalidArgument);
        assert!(!dir.path().join("a").exists());
    }

    #[test]
    fn an_update_starts_at_the_version_there() {
        let dir = tempfile::tempdir().unwrap();
        let mut store = Store::open(dir.path()).unwrap();
        let error = store
            .update(&update("web", 1, 2, &["fd00::1"], &[]))
            .unwrap_err();
        assert_eq!(error.code(), Code::FailedPrecondition, "no group");

        store.put(group("web", 1, &["fd00::1"])).unwrap();
        let error = store
            .update(&update("web", 2, 3, &["fd00::2"], &[]))
            .unwrap_err();
        assert_eq!(error.code(), Code::FailedPrecondition, "wrong version");
        assert_eq!(store.get("web").unwrap().members, ["fd00::1"]);

        store
            .update(&update("web", 1, 2, &["fd00::2"], &[]))
            .unwrap();
        let error = store
            .update(&update("web", 1, 2, &["fd00::2"], &[]))
            .unwrap_err();
        assert_eq!(error.code(), Code::FailedPrecondition, "applied already");
        store
            .update(&update("web", 2, 4, &["fd00::2"], &["fd00::1", "fd00::9"]))
            .unwrap();
        let web = store.get("web").unwrap();
        assert_eq!(
            (web.version, web.members.as_slice()),
            (4, ["fd00::2".to_string()].as_slice())
        );

        for (request, why) in [
            (update("web", 4, 4, &[], &[]), "version does not count up"),
            (update("web", 4, 5, &["10.0.0.1"], &[]), "IPv4 member"),
            (update("web", 4, 5, &[], &["nonsense"]), "bad member"),
            (
                update("web", 4, 5, &["fd00::7"], &["fd00::7"]),
                "added and removed",
            ),
            (update("", 4, 5, &[], &[]), "no ID"),
        ] {
            let error = store.update(&request).unwrap_err();
            assert_eq!(error.code(), Code::InvalidArgument, "{why}");
        }
        assert_eq!(store.get("web").unwrap().version, 4);
    }

    #[test]
    fn a_group_in_use_stays() {
        let dir = tempfile::tempdir().unwrap();
        let mut store = Store::open(dir.path()).unwrap();
        store.put(group("web", 1, &[])).unwrap();
        let error = store.delete("web", |id| id == "web").unwrap_err();
        assert_eq!(error.code(), Code::FailedPrecondition);
        assert!(store.get("web").is_some());
        assert!(dir.path().join("web.pb").exists());

        store.delete("web", |_| false).unwrap();
        assert!(store.get("web").is_none());
        assert!(!dir.path().join("web.pb").exists());
        let error = store.delete("web", |_| false).unwrap_err();
        assert_eq!(error.code(), Code::NotFound);
        assert!(Store::open(dir.path()).unwrap().groups().is_empty());
    }
}
