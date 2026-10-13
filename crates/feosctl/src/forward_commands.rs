// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

use anyhow::{bail, Result};
use clap::{Args, Subcommand};
use feos_proto::kube_service::{kube_service_client::KubeServiceClient, SetListenersRequest};
use feos_proto::portforward_service::{
    forward_request, forward_response, port_forward_service_client::PortForwardServiceClient,
    ForwardRequest, Open,
};
use feos_proto::ssh_service::{ssh_service_client::SshServiceClient, SetAgentRequest};
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::{TcpListener, TcpStream};
use tonic::transport::Channel;

/// The forwarding services are only on a guest's API socket and vsock.
#[derive(Args, Debug)]
pub struct GuestAddress {
    #[arg(
        short,
        long,
        global = true,
        env = "FEOS_GUEST_ADDRESS",
        default_value = "unix:/run/feos/api.sock"
    )]
    pub address: String,
}

/// Forward local TCP connections to a port on the guest's loopback
#[derive(Args, Debug)]
pub struct ForwardArgs {
    #[command(flatten)]
    pub guest: GuestAddress,

    /// The port in the guest.
    #[arg(long)]
    pub port: u32,

    /// Where to accept connections on this machine.
    #[arg(long, default_value = "127.0.0.1:8080")]
    pub listen: String,
}

pub async fn handle_forward_command(args: ForwardArgs) -> Result<()> {
    let channel = crate::transport::channel(&args.guest.address).await?;
    let listener = TcpListener::bind(&args.listen).await?;
    println!("forwarding {} to guest port {}", args.listen, args.port);
    loop {
        let (local, peer) = listener.accept().await?;
        let client = PortForwardServiceClient::new(channel.clone());
        let port = args.port;
        tokio::spawn(async move {
            if let Err(e) = forward(client, local, port).await {
                eprintln!("{peer}: {e}");
            }
        });
    }
}

async fn forward(
    mut client: PortForwardServiceClient<Channel>,
    local: TcpStream,
    port: u32,
) -> Result<()> {
    let (mut local_read, mut local_write) = local.into_split();
    let (tx, rx) = tokio::sync::mpsc::channel(16);
    tx.send(ForwardRequest {
        frame: Some(forward_request::Frame::Open(Open { port })),
    })
    .await?;
    let mut responses = client
        .forward(tokio_stream::wrappers::ReceiverStream::new(rx))
        .await?
        .into_inner();

    let upload = tokio::spawn(async move {
        let mut buf = vec![0u8; 16 * 1024];
        loop {
            match local_read.read(&mut buf).await {
                Ok(0) | Err(_) => break,
                Ok(n) => {
                    let frame = forward_request::Frame::Data(buf[..n].to_vec());
                    if tx
                        .send(ForwardRequest { frame: Some(frame) })
                        .await
                        .is_err()
                    {
                        break;
                    }
                }
            }
        }
        // Dropping the sender ends the request stream: the guest side
        // closes its write half.
    });
    while let Some(response) = responses.message().await? {
        match response.frame {
            Some(forward_response::Frame::Data(bytes)) => local_write.write_all(&bytes).await?,
            Some(forward_response::Frame::Ready(_)) | None => {}
        }
    }
    local_write.shutdown().await?;
    upload.abort();
    Ok(())
}

/// Kubernetes credential offloading in a guest
#[derive(Args, Debug)]
pub struct KubeArgs {
    #[command(flatten)]
    pub guest: GuestAddress,

    #[command(subcommand)]
    command: KubeCommand,
}

#[derive(Subcommand, Debug)]
pub enum KubeCommand {
    /// Set the complete set of loopback ports piped to the host's kube proxy
    SetListeners {
        ports: Vec<u32>,
        #[arg(long, help = "Close the connections opened before")]
        reset_connections: bool,
    },
}

pub async fn handle_kube_command(args: KubeArgs) -> Result<()> {
    let mut client = KubeServiceClient::new(crate::transport::channel(&args.guest.address).await?);
    match args.command {
        KubeCommand::SetListeners {
            ports,
            reset_connections,
        } => {
            let r = client
                .set_listeners(SetListenersRequest {
                    ports,
                    reset_connections,
                })
                .await?
                .into_inner();
            println!("listening: {:?}", r.listening);
            for failure in &r.failed {
                eprintln!("port {}: {}", failure.port, failure.reason);
            }
            if !r.failed.is_empty() {
                bail!("some ports could not be bound");
            }
        }
    }
    Ok(())
}

/// SSH agent offloading in a guest
#[derive(Args, Debug)]
pub struct SshArgs {
    #[command(flatten)]
    pub guest: GuestAddress,

    #[command(subcommand)]
    command: SshCommand,
}

#[derive(Subcommand, Debug)]
pub enum SshCommand {
    /// Create or remove the agent socket containers see at /run/feos/ssh-agent.sock
    SetAgent {
        #[arg(long)]
        enabled: bool,
        #[arg(long, help = "Close the connections opened before")]
        reset_connections: bool,
    },
}

pub async fn handle_ssh_command(args: SshArgs) -> Result<()> {
    let mut client = SshServiceClient::new(crate::transport::channel(&args.guest.address).await?);
    match args.command {
        SshCommand::SetAgent {
            enabled,
            reset_connections,
        } => {
            let r = client
                .set_agent(SetAgentRequest {
                    enabled,
                    reset_connections,
                })
                .await?
                .into_inner();
            if !r.error.is_empty() {
                bail!(r.error);
            }
            println!("listening: {}", r.listening);
        }
    }
    Ok(())
}
