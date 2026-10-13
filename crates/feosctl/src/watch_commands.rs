// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

use anyhow::{bail, Result};
use clap::Args;
use feos_proto::watch_service::{
    watch_request, watch_response, watch_service_client::WatchServiceClient, EventKind, Open,
    Subscribe, WatchRequest,
};

/// Print the changes to paths in a container as they happen
#[derive(Args, Debug)]
pub struct WatchArgs {
    /// The watch service is only on a guest's API socket and vsock.
    #[arg(
        short,
        long,
        env = "FEOS_GUEST_ADDRESS",
        default_value = "unix:/run/feos/api.sock"
    )]
    pub address: String,

    /// The container whose root the paths are in.
    #[arg(short, long, default_value = "main")]
    pub container: String,

    /// Directories or files to watch.
    #[arg(required = true)]
    pub paths: Vec<String>,
}

pub async fn handle_watch_command(args: WatchArgs) -> Result<()> {
    let mut client = WatchServiceClient::new(crate::transport::channel(&args.address).await?);
    let (tx, rx) = tokio::sync::mpsc::channel(4);
    tx.send(WatchRequest {
        frame: Some(watch_request::Frame::Open(Open {
            container: args.container,
        })),
    })
    .await?;
    tx.send(WatchRequest {
        frame: Some(watch_request::Frame::Subscribe(Subscribe {
            paths: args.paths,
        })),
    })
    .await?;
    let mut responses = client
        .watch(tokio_stream::wrappers::ReceiverStream::new(rx))
        .await?
        .into_inner();
    while let Some(response) = responses.message().await? {
        match response.frame {
            Some(watch_response::Frame::Ack(ack)) => {
                for failed in &ack.failed {
                    eprintln!("cannot watch {}: {}", failed.path, failed.error);
                }
                println!("watching {}", ack.watching.join(" "));
            }
            Some(watch_response::Frame::Events(batch)) => {
                for event in batch.events {
                    let kind = EventKind::try_from(event.kind).unwrap_or_default();
                    if kind == EventKind::Stale {
                        println!("stale {}", event.stale_paths.join(" "));
                    } else {
                        let name = if event.name.is_empty() {
                            event.dir
                        } else {
                            format!("{}/{}", event.dir.trim_end_matches('/'), event.name)
                        };
                        println!("{} {name}", kind.as_str_name());
                    }
                }
            }
            None => {}
        }
    }
    // Keep the request stream open until the server ends the call.
    drop(tx);
    bail!("the watch ended")
}
