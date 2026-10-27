// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! The node's identity in its directory (ADR 0015): its key, its
//! certificate chain, the trust bundle, and what it knows of the control
//! plane.
//!
//! The key is kept before the first Join that uses it, so that a Join
//! repeated after a lost answer, with the same token and key, is answered
//! again. It stays across renewals: a renewal cut off between two files
//! leaves a chain that matches the key. `node.json` is written last and
//! says that the node has joined.

use crate::token;
use chrono::{DateTime, SecondsFormat, Utc};
use feos_proto::fleet_service::{Certificate, ControlPlaneInstance};
use rcgen::{CertificateParams, DistinguishedName, DnType, KeyPair, PublicKeyData};
use rustls::crypto::CryptoProvider;
use rustls::pki_types::{CertificateDer, PrivateKeyDer, PrivatePkcs8KeyDer, UnixTime};
use rustls::server::WebPkiClientVerifier;
use rustls::RootCertStore;
use serde::{Deserialize, Serialize};
use std::fs::{self, File, OpenOptions};
use std::io::{self, Write};
use std::os::unix::fs::{OpenOptionsExt, PermissionsExt};
use std::path::{Path, PathBuf};
use std::sync::Arc;

/// The node's directory, `feosd --node-dir`.
pub const NODE_DIR: &str = "/var/lib/feos/node";

/// PKCS#8, readable by root alone.
const KEY: &str = "key.pem";
/// The node's certificate first, then the intermediates.
const CHAIN: &str = "chain.pem";
/// The platform CA's roots, which the control plane is checked against.
const BUNDLE: &str = "bundle.pem";
const STATE: &str = "node.json";

/// What `node.json` holds.
#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
pub struct State {
    /// The name the control plane assigned.
    pub name: String,
    pub spiffe_id: String,
    /// The certificate's, in RFC 3339.
    pub not_after: String,
    pub renew_after: String,
    /// The control plane's instances, as it last listed them.
    #[serde(default)]
    pub instances: Vec<Instance>,
}

/// An instance of the control plane, as nodes reach it.
#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
pub struct Instance {
    pub id: String,
    pub node_address: String,
}

impl From<&ControlPlaneInstance> for Instance {
    fn from(instance: &ControlPlaneInstance) -> Self {
        Self {
            id: instance.id.clone(),
            node_address: instance.node_address.clone(),
        }
    }
}

/// The node's key and certificate, and what it checks the control plane
/// against.
pub struct Identity {
    pub key: KeyPair,
    /// DER, the node's certificate first.
    pub chain: Vec<CertificateDer<'static>>,
    /// DER, the platform CA's roots.
    pub bundle: Vec<CertificateDer<'static>>,
    pub state: State,
    pub not_after: DateTime<Utc>,
    pub renew_after: DateTime<Utc>,
}

