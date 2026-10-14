// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

use anyhow::{bail, Result};
use clap::{Args, Subcommand};
use feos_proto::tool_service::{
    tool_service_client::ToolServiceClient, EditOp, EditRequest, FdRequest, LsRequest, ReadRequest,
    RgRequest, WebfetchRequest, WriteRequest,
};

#[derive(Args, Debug)]
pub struct ToolArgs {
    /// The tool service is only on a guest's API socket and vsock.
    #[arg(
        short,
        long,
        global = true,
        env = "FEOS_GUEST_ADDRESS",
        default_value = "unix:/run/feos/api.sock"
    )]
    pub address: String,

    /// A pod on the node at the address: the calls go to its guest.
    #[arg(long, global = true, env = "FEOS_POD")]
    pub pod: Option<String>,

    /// The container whose root the paths are in.
    #[arg(short, long, global = true, default_value = "main")]
    pub container: String,

    #[command(subcommand)]
    command: ToolCommand,
}

#[derive(Subcommand, Debug)]
pub enum ToolCommand {
    /// Print lines of a file
    Read {
        path: String,
        #[arg(long, default_value_t = 0, help = "First line, 1-based")]
        offset: u32,
        #[arg(long, default_value_t = 0, help = "Number of lines")]
        limit: u32,
    },
    /// Write stdin to a file
    Write { path: String },
    /// Replace text that occurs exactly once in a file
    Edit {
        path: String,
        #[arg(long)]
        old: String,
        #[arg(long)]
        new: String,
    },
    /// List a directory
    Ls {
        #[arg(default_value = "/")]
        path: String,
    },
    /// Search file contents with ripgrep's engine
    Rg {
        pattern: String,
        #[arg(default_value = "/")]
        path: String,
    },
    /// Find files by name with fd's semantics
    Fd {
        pattern: String,
        #[arg(default_value = "/")]
        path: String,
    },
    /// Fetch a web page as readable text
    Webfetch { url: String },
}

pub async fn handle_tool_command(args: ToolArgs) -> Result<()> {
    let mut client = ToolServiceClient::new(
        crate::transport::connect(&args.address, args.pod.as_deref()).await?,
    );
    let container = args.container;
    let error = match args.command {
        ToolCommand::Read {
            path,
            offset,
            limit,
        } => {
            let r = client
                .read(ReadRequest {
                    container,
                    path,
                    offset,
                    limit,
                })
                .await?
                .into_inner();
            if r.media_type.is_empty() {
                print!("{}", r.text);
            } else {
                println!(
                    "{} {}x{}, {} bytes",
                    r.media_type,
                    r.width,
                    r.height,
                    r.image.len()
                );
            }
            r.error
        }
        ToolCommand::Write { path } => {
            let mut content = Vec::new();
            std::io::Read::read_to_end(&mut std::io::stdin(), &mut content)?;
            let request = WriteRequest {
                container,
                path,
                content,
            };
            client.write(request).await?.into_inner().error
        }
        ToolCommand::Edit { path, old, new } => {
            let request = EditRequest {
                container,
                path,
                edits: vec![EditOp { old, new }],
            };
            let r = client.edit(request).await?.into_inner();
            if r.error.is_empty() {
                println!("{} replacement(s)", r.replacements);
            }
            r.error
        }
        ToolCommand::Ls { path } => {
            let r = client
                .ls(LsRequest {
                    container,
                    path,
                    limit: 0,
                })
                .await?
                .into_inner();
            for entry in &r.entries {
                println!("{entry}");
            }
            r.error
        }
        ToolCommand::Rg { pattern, path } => {
            let r = client
                .rg(RgRequest {
                    container,
                    pattern,
                    path,
                    ..Default::default()
                })
                .await?
                .into_inner();
            print!("{}", r.text);
            r.error
        }
        ToolCommand::Fd { pattern, path } => {
            let r = client
                .fd(FdRequest {
                    container,
                    pattern,
                    path,
                    ..Default::default()
                })
                .await?
                .into_inner();
            for entry in &r.entries {
                println!("{entry}");
            }
            r.error
        }
        ToolCommand::Webfetch { url } => {
            let r = client.webfetch(WebfetchRequest { url }).await?.into_inner();
            if !r.title.is_empty() {
                println!("# {}\n", r.title);
            }
            println!("{}", r.text);
            r.error
        }
    };
    if !error.is_empty() {
        bail!(error);
    }
    Ok(())
}
