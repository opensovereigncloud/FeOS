// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! Drives the node's NAT64 by hand, as feosd would; hack/nat64/test uses it.
//!
//!     nat64 [--pin-dir DIR] [--prefix PREFIX] [--pool POOL/LEN] COMMAND

use clap::{Parser, Subcommand};
use nat64::{Config, Nat64};
use std::io;
use std::net::{Ipv4Addr, Ipv6Addr};
use std::path::PathBuf;

#[derive(Parser)]
struct Args {
    /// Where the maps are pinned.
    #[arg(long, default_value = nat64::PIN_DIR)]
    pin_dir: PathBuf,
    /// The NAT64 prefix, a /96.
    #[arg(long, default_value_t = nat64::PREFIX)]
    prefix: Ipv6Addr,
    /// The pods' IPv4 pool.
    #[arg(long, default_value = "10.200.0.0/16")]
    pool: String,
    #[command(subcommand)]
    command: Command,
}

#[derive(Subcommand)]
enum Command {
    /// Create or take over the device, its routes and the program.
    Attach,
    /// Remove the device and the pins.
    Detach,
    /// Give a pod an IPv4 address.
    Map { pod: Ipv6Addr, ipv4: Ipv4Addr },
    /// Take a pod's IPv4 address away.
    Unmap { pod: Ipv6Addr },
    /// List the pods' IPv4 addresses.
    List,
    /// Print the counters.
    Stats,
}

#[tokio::main]
async fn main() -> io::Result<()> {
    env_logger::Builder::from_env(env_logger::Env::default().default_filter_or("info")).init();
    let args = Args::parse();
    let invalid = || io::Error::new(io::ErrorKind::InvalidInput, "--pool is ADDRESS/LENGTH");
    let (pool, pool_len) = args.pool.split_once('/').ok_or_else(invalid)?;
    let config = Config {
        prefix: args.prefix,
        pool: pool.parse().map_err(|_| invalid())?,
        pool_len: pool_len.parse().map_err(|_| invalid())?,
        pin_dir: args.pin_dir,
    };
    match args.command {
        Command::Attach => {
            Nat64::attach(&config).await?;
        }
        Command::Detach => Nat64::detach(&config.pin_dir).await?,
        Command::Map { pod, ipv4 } => Nat64::open(&config)?.map(pod, ipv4)?,
        Command::Unmap { pod } => Nat64::open(&config)?.unmap(pod)?,
        Command::List => {
            for (pod, ipv4) in Nat64::open(&config)?.mappings()? {
                println!("{pod} {ipv4}");
            }
        }
        Command::Stats => {
            let stats = Nat64::open(&config)?.stats()?;
            println!("six_to_four {}", stats.six_to_four);
            println!("four_to_six {}", stats.four_to_six);
            println!("no_mapping {}", stats.no_mapping);
            println!("denied {}", stats.denied);
            println!("unsupported {}", stats.unsupported);
            println!("errors {}", stats.errors);
            println!("udp_checksums {}", stats.udp_checksums);
        }
    }
    Ok(())
}