impl Identity {
    /// The identity that a Join or a RenewCertificate issued for `key` and
    /// node `name`, once it is checked: the certificate names the node in
    /// the trust domain, is for the key, and verifies to the bundle.
    pub fn issued(
        key: KeyPair,
        certificate: Certificate,
        name: &str,
        trust_domain: &str,
        instances: Vec<Instance>,
        provider: &Arc<CryptoProvider>,
    ) -> Result<Self, String> {
        if !token::is_dns_label(name) {
            return Err(format!("the control plane named the node {name:?}"));
        }
        let expected = token::node_id(trust_domain, name);
        if certificate.spiffe_id != expected {
            return Err(format!(
                "the certificate is for {:?}, not {expected}",
                certificate.spiffe_id
            ));
        }
        let chain: Vec<CertificateDer<'static>> =
            certificate.chain.into_iter().map(Into::into).collect();
        let bundle: Vec<CertificateDer<'static>> = certificate
            .trust_bundle
            .into_iter()
            .map(Into::into)
            .collect();
        let Some(leaf) = chain.first() else {
            return Err("the certificate's chain is empty".into());
        };
        check_node_leaf(leaf, &expected, &key)?;
        let mut roots = RootCertStore::empty();
        for root in &bundle {
            roots
                .add(root.clone())
                .map_err(|e| format!("the trust bundle: {e}"))?;
        }
        if roots.is_empty() {
            return Err("the trust bundle is empty".into());
        }
        let verifier =
            WebPkiClientVerifier::builder_with_provider(Arc::new(roots), provider.clone())
                .build()
                .map_err(|e| format!("the trust bundle: {e}"))?;
        verifier
            .verify_client_cert(leaf, &chain[1..], UnixTime::now())
            .map_err(|e| format!("the certificate does not verify to the trust bundle: {e}"))?;
        let not_after = timestamp(certificate.not_after.as_ref())
            .ok_or("the certificate has no end of its lifetime")?;
        let renew_after = timestamp(certificate.renew_after.as_ref())
            .ok_or("the certificate has no time for its renewal")?;
        Ok(Self {
            key,
            chain,
            bundle,
            state: State {
                name: name.to_string(),
                spiffe_id: expected,
                not_after: rfc3339(not_after),
                renew_after: rfc3339(renew_after),
                instances,
            },
            not_after,
            renew_after,
        })
    }

    /// The key, as TLS takes it.
    pub fn private_key(&self) -> PrivateKeyDer<'static> {
        PrivateKeyDer::Pkcs8(PrivatePkcs8KeyDer::from(self.key.serialize_der()))
    }
}

/// Checks the node's certificate: its one URI name is the node's SPIFFE ID,
/// and it is for the node's key.
fn check_node_leaf(leaf: &[u8], spiffe_id: &str, key: &KeyPair) -> Result<(), String> {
    let (_, cert) = x509_parser::parse_x509_certificate(leaf)
        .map_err(|e| format!("the certificate does not parse: {e}"))?;
    let uris: Vec<&str> = match cert.subject_alternative_name() {
        Ok(Some(names)) => names
            .value
            .general_names
            .iter()
            .filter_map(|name| match name {
                x509_parser::extensions::GeneralName::URI(uri) => Some(*uri),
                _ => None,
            })
            .collect(),
        Ok(None) => Vec::new(),
        Err(e) => return Err(format!("the certificate's names: {e}")),
    };
    if uris != [spiffe_id] {
        return Err(format!("the certificate names {uris:?}, not {spiffe_id}"));
    }
    if cert.tbs_certificate.subject_pki.raw != key.subject_public_key_info() {
        return Err("the certificate is not for the node's key".into());
    }
    Ok(())
}

/// A PKCS#10 signing request in DER for `key`. The control plane ignores
/// its subject; the name is there for logs.
pub fn csr(key: &KeyPair, name: &str) -> Result<Vec<u8>, String> {
    let mut params = CertificateParams::new(Vec::<String>::new()).map_err(|e| e.to_string())?;
    params.distinguished_name = DistinguishedName::new();
    params.distinguished_name.push(DnType::CommonName, name);
    let request = params
        .serialize_request(key)
        .map_err(|e| format!("the signing request: {e}"))?;
    Ok(request.der().to_vec())
}

/// A protobuf timestamp.
pub(crate) fn timestamp(t: Option<&prost_types::Timestamp>) -> Option<DateTime<Utc>> {
    let t = t?;
    DateTime::from_timestamp(t.seconds, u32::try_from(t.nanos).ok()?)
}

fn rfc3339(t: DateTime<Utc>) -> String {
    t.to_rfc3339_opts(SecondsFormat::Secs, true)
}

/// The node's directory.
pub struct NodeDir {
    path: PathBuf,
}

impl NodeDir {
    /// The directory at `path`, made if it is missing, for root alone.
    pub fn open(path: &Path) -> io::Result<Self> {
        fs::create_dir_all(path)?;
        fs::set_permissions(path, fs::Permissions::from_mode(0o700))?;
        Ok(Self {
            path: path.to_path_buf(),
        })
    }

