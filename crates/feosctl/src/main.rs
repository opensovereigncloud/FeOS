// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

use anyhow::Result;
use clap::{Parser, Subcommand};

mod container_commands;
mod forward_commands;
mod host_commands;
mod image_commands;
mod pod_commands;
mod security_group_commands;
mod tool_commands;
mod transport;
mod watch_commands;

#[derive(Parser, Debug)]
#[command(author, version, about, long_about = None)]
struct Cli {
    #[command(subcommand)]
    service: Service,
}

#[derive(Subcommand, Debug)]
enum Service {
    /// Pods: a microVM and its containers
    Pod(pod_commands::PodArgs),
    /// Security groups: what pods may send and receive
    #[command(alias = "sg")]
    SecurityGroup(security_group_commands::SecurityGroupArgs),
    Host(host_commands::HostArgs),
    Image(image_commands::ImageArgs),
    Container(container_commands::ContainerArgs),
    /// Agent tools on a container's files, in a guest
    Tool(tool_commands::ToolArgs),
    Watch(watch_commands::WatchArgs),
    Forward(forward_commands::ForwardArgs),
    Kube(forward_commands::KubeArgs),
    Ssh(forward_commands::SshArgs),
}

#[tokio::main]
async fn main() -> Result<()> {
    env_logger::Builder::new()
        .filter_level(log::LevelFilter::Warn)
        .parse_default_env()
        .init();

    let cli = Cli::parse();

    match cli.service {
        Service::Pod(args) => pod_commands::handle_pod_command(args).await?,
        Service::SecurityGroup(args) => {
            security_group_commands::handle_security_group_command(args).await?
        }
        Service::Host(args) => host_commands::handle_host_command(args).await?,
        Service::Image(args) => image_commands::handle_image_command(args).await?,
        Service::Container(args) => container_commands::handle_container_command(args).await?,
        Service::Tool(args) => tool_commands::handle_tool_command(args).await?,
        Service::Watch(args) => watch_commands::handle_watch_command(args).await?,
        Service::Forward(args) => forward_commands::handle_forward_command(args).await?,
        Service::Kube(args) => forward_commands::handle_kube_command(args).await?,
        Service::Ssh(args) => forward_commands::handle_ssh_command(args).await?,
    }

    Ok(())
}
