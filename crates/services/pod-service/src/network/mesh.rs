// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! The cluster's mesh (ADR 0017), for a node whose network routes it no
//! prefix: a WireGuard device, `feos-mesh`, with a key of the node's own,
//! whose peers are the other nodes, each with its /64 of the cluster's ULA
//! as the one prefix allowed for it. The control plane gives the node its
//! /64 and sends it the peers; both are kept in the mesh's directory, the
//! key too, so that the mesh comes up at feosd's start without the control
//! plane.

use super::devices;
use feos_proto::network_service::{
    Mesh as MeshInfo, MeshPeer, PutMeshPeersRequest, UpdateMeshPeersRequest,
};
use feos_utils::wireguard::{self, Key, Peer};
use log::{info, warn};
use network_service::Prefix;
use prost::Message;
use rtnetlink::packet_route::route::RouteType;
use rtnetlink::{Handle, LinkWireguard, RouteMessageBuilder};
use std::collections::BTreeMap;
use std::fs;
use std::io::{self, Write};
use std::net::{IpAddr, Ipv6Addr, SocketAddr};
use std::os::unix::fs::{DirBuilderExt, OpenOptionsExt};
use std::path::{Path, PathBuf};
use std::sync::{Arc, Mutex, MutexGuard};
use tokio::sync::OwnedMutexGuard;
use tonic::Status;

/// The mesh's device.
pub const DEVICE: &str = "feos-mesh";
/// WireGuard's port, unless `--mesh-port` names another.
pub const PORT: u16 = 51820;
/// WireGuard's MTU, which leaves room for its headers over either family
/// on a network of 1500.
pub const MTU: u32 = 1420;
/// The node's private key, 32 bytes, readable by root alone.
const KEY: &str = "key";
/// What the control plane gave the node and sent it, as a `Mesh` in
/// protobuf: the cluster's ULA, the node's /64, the peers' version and
/// the peers.
const STATE: &str = "mesh.pb";

/// How the node comes onto a mesh: feosd's flags.
#[derive(Clone, Debug, PartialEq)]
pub struct Config {
    /// The key and what the control plane sends, `<node dir>/mesh`.
    pub dir: PathBuf,
    /// The UDP port of the device (`--mesh-port`).
    pub port: u16,
}

/// The cluster's ULA and the node's /64 of it.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct Assignment {
    pub pod_network: (Ipv6Addr, u8),
    pub pod_prefix: (Ipv6Addr, u8),
}

impl Assignment {
    /// What the control plane answered: a /48 of fd00::/8 and a /64 of it.
    pub fn parse(pod_network: &str, pod_prefix: &str) -> Result<Self, String> {
        let network = match pod_network.parse::<Prefix>()? {
            Prefix::V6(address, 48) if address.octets()[0] == 0xfd => (address, 48),
            _ => return Err(format!("{pod_network} is not a /48 of fd00::/8")),
        };
        let prefix = match pod_prefix.parse::<Prefix>()? {
            Prefix::V6(address, 64)
                if Prefix::V6(network.0, 48).contains(&Prefix::V6(address, 64)) =>
            {
                (address, 64)
            }
            _ => return Err(format!("{pod_prefix} is not a /64 of {pod_network}")),
        };
        Ok(Self {
            pod_network: network,
            pod_prefix: prefix,
        })
    }
}

/// The node's side of the mesh.
pub struct Mesh {
    config: Config,
    key: Key,
    public_key: Key,
    /// What the node keeps, as last kept: a change is written first, on
    /// the blocking pool, and seen then, so that a reader never waits for
    /// its fsync.
    state: Arc<Mutex<State>>,
    /// Held by a change, one at a time, until it is kept.
    changing: Arc<tokio::sync::Mutex<()>>,
}

#[derive(Clone, Default)]
struct State {
    assignment: Option<Assignment>,
    version: u64,
    /// By node.
    peers: BTreeMap<String, MeshPeer>,
}

impl Mesh {
    /// The node's key, made the first time, its device with the key and the
    /// port, and what the control plane gave and sent the node before: its
    /// /64's route and the peers.
    pub async fn open(config: Config, handle: &Handle) -> io::Result<Self> {
        fs::DirBuilder::new()
            .recursive(true)
            .mode(0o700)
            .create(&config.dir)?;
        let key = load_or_make_key(&config.dir.join(KEY))?;
        let state = load_state(&config.dir.join(STATE))?;
        device(handle).await?;
        wireguard::set_key(DEVICE, &key, config.port)
            .map_err(|e| io::Error::new(e.kind(), format!("{DEVICE}: key and port: {e}")))?;
        let public_key = wireguard::get(DEVICE)
            .map_err(|e| io::Error::new(e.kind(), format!("{DEVICE}: {e}")))?
            .public_key
            .ok_or_else(|| io::Error::other(format!("{DEVICE} has no public key")))?;
        let mesh = Self {
            config,
            key,
            public_key,
            state: Arc::new(Mutex::new(state)),
            changing: Arc::default(),
        };
        if let Some(assignment) = mesh.assignment() {
            route(handle, assignment.pod_network).await?;
        }
        mesh.sync_peers()?;
        Ok(mesh)
    }

