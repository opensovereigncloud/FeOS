use std::thread;
use std::time::Duration;

use futures::stream::TryStreamExt;
use rtnetlink::{new_connection, Error, Handle};
use netlink_packet_route::link::nlas::Nla;

#[tokio::main]
async fn main() -> Result<(), ()> {
    println!("

    ███████╗███████╗ ██████╗ ███████╗
    ██╔════╝██╔════╝██╔═══██╗██╔════╝
    █████╗  █████╗  ██║   ██║███████╗
    ██╔══╝  ██╔══╝  ██║   ██║╚════██║
    ██║     ███████╗╚██████╔╝███████║
    ╚═╝     ╚══════╝ ╚═════╝ ╚══════╝
                 v{}
    ", env!("CARGO_PKG_VERSION"));

    env_logger::init();

    // Special stuff for pid 1
    if std::process::id() == 1 { 
        mount_virtual_filesystems();
        configure_network_devices().await;
    }

    feos_daemon();
    
    // loop forever if pid == 1
    if std::process::id() == 1 { 
        loop {
            thread::sleep(Duration::from_secs(1))
        }
    }
    Ok(())
}

fn mount_virtual_filesystems() {
    // TODO: Mount virtual filesystems /dev /sys /proc 
}

async fn configure_network_devices() {
    // TODO: configure network devices

    let (connection, handle, _) = new_connection().unwrap();
    tokio::spawn(connection);

    println!("*** dumping links ***");
    if let Err(e) = dump_links(handle.clone()).await {
        eprintln!("{e}");
    }
    let name = "eth0";
    println!("\n*** retrieving link named \"{name}\" ***");
    if let Err(e) = get_link_by_name(handle.clone(), name.to_string()).await {
        eprintln!("{e}");
    }
}

fn feos_daemon() {
    // TODO: implement feos daemon stuff
}







async fn dump_links(handle: Handle) -> Result<(), Error> {
    let mut links = handle.link().get().execute();
    while let Some(msg) = links.try_next().await? {
        for nla in msg.nlas.into_iter() {
            if let Nla::IfName(name) = nla {
                println!("found link {} ({})", msg.header.index, name);
                if let Err(e) = dump_addresses(handle.clone(), name).await {
                    eprintln!("{e}");
                }
                //continue 'outer;
            }
            else if let Nla::Address(ether_addr) = nla {
                println!("  EtherAddr: {}", format_mac(&ether_addr));
            }
            else if let Nla::Carrier(carrier) = nla {
                let status = if carrier > 0 {"UP"} else {"DOWN"};
                println!("  Carrier: {}", status);
            }
            else if let Nla::Mtu(mtu) = nla {
                println!("  MTU: {}", mtu);
            }
        }

        eprintln!("found link {}, but the link has no name", msg.header.index);
    }
    Ok(())
}

async fn get_link_by_name(handle: Handle, name: String) -> Result<(), Error> {
    let mut links = handle.link().get().match_name(name.clone()).execute();
    if (links.try_next().await?).is_some() {
        println!("found link {name}");
        // We should only have one link with that name
        assert!(links.try_next().await?.is_none());
    } else {
        println!("no link link {name} found");
    }
    Ok(())
}

async fn dump_addresses(handle: Handle, link: String) -> Result<(), Error> {
    let mut links = handle.link().get().match_name(link.clone()).execute();
    if let Some(link) = links.try_next().await? {
        let mut addresses = handle
            .address()
            .get()
            .set_link_index_filter(link.header.index)
            .execute();
        while let Some(msg) = addresses.try_next().await? {
            println!("{msg:?}");
        }
        Ok(())
    } else {
        eprintln!("link {link} not found");
        Ok(())
    }
}

fn format_mac(bytes: &Vec<u8>) -> String {
    bytes
        .iter()
        .map(|byte| format!("{:02x}", byte))
        .collect::<Vec<String>>()
        .join(":")
}