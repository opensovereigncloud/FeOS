extern crate nix;
mod filesystem;
mod network;

use crate::filesystem::mount_virtual_filesystems;
use crate::network::configure_network_devices;
use nix::unistd::Uid;
use simple_logger::SimpleLogger;
use std::time::Duration;

use log::{info, warn, LevelFilter};

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

    SimpleLogger::new()
        .with_level(LevelFilter::Debug)
        .init()
        .unwrap();

    // if not run as root, print warning.
    if !Uid::current().is_root() {
        warn!("Not running as root! (uid: {})", Uid::current());
    }

    // Special stuff for pid 1
    if std::process::id() == 1 {
        info!("Mounting virtual filesystems...");
        mount_virtual_filesystems();

        info!("Configuring network devices...");
        configure_network_devices().await;
    }

    info!("Starting FeOS daemon...");
    feos_daemon()
}

fn feos_daemon() -> Result<(), ()> {
    // TODO: implement feos daemon stuff

    loop {
        std::thread::sleep(Duration::from_secs(1));
    }
}
