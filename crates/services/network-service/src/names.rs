// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! What a valid name table is (ADR 0010, ADR 0018), and a table as the
//! resolver reads it.

use crate::group::valid_id;
use feos_proto::network_service::{NameEntry, NameTable};
use std::collections::{BTreeMap, BTreeSet, HashMap};
use std::net::IpAddr;
use tonic::Status;

/// A table as the resolver reads it: its names, in lower case, with their
/// addresses, and the tables it includes.
#[derive(Clone, Debug, PartialEq)]
pub(crate) struct Table {
    /// The table as it was put, normalized.
    pub table: NameTable,
    pub names: HashMap<String, Vec<IpAddr>>,
}

impl Table {
    /// Reads a normalized table.
    pub fn new(table: NameTable) -> Self {
        let names = table
            .entries
            .iter()
            .map(|entry| {
                let addresses = entry
                    .addresses
                    .iter()
                    .filter_map(|address| address.parse().ok())
                    .collect();
                (entry.name.clone(), addresses)
            })
            .collect();
        Self { table, names }
    }
}

/// Checks a table's ID: 1 to 64 letters, digits, "-", "_" and ".".
pub fn check_table_id(id: &str) -> Result<(), Status> {
    if valid_id(id) {
        Ok(())
    } else {
        Err(Status::invalid_argument(format!(
            "invalid name table ID {id:?}: 1 to 64 letters, digits, \"-\", \"_\" and \".\""
        )))
    }
}

/// Whether `name` is one label of a DNS name, as a table holds it: 1 to 63
/// letters, digits and "-", "-" neither first nor last.
pub(crate) fn valid_label(name: &str) -> bool {
    (1..=63).contains(&name.len())
        && name
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || byte == b'-')
        && !name.starts_with('-')
        && !name.ends_with('-')
}

/// A name of an entry or of a removal, in lower case.
pub(crate) fn label(name: &str) -> Result<String, Status> {
    if valid_label(name) {
        Ok(name.to_ascii_lowercase())
    } else {
        Err(Status::invalid_argument(format!(
            "name {name:?} is not one label of a DNS name: 1 to 63 letters, digits and \"-\", \
             \"-\" neither first nor last"
        )))
    }
}

/// Checks an entry, and writes its name in lower case and its addresses
/// as a sorted set in canonical form.
pub(crate) fn normalize_entry(entry: &NameEntry) -> Result<NameEntry, Status> {
    let name = label(&entry.name)?;
    let addresses = entry
        .addresses
        .iter()
        .map(|address| {
            address.parse::<IpAddr>().map_err(|_| {
                Status::invalid_argument(format!(
                    "address {address:?} of name {name} is not an IP address"
                ))
            })
        })
        .collect::<Result<BTreeSet<IpAddr>, Status>>()?;
    if addresses.is_empty() {
        return Err(Status::invalid_argument(format!(
            "name {name} has no address"
        )));
    }
    Ok(NameEntry {
        name,
        addresses: addresses.iter().map(IpAddr::to_string).collect(),
    })
}

/// Checks a table, and writes its entries sorted by name, each normalized,
/// so that two tables compare as they answer.
pub(crate) fn normalize(table: NameTable) -> Result<NameTable, Status> {
    check_table_id(&table.id)?;
    for include in &table.includes {
        if !valid_id(include) {
            return Err(Status::invalid_argument(format!(
                "name table {} includes {include:?}, which is no table ID",
                table.id
            )));
        }
    }
    let mut entries = BTreeMap::new();
    for entry in &table.entries {
        let entry = normalize_entry(entry)?;
        if entries.contains_key(&entry.name) {
            return Err(Status::invalid_argument(format!(
                "name table {} has name {} twice",
                table.id, entry.name
            )));
        }
        entries.insert(entry.name.clone(), entry);
    }
    Ok(NameTable {
        entries: entries.into_values().collect(),
        ..table
    })
}

#[cfg(test)]
mod tests {
    use super::*;
    use tonic::Code;

    fn entry(name: &str, addresses: &[&str]) -> NameEntry {
        NameEntry {
            name: name.to_string(),
            addresses: addresses.iter().map(|a| a.to_string()).collect(),
        }
    }

    fn table(entries: Vec<NameEntry>, includes: &[&str]) -> NameTable {
        NameTable {
            id: "env-1".to_string(),
            version: 7,
            entries,
            includes: includes.iter().map(|i| i.to_string()).collect(),
        }
    }

    #[test]
    fn names_are_single_labels() {
        for name in ["a", "web", "Postgres", "db-1", "1db", &"a".repeat(63)] {
            assert!(valid_label(name), "{name}");
        }
        for name in [
            "",
            "-db",
            "db-",
            "a.b",
            "db_1",
            "db.",
            "über",
            "a b",
            &"a".repeat(64),
        ] {
            assert!(!valid_label(name), "{name}");
        }
    }

    #[test]
    fn a_table_is_kept_canonical_and_sorted() {
        let got = normalize(table(
            vec![
                entry("Web", &["fd00::2", "fd00:0::1", "fd00::2"]),
                entry("api", &["127.0.0.1", "::1"]),
            ],
            &["env-2"],
        ))
        .unwrap();
        assert_eq!(
            got.entries,
            [
                entry("api", &["127.0.0.1", "::1"]),
                entry("web", &["fd00::1", "fd00::2"]),
            ]
        );
        assert_eq!((got.id.as_str(), got.version), ("env-1", 7));
        assert_eq!(got.includes, ["env-2"]);
        let read = Table::new(got);
        assert_eq!(
            read.names["web"],
            [
                "fd00::1".parse::<IpAddr>().unwrap(),
                "fd00::2".parse().unwrap()
            ]
        );
    }

    #[test]
    fn bad_tables_are_refused() {
        for (bad, why) in [
            (table(vec![entry("a.b", &["fd00::1"])], &[]), "two labels"),
            (table(vec![entry("web", &[])], &[]), "no address"),
            (table(vec![entry("web", &["fd00::x"])], &[]), "bad address"),
            (
                table(
                    vec![entry("web", &["fd00::1"]), entry("WEB", &["fd00::2"])],
                    &[],
                ),
                "a name twice",
            ),
            (table(vec![], &["a/b"]), "bad include"),
            (
                NameTable {
                    id: String::new(),
                    ..table(vec![], &[])
                },
                "no ID",
            ),
        ] {
            assert_eq!(
                normalize(bad).unwrap_err().code(),
                Code::InvalidArgument,
                "{why}"
            );
        }
    }
}
