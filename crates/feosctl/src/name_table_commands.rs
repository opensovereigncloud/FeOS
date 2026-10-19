// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! A node's name tables (ADR 0010, ADR 0018), whose names the node's
//! resolver answers the pods that name them. The control plane sends them;
//! these commands are for development and for looking at what a node has.

use anyhow::Result;
use clap::{Args, Subcommand};
use feos_proto::network_service::{
    name_service_client::NameServiceClient, DeleteNameTableRequest, GetNameTableRequest,
    ListNameTablesRequest, NameEntry, NameTable, PutNameTableRequest, UpdateNameTableRequest,
};

#[derive(Args, Debug)]
pub struct NameTableArgs {
    #[arg(
        short,
        long,
        global = true,
        env = "FEOS_ADDRESS",
        default_value = "unix:/run/feos/api.sock"
    )]
    pub address: String,

    #[command(subcommand)]
    command: NameTableCommand,
}

#[derive(Subcommand, Debug)]
pub enum NameTableCommand {
    /// Put a table whole, replacing the one the node has
    Put {
        id: String,
        #[arg(long, default_value_t = 1)]
        version: u64,
        #[arg(
            long = "name",
            value_name = "NAME=ADDRESS[,ADDRESS...]",
            value_parser = parse_entry,
            help = "A name of one label and its addresses (repeatable)"
        )]
        entries: Vec<NameEntry>,
        #[arg(
            long = "include",
            value_name = "ID",
            help = "A table whose names the pods resolve too, after this one's (repeatable)"
        )]
        includes: Vec<String>,
    },
    /// Put and remove names of a table, from one version to the next
    Update {
        id: String,
        #[arg(long, help = "The version the node must have")]
        from_version: u64,
        #[arg(
            long,
            help = "The version after the change; the next one when not given"
        )]
        version: Option<u64>,
        #[arg(
            long = "name",
            value_name = "NAME=ADDRESS[,ADDRESS...]",
            value_parser = parse_entry,
            help = "A name to put, with its addresses (repeatable)"
        )]
        put: Vec<NameEntry>,
        #[arg(long, value_name = "NAME", help = "A name to remove (repeatable)")]
        remove: Vec<String>,
    },
    /// Remove a table that no pod on the node names
    Delete { id: String },
    /// Show a table: its names and includes
    Get { id: String },
    /// List the tables
    List,
}

pub async fn handle_name_table_command(args: NameTableArgs) -> Result<()> {
    let mut client = NameServiceClient::new(crate::transport::channel(&args.address).await?);

    match args.command {
        NameTableCommand::Put {
            id,
            version,
            entries,
            includes,
        } => {
            let table = NameTable {
                id: id.clone(),
                version,
                entries,
                includes,
            };
            client
                .put_name_table(PutNameTableRequest { table: Some(table) })
                .await?;
            println!("Name table {id} is at version {version}");
        }
        NameTableCommand::Update {
            id,
            from_version,
            version,
            put,
            remove,
        } => {
            let version = version.unwrap_or(from_version.wrapping_add(1));
            client
                .update_name_table(UpdateNameTableRequest {
                    id: id.clone(),
                    from_version,
                    version,
                    put_entries: put,
                    remove_names: remove,
                })
                .await?;
            println!("Name table {id} is at version {version}");
        }
        NameTableCommand::Delete { id } => {
            client
                .delete_name_table(DeleteNameTableRequest { id: id.clone() })
                .await?;
            println!("Name table {id} is deleted");
        }
        NameTableCommand::Get { id } => {
            let table = client
                .get_name_table(GetNameTableRequest { id })
                .await?
                .into_inner();
            println!("Name table {}", table.id);
            println!("  Version: {}", table.version);
            if !table.includes.is_empty() {
                println!("  Includes: {}", table.includes.join(", "));
            }
            println!("  Names:");
            for entry in &table.entries {
                println!("    {} {}", entry.name, entry.addresses.join(" "));
            }
        }
        NameTableCommand::List => {
            let tables = client
                .list_name_tables(ListNameTablesRequest {})
                .await?
                .into_inner()
                .tables;
            if tables.is_empty() {
                println!("No name tables");
                return Ok(());
            }
            println!("{:<44} {:<20} {:<6} INCLUDES", "ID", "VERSION", "NAMES");
            println!("{:-<44} {:-<20} {:-<6} {:-<8}", "", "", "", "");
            for table in tables {
                println!(
                    "{:<44} {:<20} {:<6} {}",
                    table.id,
                    table.version,
                    table.entries.len(),
                    table.includes.len()
                );
            }
        }
    }
    Ok(())
}

/// NAME=ADDRESS[,ADDRESS...]: the node checks the name and the addresses.
fn parse_entry(s: &str) -> Result<NameEntry, String> {
    let (name, addresses) = s
        .split_once('=')
        .ok_or_else(|| format!("{s:?} is not NAME=ADDRESS[,ADDRESS...]"))?;
    let addresses: Vec<String> = addresses
        .split(',')
        .filter(|address| !address.is_empty())
        .map(str::to_string)
        .collect();
    if name.is_empty() || addresses.is_empty() {
        return Err(format!("{s:?} is not NAME=ADDRESS[,ADDRESS...]"));
    }
    Ok(NameEntry {
        name: name.to_string(),
        addresses,
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn entries_parse() {
        let entry = parse_entry("web=fd00::1,fd00::2").unwrap();
        assert_eq!(entry.name, "web");
        assert_eq!(entry.addresses, ["fd00::1", "fd00::2"]);
        for bad in ["web", "=fd00::1", "web=", "web=,"] {
            assert!(parse_entry(bad).is_err(), "{bad}");
        }
    }
}
