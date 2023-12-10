extern crate nix;
mod daemon;
mod filesystem;
mod network;

use crate::daemon::daemon_start;
use crate::filesystem::mount_virtual_filesystems;
use crate::network::configure_network_devices;

use log::{error, info, warn, LevelFilter};
use nix::unistd::Uid;
use simple_logger::SimpleLogger;

#[tokio::main]
async fn main() {
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
        .with_level(LevelFilter::Info)
        .with_module_level("feos::filesystem", LevelFilter::Debug)
        .with_utc_timestamps()
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
    match daemon_start().await {
        Err(e) => error!("FeOS daemon crashed: {}", e),
        _ => error!("FeOS daemon exited."),
    }
}