    pub fn public_key(&self) -> Key {
        self.public_key
    }

    pub fn port(&self) -> u16 {
        self.config.port
    }

    pub fn assignment(&self) -> Option<Assignment> {
        self.state().assignment
    }

    /// Keeps what the control plane gave the node, and routes the
    /// cluster's ULA to the device. Returns whether it changed.
    pub async fn assign(&self, assignment: Assignment, handle: &Handle) -> io::Result<bool> {
        let changed = {
            let changing = self.changing.clone().lock_owned().await;
            let mut next = self.state().clone();
            if next.assignment == Some(assignment) {
                false
            } else {
                next.assignment = Some(assignment);
                self.keep(next, changing).await?;
                true
            }
        };
        route(handle, assignment.pod_network).await?;
        Ok(changed)
    }

    /// The mesh as `MeshService.GetMesh` shows it.
    pub fn info(&self) -> MeshInfo {
        let state = self.state();
        let prefix = |p: Option<(Ipv6Addr, u8)>| {
            p.map(|(address, len)| format!("{address}/{len}"))
                .unwrap_or_default()
        };
        MeshInfo {
            pod_network: prefix(state.assignment.map(|a| a.pod_network)),
            pod_prefix: prefix(state.assignment.map(|a| a.pod_prefix)),
            public_key: self.public_key.to_vec(),
            port: u32::from(self.config.port),
            version: state.version,
            peers: state.peers.values().cloned().collect(),
        }
    }

    /// The peers' endpoints, which no pod reaches.
    pub fn endpoints(&self) -> Vec<IpAddr> {
        self.state()
            .peers
            .values()
            .filter_map(|peer| peer.endpoint.parse::<SocketAddr>().ok())
            .map(|endpoint| endpoint.ip().to_canonical())
            .collect()
    }

    /// Replaces the peers, whatever their version.
    pub async fn put(&self, request: PutMeshPeersRequest) -> Result<(), Status> {
        let mut peers = BTreeMap::new();
        for peer in request.peers {
            check_peer(&peer)?;
            peers.insert(peer.node.clone(), peer);
        }
        let n = peers.len();
        self.change(|state| {
            state.version = request.version;
            state.peers = peers;
            Ok(())
        })
        .await?;
        info!("Mesh: {n} peers put at version {}", request.version);
        Ok(())
    }

    /// Changes the peers from one version to the next.
    pub async fn update(&self, request: UpdateMeshPeersRequest) -> Result<(), Status> {
        for peer in &request.put_peers {
            check_peer(peer)?;
        }
        let (put, removed) = (request.put_peers.len(), request.remove_peers.len());
        self.change(|state| {
            if state.version != request.from_version {
                return Err(Status::failed_precondition(format!(
                    "the node has the mesh's peers at version {}, not {}",
                    state.version, request.from_version
                )));
            }
            state.version = request.version;
            for node in &request.remove_peers {
                state.peers.remove(node);
            }
            for peer in request.put_peers {
                state.peers.insert(peer.node.clone(), peer);
            }
            Ok(())
        })
        .await?;
        info!(
            "Mesh: Peers updated to version {}, {put} put, {removed} removed",
            request.version
        );
        Ok(())
    }

    /// Changes the state, keeps it, and gives the device its peers, on the
    /// blocking pool; a change that is not kept is not seen.
    async fn change(
        &self,
        change: impl FnOnce(&mut State) -> Result<(), Status>,
    ) -> Result<(), Status> {
        let changing = self.changing.clone().lock_owned().await;
        let mut next = self.state().clone();
        if next.assignment.is_none() {
            return Err(Status::failed_precondition(
                "the node has no /64 of the mesh yet",
            ));
        }
        change(&mut next)?;
        self.keep(next, changing)
            .await
            .map_err(|e| Status::internal(format!("keeping the mesh's peers: {e}")))?;
        let peers = self.peers();
        tokio::task::spawn_blocking(move || wireguard::set_peers(DEVICE, &peers))
            .await
            .unwrap_or_else(|e| Err(io::Error::other(e)))
            .map_err(|e| Status::internal(format!("{DEVICE}: {e}")))
    }

