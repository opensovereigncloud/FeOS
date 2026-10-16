// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! A node's persistent volumes (ADR 0016). A pod names its volumes when it
//! is created (`feosctl pod create --volume`), and has them until it is
//! deleted.

use anyhow::Result;
use clap::{Args, Subcommand};
use feos_proto::volume_service::{
    volume_service_client::VolumeServiceClient, CreateVolumeRequest, DeleteVolumeRequest,
    GetVolumeRequest, ListVolumesRequest, Volume, VolumeState,
};
use std::collections::HashMap;

#[derive(Args, Debug)]
pub struct VolumeArgs {
    #[arg(
        short,
        long,
        global = true,
        env = "FEOS_ADDRESS",
        default_value = "unix:/run/feos/api.sock"
    )]
    pub address: String,

    #[command(subcommand)]
    command: VolumeCommand,
}

#[derive(Subcommand, Debug)]
pub enum VolumeCommand {
    /// Create a volume, attached to no pod
    Create {
        #[arg(long, help = "The volume's ID, a UUID; a new one when not given")]
        volume_id: Option<String>,
        #[arg(
            long,
            value_parser = parse_size,
            help = "Its size, e.g. 512M, 8G or 1T: from 64M to 16T, in whole MiB"
        )]
        size: u64,
        #[arg(long, value_parser = parse_key_val, help = "KEY=value (repeatable)")]
        label: Vec<(String, String)>,
    },
    /// Delete a volume that no pod has, and what the node holds of it
    Delete { volume_id: String },
    /// Show a volume
    Get { volume_id: String },
    /// List the volumes
    List,
}

/// A size in bytes, or with a binary suffix: K, M, G, T.
fn parse_size(s: &str) -> Result<u64, String> {
    let s = s.trim();
    let (number, shift) = match s.char_indices().last() {
        Some((i, 'K' | 'k')) => (&s[..i], 10),
        Some((i, 'M' | 'm')) => (&s[..i], 20),
        Some((i, 'G' | 'g')) => (&s[..i], 30),
        Some((i, 'T' | 't')) => (&s[..i], 40),
        _ => (s, 0),
    };
    number
        .parse::<u64>()
        .ok()
        .and_then(|n| n.checked_mul(1 << shift))
        .ok_or_else(|| format!("{s:?} is not a size, such as 512M or 8G"))
}

fn parse_key_val(s: &str) -> Result<(String, String), String> {
    s.split_once('=')
        .map(|(key, value)| (key.to_string(), value.to_string()))
        .ok_or_else(|| format!("invalid KEY=value format: {s}"))
}

pub async fn handle_volume_command(args: VolumeArgs) -> Result<()> {
    let mut client = VolumeServiceClient::new(crate::transport::channel(&args.address).await?);
    match args.command {
        VolumeCommand::Create {
            volume_id,
            size,
            label,
        } => {
            let volume = client
                .create_volume(CreateVolumeRequest {
                    volume_id: volume_id.unwrap_or_else(|| uuid::Uuid::new_v4().to_string()),
                    size_bytes: size,
                    labels: label.into_iter().collect::<HashMap<_, _>>(),
                })
                .await?
                .into_inner();
            println!(
                "Volume {} has {} MiB",
                volume.volume_id,
                volume.size_bytes >> 20
            );
        }
        VolumeCommand::Delete { volume_id } => {
            client
                .delete_volume(DeleteVolumeRequest {
                    volume_id: volume_id.clone(),
                })
                .await?;
            println!("Volume {volume_id} is deleted");
        }
        VolumeCommand::Get { volume_id } => {
            let volume = client
                .get_volume(GetVolumeRequest { volume_id })
                .await?
                .into_inner();
            print_volume(&volume);
        }
        VolumeCommand::List => {
            let volumes = client
                .list_volumes(ListVolumesRequest {})
                .await?
                .into_inner()
                .volumes;
            if volumes.is_empty() {
                println!("No volumes");
                return Ok(());
            }
            println!(
                "{:<38} {:<13} {:>10} {:<10} POD",
                "VOLUME_ID", "STATE", "SIZE", "FORMATTED"
            );
            println!(
                "{:-<38} {:-<13} {:->10} {:-<10} {:-<36}",
                "", "", "", "", ""
            );
            for volume in volumes {
                println!(
                    "{:<38} {:<13} {:>10} {:<10} {}",
                    volume.volume_id,
                    state_name(volume.state()),
                    format!("{} MiB", volume.size_bytes >> 20),
                    if volume.formatted { "yes" } else { "no" },
                    volume.pod_id
                );
            }
        }
    }
    Ok(())
}

fn print_volume(volume: &Volume) {
    println!("Volume {}", volume.volume_id);
    println!("  State: {}", state_name(volume.state()));
    println!("  Size: {} MiB", volume.size_bytes >> 20);
    println!(
        "  Filesystem: {}",
        if volume.formatted {
            "yes"
        } else {
            "not yet: the first boot that has it makes it"
        }
    );
    if !volume.pod_id.is_empty() {
        println!("  Pod: {}", volume.pod_id);
    }
    if let Some(checkpoint) = &volume.checkpoint {
        println!("  Checkpoint: {}", checkpoint.head_key);
    }
    let mut labels: Vec<_> = volume.labels.iter().collect();
    labels.sort();
    for (key, value) in labels {
        println!("  Label: {key}={value}");
    }
}

fn state_name(state: VolumeState) -> &'static str {
    match state {
        VolumeState::Available => "available",
        VolumeState::Attached => "attached",
        VolumeState::Checkpointed => "checkpointed",
        VolumeState::Unspecified => "unknown",
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn sizes_take_binary_suffixes() {
        assert_eq!(parse_size("67108864"), Ok(64 << 20));
        assert_eq!(parse_size("512M"), Ok(512 << 20));
        assert_eq!(parse_size("8G"), Ok(8 << 30));
        assert_eq!(parse_size("1t"), Ok(1 << 40));
        assert!(parse_size("8GB").is_err());
        assert!(parse_size("").is_err());
        assert!(parse_size("99999999999T").is_err());
    }
}
