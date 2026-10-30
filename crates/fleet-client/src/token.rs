// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! Join tokens and their pins, and the names and SPIFFE IDs of the control
//! plane's PKI, as fleet.proto and the control plane define them.

use sha2::{Digest, Sha256};
use std::fmt;
use std::str::FromStr;

/// What every join token starts with, and the part a node sends.
const PREFIX: &str = "feos-join-";
/// The secret: 32 random bytes in base64url without padding.
const SECRET_LEN: usize = 43;

/// An operator's join token, `feos-join-<secret>.<pin>`.
#[derive(Clone, PartialEq, Eq)]
pub struct JoinToken {
    /// `feos-join-<secret>`, what the node sends in JoinRequest.token.
    pub secret: String,
    /// The lowercase hex SHA-256 of the DER SubjectPublicKeyInfo of the
    /// platform CA's root that the control plane's chain must hold.
    pub pin: String,
}

/// The secret stays out of logs.
impl fmt::Debug for JoinToken {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(f, "JoinToken {{ pin: {} }}", self.pin)
    }
}

impl FromStr for JoinToken {
    type Err = String;

    fn from_str(token: &str) -> Result<Self, String> {
        let malformed = || "the join token is not feos-join-<secret>.<pin>".to_string();
        let (secret, pin) = token.trim().split_once('.').ok_or_else(malformed)?;
        let encoded = secret.strip_prefix(PREFIX).ok_or_else(malformed)?;
        if encoded.len() != SECRET_LEN {
            return Err(malformed());
        }
        let base64url = |c: u8| c.is_ascii_alphanumeric() || c == b'-' || c == b'_';
        if !encoded.bytes().all(base64url) {
            return Err("the join token's secret is not base64url".into());
        }
        if pin.len() != 64 || !pin.bytes().all(|c| c.is_ascii_hexdigit()) {
            return Err("the join token's pin is not a hex SHA-256".into());
        }
        Ok(Self {
            secret: secret.to_string(),
            pin: pin.to_ascii_lowercase(),
        })
    }
}

/// The lowercase hex SHA-256 of `bytes`.
pub fn sha256_hex(bytes: &[u8]) -> String {
    Sha256::digest(bytes)
        .iter()
        .map(|b| format!("{b:02x}"))
        .collect()
}

/// A certificate's pin: the SHA-256 of its whole DER SubjectPublicKeyInfo,
/// the SEQUENCE with its tag and length, as Go's
/// `x509.Certificate.RawSubjectPublicKeyInfo` has it.
pub fn pin(cert_der: &[u8]) -> Result<String, String> {
    let (_, cert) = x509_parser::parse_x509_certificate(cert_der)
        .map_err(|e| format!("a certificate does not parse: {e}"))?;
    Ok(sha256_hex(cert.tbs_certificate.subject_pki.raw))
}

/// Whether `name` is a DNS label, `[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?`:
/// what a node's name is (JoinRequest.name).
pub fn is_dns_label(name: &str) -> bool {
    let bytes = name.as_bytes();
    (1..=63).contains(&bytes.len())
        && bytes.iter().enumerate().all(|(i, &c)| {
            c.is_ascii_lowercase()
                || c.is_ascii_digit()
                || (c == b'-' && i > 0 && i < bytes.len() - 1)
        })
}

/// Checks a SPIFFE trust domain: a lowercase DNS name of labels joined by
/// dots, at most 255 bytes, without a scheme or a port, and not an IP
/// address.
pub fn check_trust_domain(domain: &str) -> Result<(), String> {
    if domain.is_empty() {
        return Err("the trust domain is empty".into());
    }
    if domain.len() > 255 {
        return Err(format!(
            "the trust domain {domain:?} is longer than 255 bytes"
        ));
    }
    if domain.contains(':') {
        return Err(format!(
            "the trust domain {domain:?} has a scheme or a port; give the name alone"
        ));
    }
    if domain.parse::<std::net::IpAddr>().is_ok() {
        return Err(format!("the trust domain {domain:?} is an IP address"));
    }
    if let Some(label) = domain.split('.').find(|label| !is_dns_label(label)) {
        return Err(format!(
            "the trust domain {domain:?} is not a lowercase DNS name (label {label:?})"
        ));
    }
    Ok(())
}