    /// Writes `next` on the blocking pool, and makes it the state once it
    /// is kept, also when the caller went away meanwhile: the change ends
    /// with it.
    async fn keep(&self, next: State, changing: OwnedMutexGuard<()>) -> io::Result<()> {
        let (path, state) = (self.config.dir.join(STATE), self.state.clone());
        tokio::task::spawn_blocking(move || {
            let _changing = changing;
            store_state(&path, &next)?;
            *state.lock().unwrap_or_else(|e| e.into_inner()) = next;
            Ok(())
        })
        .await
        .unwrap_or_else(|e| Err(io::Error::other(e)))
    }

    /// Gives the device the peers kept.
    fn sync_peers(&self) -> io::Result<()> {
        wireguard::set_peers(DEVICE, &self.peers())
    }

    /// The peers kept, as the device takes them.
    fn peers(&self) -> Vec<Peer> {
        self.state()
            .peers
            .values()
            .filter_map(|peer| match wireguard_peer(peer) {
                Ok(peer) => Some(peer),
                Err(e) => {
                    warn!("Mesh: Peer {} is left out: {e}", peer.node);
                    None
                }
            })
            // A peer with the node's own key would take its traffic.
            .filter(|peer| peer.public_key != self.public_key)
            .collect()
    }

    fn state(&self) -> MutexGuard<'_, State> {
        self.state.lock().unwrap_or_else(|e| e.into_inner())
    }

    #[cfg(test)]
    pub(super) fn for_tests(dir: &Path, assignment: Option<Assignment>) -> Self {
        Self {
            config: Config {
                dir: dir.to_path_buf(),
                port: PORT,
            },
            key: [1; 32],
            public_key: [2; 32],
            state: Arc::new(Mutex::new(State {
                assignment,
                ..Default::default()
            })),
            changing: Arc::default(),
        }
    }
}

impl Drop for Mesh {
    fn drop(&mut self) {
        // The key stays on disk, not in feosd's memory longer than needed.
        self.key = [0; 32];
    }
}

/// Checks a peer as the control plane sends it.
fn check_peer(peer: &MeshPeer) -> Result<(), Status> {
    wireguard_peer(peer)
        .map(|_| ())
        .map_err(|e| Status::invalid_argument(format!("peer {:?}: {e}", peer.node)))
}

fn wireguard_peer(peer: &MeshPeer) -> Result<Peer, String> {
    if peer.node.is_empty() {
        return Err("no node".to_string());
    }
    let public_key: Key = peer
        .public_key
        .as_slice()
        .try_into()
        .map_err(|_| format!("a key of {} bytes", peer.public_key.len()))?;
    let endpoint = peer
        .endpoint
        .parse::<SocketAddr>()
        .map_err(|_| format!("endpoint {:?} is not host:port", peer.endpoint))?;
    let allowed = match peer.pod_prefix.parse::<Prefix>()? {
        Prefix::V6(address, 64) => (IpAddr::V6(address), 64),
        _ => return Err(format!("{} is not a /64", peer.pod_prefix)),
    };
    Ok(Peer {
        public_key,
        endpoint: Some(endpoint),
        allowed_ips: vec![allowed],
    })
}

/// Makes the device, with WireGuard's MTU, and brings it up.
async fn device(handle: &Handle) -> io::Result<()> {
    if devices::link(handle, DEVICE).await?.is_none() {
        handle
            .link()
            .add(LinkWireguard::new(DEVICE).build())
            .execute()
            .await
            .map_err(devices::netlink_error(&format!(
                "create {DEVICE} (CONFIG_WIREGUARD)"
            )))?;
        info!("Mesh: Created {DEVICE}");
    }
    let index = devices::index(handle, DEVICE).await?;
    devices::own_ipv6(DEVICE)?;
    devices::set_mtu(handle, index, DEVICE, MTU).await?;
    devices::up(handle, index, DEVICE).await
}

/// Routes the cluster's ULA to the device: the node's own /64 stays
/// unreachable but for its pods' addresses, which are longer.
async fn route(handle: &Handle, (network, len): (Ipv6Addr, u8)) -> io::Result<()> {
    let index = devices::index(handle, DEVICE).await?;
    let route = RouteMessageBuilder::<Ipv6Addr>::new()
        .destination_prefix(network, len)
        .output_interface(index)
        .kind(RouteType::Unicast)
        .build();
    handle
        .route()
        .add(route)
        .replace()
        .execute()
        .await
        .map_err(devices::netlink_error(&format!(
            "route {network}/{len} to {DEVICE}"
        )))
}