    pub fn path(&self) -> &Path {
        &self.path
    }

    /// The key to join with: the one kept from an earlier attempt, or a new
    /// one, kept before it is used.
    pub fn join_key(&self) -> io::Result<KeyPair> {
        match fs::read_to_string(self.path.join(KEY)) {
            Ok(pem) => return KeyPair::from_pem(&pem).map_err(|e| invalid(KEY, e)),
            Err(e) if e.kind() == io::ErrorKind::NotFound => {}
            Err(e) => return Err(e),
        }
        let key =
            KeyPair::generate_for(&rcgen::PKCS_ECDSA_P256_SHA256).map_err(io::Error::other)?;
        write_atomically(&self.path.join(KEY), key.serialize_pem().as_bytes(), 0o600)?;
        Ok(key)
    }

    /// The node's identity, if it has joined.
    pub fn load(&self) -> io::Result<Option<Identity>> {
        let state = match fs::read(self.path.join(STATE)) {
            Ok(state) => state,
            Err(e) if e.kind() == io::ErrorKind::NotFound => return Ok(None),
            Err(e) => return Err(e),
        };
        let state: State = serde_json::from_slice(&state).map_err(|e| invalid(STATE, e))?;
        let parse_time = |t: &str| {
            DateTime::parse_from_rfc3339(t)
                .map(|t| t.with_timezone(&Utc))
                .map_err(|e| invalid(STATE, e))
        };
        let not_after = parse_time(&state.not_after)?;
        let renew_after = parse_time(&state.renew_after)?;
        let key = fs::read_to_string(self.path.join(KEY))?;
        let key = KeyPair::from_pem(&key).map_err(|e| invalid(KEY, e))?;
        let chain = self.certificates(CHAIN)?;
        let bundle = self.certificates(BUNDLE)?;
        Ok(Some(Identity {
            key,
            chain,
            bundle,
            state,
            not_after,
            renew_after,
        }))
    }

    fn certificates(&self, file: &str) -> io::Result<Vec<CertificateDer<'static>>> {
        let text = fs::read(self.path.join(file))?;
        let certs: Vec<CertificateDer<'static>> = pem::parse_many(text)
            .map_err(|e| invalid(file, e))?
            .into_iter()
            .filter(|block| block.tag() == "CERTIFICATE")
            .map(|block| block.into_contents().into())
            .collect();
        if certs.is_empty() {
            return Err(invalid(file, "no certificate"));
        }
        Ok(certs)
    }

    /// Keeps an identity: its chain and bundle, then node.json.
    pub fn store(&self, identity: &Identity) -> io::Result<()> {
        write_atomically(
            &self.path.join(CHAIN),
            pem(&identity.chain).as_bytes(),
            0o644,
        )?;
        write_atomically(
            &self.path.join(BUNDLE),
            pem(&identity.bundle).as_bytes(),
            0o644,
        )?;
        self.store_state(&identity.state)
    }

    pub fn store_state(&self, state: &State) -> io::Result<()> {
        let json = serde_json::to_vec_pretty(state).map_err(io::Error::other)?;
        write_atomically(&self.path.join(STATE), &json, 0o644)
    }

    /// Moves the identity's files into a directory of their own, `removed-
    /// <time>`, for a node that has to join again: nothing is deleted.
    pub fn set_aside(&self) -> io::Result<PathBuf> {
        let stamp = Utc::now().format("%Y%m%dT%H%M%SZ");
        let mut aside = self.path.join(format!("removed-{stamp}"));
        let mut n = 1;
        loop {
            match fs::create_dir(&aside) {
                Ok(()) => break,
                Err(e) if e.kind() == io::ErrorKind::AlreadyExists => {
                    aside = self.path.join(format!("removed-{stamp}-{n}"));
                    n += 1;
                }
                Err(e) => return Err(e),
            }
        }
        fs::set_permissions(&aside, fs::Permissions::from_mode(0o700))?;
        // node.json first: without it, the node has not joined.
        for file in [STATE, CHAIN, BUNDLE, KEY] {
            match fs::rename(self.path.join(file), aside.join(file)) {
                Ok(()) => {}
                Err(e) if e.kind() == io::ErrorKind::NotFound => {}
                Err(e) => return Err(e),
            }
        }
        Ok(aside)
    }
}

