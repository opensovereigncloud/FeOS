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
    
    loop {
        thread::sleep(Duration::from_secs(1));
    }
}

async fn dump_links(handle: Handle) -> Result<(), Error> {
    let mut links = handle.link().get().execute();
    'outer: while let Some(msg) = links.try_next().await? {
        for nla in msg.nlas.into_iter() {
            if let Nla::IfName(name) = nla {
                println!("found link {} ({})", msg.header.index, name);
                continue 'outer;
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