/// The key in `path`, or a new one, written there first.
fn load_or_make_key(path: &Path) -> io::Result<Key> {
    match fs::read(path) {
        Ok(bytes) => bytes.as_slice().try_into().map_err(|_| {
            io::Error::new(
                io::ErrorKind::InvalidData,
                format!("{} holds {} bytes, not a key", path.display(), bytes.len()),
            )
        }),
        Err(e) if e.kind() == io::ErrorKind::NotFound => {
            let key = wireguard::generate_key()?;
            write_file(path, &key)?;
            info!("Mesh: Made the node's key in {}", path.display());
            Ok(key)
        }
        Err(e) => Err(io::Error::new(e.kind(), format!("{}: {e}", path.display()))),
    }
}

fn load_state(path: &Path) -> io::Result<State> {
    let bytes = match fs::read(path) {
        Ok(bytes) => bytes,
        Err(e) if e.kind() == io::ErrorKind::NotFound => return Ok(State::default()),
        Err(e) => return Err(io::Error::new(e.kind(), format!("{}: {e}", path.display()))),
    };
    let kept = MeshInfo::decode(bytes.as_slice()).map_err(|e| {
        io::Error::new(
            io::ErrorKind::InvalidData,
            format!("{}: {e}", path.display()),
        )
    })?;
    let assignment = match kept.pod_network.is_empty() {
        true => None,
        false => Some(
            Assignment::parse(&kept.pod_network, &kept.pod_prefix)
                .map_err(|e| io::Error::new(io::ErrorKind::InvalidData, e))?,
        ),
    };
    Ok(State {
        assignment,
        version: kept.version,
        peers: kept
            .peers
            .into_iter()
            .map(|peer| (peer.node.clone(), peer))
            .collect(),
    })
}

fn store_state(path: &Path, state: &State) -> io::Result<()> {
    let prefix = |p: Option<(Ipv6Addr, u8)>| {
        p.map(|(address, len)| format!("{address}/{len}"))
            .unwrap_or_default()
    };
    let kept = MeshInfo {
        pod_network: prefix(state.assignment.map(|a| a.pod_network)),
        pod_prefix: prefix(state.assignment.map(|a| a.pod_prefix)),
        version: state.version,
        peers: state.peers.values().cloned().collect(),
        ..Default::default()
    };
    write_file(path, &kept.encode_to_vec())
}

/// Writes `bytes` to `path` through a file beside it, readable by root
/// alone, and a rename.
fn write_file(path: &Path, bytes: &[u8]) -> io::Result<()> {
    let tmp = path.with_extension("tmp");
    let mut file = fs::OpenOptions::new()
        .write(true)
        .create(true)
        .truncate(true)
        .mode(0o600)
        .open(&tmp)?;
    file.write_all(bytes)?;
    file.sync_all()?;
    fs::rename(&tmp, path)
}

#[cfg(test)]
mod tests {
    use super::*;

    fn peer(node: &str, n: u8, endpoint: &str, subnet: u16) -> MeshPeer {
        MeshPeer {
            node: node.to_string(),
            public_key: vec![n; 32],
            endpoint: endpoint.to_string(),
            pod_prefix: format!("fd5e:1b2c:93a0:{subnet:x}::/64"),
        }
    }

    fn assignment() -> Assignment {
        Assignment::parse("fd5e:1b2c:93a0::/48", "fd5e:1b2c:93a0:7::/64").unwrap()
    }

    #[test]
    fn assignments_are_a_48_and_a_64_of_it() {
        let a = assignment();
        assert_eq!(a.pod_network, ("fd5e:1b2c:93a0::".parse().unwrap(), 48));
        assert_eq!(a.pod_prefix, ("fd5e:1b2c:93a0:7::".parse().unwrap(), 64));
        for (network, prefix) in [
            ("fd5e:1b2c:93a0::/48", "fd5e:1b2c:93a1:7::/64"),
            ("fd5e:1b2c:93a0::/48", "fd5e:1b2c:93a0::/56"),
            ("fd5e:1b2c::/32", "fd5e:1b2c:93a0:7::/64"),
            ("fc5e:1b2c:93a0::/48", "fc5e:1b2c:93a0:7::/64"),
            ("2001:db8:1::/48", "2001:db8:1:7::/64"),
            ("", ""),
        ] {
            assert!(
                Assignment::parse(network, prefix).is_err(),
                "{network} {prefix}"
            );
        }
    }

