// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

use anyhow::Result;
use clap::{Parser, Subcommand};

mod container_commands;
mod host_commands;
mod image_commands;
mod tool_commands;
mod transport;
mod vm_commands;
mod watch_commands;

#[derive(Parser, Debug)]
#[command(author, version, about, long_about = None)]
struct Cli {
    #[command(subcommand)]
    service: Service,
}

#[derive(Subcommand, Debug)]
enum Service {
    Vm(vm_commands::VmArgs),
    Host(host_commands::HostArgs),
    Image(image_commands::ImageArgs),
    Container(container_commands::ContainerArgs),
    /// Agent tools on a container's files, in a guest
    Tool(tool_commands::ToolArgs),
    Watch(watch_commands::WatchArgs),
}

#[tokio::main]
async fn main() -> Result<()> {
    env_logger::Builder::new()
        .filter_level(log::LevelFilter::Warn)
        .parse_default_env()
        .init();

    let cli = Cli::parse();

    match cli.service {
        Service::Vm(args) => vm_commands::handle_vm_command(args).await?,
        Service::Host(args) => host_commands::handle_host_command(args).await?,
        Service::Image(args) => image_commands::handle_image_command(args).await?,
        Service::Container(args) => container_commands::handle_container_command(args).await?,
        Service::Tool(args) => tool_commands::handle_tool_command(args).await?,
        Service::Watch(args) => watch_commands::handle_watch_command(args).await?,
    }

    Ok(())
}