fn invalid(file: &str, e: impl std::fmt::Display) -> io::Error {
    io::Error::new(io::ErrorKind::InvalidData, format!("{file}: {e}"))
}

fn pem(certs: &[CertificateDer<'_>]) -> String {
    let blocks: Vec<pem::Pem> = certs
        .iter()
        .map(|cert| pem::Pem::new("CERTIFICATE", cert.to_vec()))
        .collect();
    pem::encode_many(&blocks)
}

/// Writes a file whole or not at all: into a temporary file, synced, then
/// renamed over the old one.
fn write_atomically(path: &Path, contents: &[u8], mode: u32) -> io::Result<()> {
    let mut tmp = path.as_os_str().to_owned();
    tmp.push(".tmp");
    let tmp = PathBuf::from(tmp);
    // A file left by a crash may have another mode, which open keeps.
    fs::remove_file(&tmp).ok();
    let mut file = OpenOptions::new()
        .write(true)
        .create_new(true)
        .mode(mode)
        .open(&tmp)?;
    file.write_all(contents)?;
    file.sync_all()?;
    fs::rename(&tmp, path)?;
    if let Some(dir) = path.parent() {
        File::open(dir)?.sync_all()?;
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_join_key_is_kept_before_it_is_used() {
        let dir = tempfile::tempdir().unwrap();
        let node = NodeDir::open(&dir.path().join("node")).unwrap();
        let mode = |path: &Path| fs::metadata(path).unwrap().permissions().mode() & 0o777;
        assert_eq!(mode(node.path()), 0o700);
        let key = node.join_key().unwrap();
        assert_eq!(mode(&node.path().join(KEY)), 0o600);
        // A second attempt joins with the same key.
        let again = node.join_key().unwrap();
        assert_eq!(key.serialize_der(), again.serialize_der());
        assert!(node.load().unwrap().is_none());
        assert!(!node.path().join("key.pem.tmp").exists());
    }

    #[test]
    fn an_identity_is_stored_and_set_aside() {
        let dir = tempfile::tempdir().unwrap();
        let node = NodeDir::open(dir.path()).unwrap();
        let key = node.join_key().unwrap();
        let cert = rcgen::CertificateParams::new(vec!["n1".to_string()])
            .unwrap()
            .self_signed(&key)
            .unwrap();
        let now = Utc::now();
        let identity = Identity {
            key,
            chain: vec![cert.der().clone()],
            bundle: vec![cert.der().clone()],
            state: State {
                name: "n1".into(),
                spiffe_id: "spiffe://feos.local/node/n1".into(),
                not_after: rfc3339(now),
                renew_after: rfc3339(now),
                instances: vec![Instance {
                    id: "i1".into(),
                    node_address: "10.0.0.1:7443".into(),
                }],
            },
            not_after: now,
            renew_after: now,
        };
        node.store(&identity).unwrap();
        let loaded = node.load().unwrap().unwrap();
        assert_eq!(loaded.state, identity.state);
        assert_eq!(loaded.chain, identity.chain);
        assert_eq!(loaded.bundle, identity.bundle);
        assert_eq!(loaded.key.serialize_der(), identity.key.serialize_der());
        assert_eq!(loaded.renew_after.timestamp(), now.timestamp());

        let aside = node.set_aside().unwrap();
        assert!(node.load().unwrap().is_none());
        for file in [KEY, CHAIN, BUNDLE, STATE] {
            assert!(aside.join(file).exists(), "{file}");
            assert!(!node.path().join(file).exists(), "{file}");
        }
        // The next set-aside does not overwrite this one.
        let next = node.set_aside().unwrap();
        assert_ne!(next, aside);
    }
}