    #[test]
    fn peers_are_checked() {
        assert!(check_peer(&peer("b", 2, "10.0.0.11:51820", 2)).is_ok());
        assert!(check_peer(&peer("b", 2, "[2001:db8::11]:51820", 2)).is_ok());
        let bad = [
            MeshPeer {
                public_key: vec![2; 31],
                ..peer("b", 2, "10.0.0.11:51820", 2)
            },
            peer("", 2, "10.0.0.11:51820", 2),
            peer("b", 2, "10.0.0.11", 2),
            peer("b", 2, "node-b:51820", 2),
            MeshPeer {
                pod_prefix: "fd5e:1b2c:93a0::/48".to_string(),
                ..peer("b", 2, "10.0.0.11:51820", 2)
            },
        ];
        for peer in bad {
            assert_eq!(
                check_peer(&peer).unwrap_err().code(),
                tonic::Code::InvalidArgument,
                "{peer:?}"
            );
        }
    }

    #[test]
    fn the_state_is_kept() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join(STATE);
        let state = State {
            assignment: Some(assignment()),
            version: 9,
            peers: [("b".to_string(), peer("b", 2, "10.0.0.11:51820", 2))].into(),
        };
        store_state(&path, &state).unwrap();
        let kept = load_state(&path).unwrap();
        assert_eq!(kept.assignment, state.assignment);
        assert_eq!(kept.version, 9);
        assert_eq!(kept.peers, state.peers);
        assert_eq!(
            std::os::unix::fs::PermissionsExt::mode(&fs::metadata(&path).unwrap().permissions())
                & 0o777,
            0o600
        );
        assert!(load_state(&dir.path().join("none"))
            .unwrap()
            .assignment
            .is_none());
    }

    #[test]
    fn a_key_is_made_once() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join(KEY);
        let key = load_or_make_key(&path).unwrap();
        assert_eq!(load_or_make_key(&path).unwrap(), key);
        fs::write(&path, [0; 31]).unwrap();
        assert!(load_or_make_key(&path).is_err());
    }

    /// Updates start at the version the node has; without a /64, there is
    /// nothing to put peers on. The device is not touched: sync_peers is
    /// left out of these.
    #[tokio::test]
    async fn updates_start_at_the_version_the_node_has() {
        let dir = tempfile::tempdir().unwrap();
        let mesh = Mesh::for_tests(dir.path(), None);
        let error = mesh.change(|_| Ok(())).await.unwrap_err();
        assert_eq!(error.code(), tonic::Code::FailedPrecondition);
        let mesh = Mesh::for_tests(dir.path(), Some(assignment()));
        {
            let mut state = mesh.state();
            state.version = 3;
        }
        let update = |from_version| UpdateMeshPeersRequest {
            from_version,
            version: from_version + 1,
            put_peers: vec![peer("c", 3, "10.0.0.12:51820", 3)],
            remove_peers: vec!["b".to_string()],
        };
        // A delta from another version is refused before anything changes.
        let error = mesh.update(update(2)).await.unwrap_err();
        assert_eq!(error.code(), tonic::Code::FailedPrecondition);
        assert_eq!(mesh.info().version, 3);
        assert_eq!(mesh.endpoints(), Vec::<IpAddr>::new());
    }

    /// A change is written on the blocking pool and seen once it is kept:
    /// what reads the mesh, as the rules that deny the peers' endpoints,
    /// does not wait for its fsync.
    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn the_mesh_answers_while_a_change_is_written() {
        let dir = tempfile::tempdir().unwrap();
        let mesh = Arc::new(Mesh::for_tests(dir.path(), Some(assignment())));
        let hung = crate::hung::Hung::at(dir.path().join(STATE).with_extension("tmp"));
        let put = tokio::spawn({
            let mesh = mesh.clone();
            async move {
                let peers = vec![peer("b", 2, "10.0.0.11:51820", 2)];
                mesh.put(PutMeshPeersRequest { version: 1, peers }).await
            }
        });
        tokio::time::sleep(std::time::Duration::from_millis(100)).await;
        let (answer, answered) = std::sync::mpsc::channel();
        std::thread::spawn({
            let mesh = mesh.clone();
            move || answer.send(mesh.endpoints())
        });
        let endpoints = answered.recv_timeout(std::time::Duration::from_secs(2));
        hung.open();
        assert_eq!(endpoints, Ok(Vec::new()), "the read waited for the write");
        assert!(put.await.unwrap().is_err(), "a FIFO does not sync");
        assert_eq!(mesh.info().version, 0, "what was not kept is not seen");
    }
}