/// The SPIFFE ID of node `name`.
pub fn node_id(trust_domain: &str, name: &str) -> String {
    format!("spiffe://{trust_domain}/node/{name}")
}

/// The prefix of the SPIFFE IDs of the sink's instances (ADR 0026),
/// `spiffe://<trust domain>/sink/<name>`.
pub fn sink_id_prefix(trust_domain: &str) -> String {
    format!("spiffe://{trust_domain}/sink/")
}

/// The SPIFFE ID that the control plane's certificates on its port for
/// nodes carry, and no other certificate of the platform CA.
pub fn control_plane_id(trust_domain: &str) -> String {
    format!("spiffe://{trust_domain}/ferun")
}

#[cfg(test)]
mod tests {
    use super::*;

    /// 43 characters of base64url.
    fn secret() -> String {
        format!("feos-join-{}_", "A".repeat(42))
    }

    const PIN: &str = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef";

    #[test]
    fn a_join_token_splits_into_its_secret_and_its_pin() {
        let secret = secret();
        let token: JoinToken = format!("{secret}.{PIN}\n").parse().unwrap();
        assert_eq!(token.secret, secret);
        assert_eq!(token.pin, PIN);
        // The pin's case does not matter; it is kept in lowercase.
        let token: JoinToken = format!("{secret}.{}", PIN.to_uppercase()).parse().unwrap();
        assert_eq!(token.pin, PIN);
        // The secret stays out of logs.
        assert!(!format!("{token:?}").contains(&secret));
    }

    #[test]
    fn bad_join_tokens_are_refused() {
        let secret = secret();
        for bad in [
            String::new(),
            secret.clone(),
            format!("{secret}{PIN}"),
            format!("{}.{PIN}", &secret[..secret.len() - 1]),
            format!("{secret}A.{PIN}"),
            format!("{}.{PIN}", secret.replace("feos-join-", "feos-jojn-")),
            format!("{}.{PIN}", secret.replace('_', "+")),
            format!("{secret}.{}", &PIN[1..]),
            format!("{secret}.{}", PIN.replace('a', "g")),
            format!("{secret}.{PIN}.{PIN}"),
        ] {
            assert!(bad.parse::<JoinToken>().is_err(), "{bad}");
        }
    }

    #[test]
    fn names_are_dns_labels() {
        for good in ["a", "node-1", "0", &"x".repeat(63)] {
            assert!(is_dns_label(good), "{good}");
        }
        for bad in ["", "-a", "a-", "Node", "a.b", "a_b", &"x".repeat(64)] {
            assert!(!is_dns_label(bad), "{bad}");
        }
    }

    #[test]
    fn trust_domains_are_lowercase_dns_names() {
        for good in ["feos.local", "run.fecode.dev", "localhost"] {
            assert_eq!(check_trust_domain(good), Ok(()), "{good}");
        }
        for bad in [
            "",
            "Feos.local",
            "spiffe://feos.local",
            "feos.local:443",
            "feos..local",
            "10.0.0.1",
            "feos.local.",
        ] {
            assert!(check_trust_domain(bad).is_err(), "{bad}");
        }
        assert_eq!(node_id("feos.local", "n1"), "spiffe://feos.local/node/n1");
        assert_eq!(control_plane_id("feos.local"), "spiffe://feos.local/ferun");
    }

    #[test]
    fn the_pin_is_over_the_whole_subject_public_key_info() {
        let key = rcgen::KeyPair::generate_for(&rcgen::PKCS_ECDSA_P256_SHA256).unwrap();
        let cert = rcgen::CertificateParams::new(Vec::<String>::new())
            .unwrap()
            .self_signed(&key)
            .unwrap();
        // rcgen's SubjectPublicKeyInfo is the SEQUENCE, tag 0x30 first.
        let spki = rcgen::PublicKeyData::subject_public_key_info(&key);
        assert_eq!(spki[0], 0x30);
        assert_eq!(pin(cert.der()).unwrap(), sha256_hex(&spki));
        assert!(pin(b"not a certificate").is_err());
        assert_eq!(
            sha256_hex(b""),
            "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
        );
    }
}
