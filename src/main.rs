extern crate nix;
mod filesystem;
mod network;

use crate::filesystem::mount_virtual_filesystems;
use crate::network::configure_network_devices;
use std::time::Duration;

#[tokio::main]
async fn main() -> Result<(), ()> {
    println!(
        "

    ███████╗███████╗ ██████╗ ███████╗
    ██╔════╝██╔════╝██╔═══██╗██╔════╝
    █████╗  █████╗  ██║   ██║███████╗
    ██╔══╝  ██╔══╝  ██║   ██║╚════██║
    ██║     ███████╗╚██████╔╝███████║
    ╚═╝     ╚══════╝ ╚═════╝ ╚══════╝
                 v{}
    ",
        env!("CARGO_PKG_VERSION")
    );

    env_logger::init();

    let pid1 = std::process::id() == 1;

    // Special stuff for pid 1
    if pid1 {
        mount_virtual_filesystems();
        configure_network_devices().await;
    }

    feos_daemon();

    // loop forever if pid == 1
    if pid1 {
        loop {
            tokio::time::sleep(Duration::from_secs(1)).await;
        }
    }
    Ok(())
}

fn feos_daemon() {
    // TODO: implement feos daemon stuff
}
