// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! The node's traffic records (ADR 0013): flows, drops, DNS queries and
//! server names of its pods.

use anyhow::{bail, Result};
use chrono::SecondsFormat;
use clap::{Args, Subcommand};
use feos_proto::network_service::{
    traffic_record::Record, traffic_service_client::TrafficServiceClient, DnsQuery,
    Drop as DropRecord, DropReason, Flow, ServerName, ServerNameSource,
    StreamTrafficRecordsRequest, TrafficRecord, Tuple,
};
use prost_types::Timestamp;
use std::net::IpAddr;

#[derive(Args, Debug)]
pub struct TrafficArgs {
    #[arg(
        short,
        long,
        global = true,
        env = "FEOS_ADDRESS",
        default_value = "unix:/run/feos/api.sock"
    )]
    pub address: String,

    #[command(subcommand)]
    command: TrafficCommand,
}

#[derive(Subcommand, Debug)]
pub enum TrafficCommand {
    /// Print the records as feosd makes them, one per line
    Watch {
        #[arg(long, help = "Only this pod's")]
        pod: Option<String>,
    },
}

pub async fn handle_traffic_command(args: TrafficArgs) -> Result<()> {
    let mut client = TrafficServiceClient::new(crate::transport::channel(&args.address).await?);
    match args.command {
        TrafficCommand::Watch { pod } => {
            let request = StreamTrafficRecordsRequest {
                pod_id: pod.unwrap_or_default(),
            };
            let mut records = client.stream_traffic_records(request).await?.into_inner();
            while let Some(record) = records.message().await? {
                println!("{}", line(&record));
            }
            bail!("the stream ended")
        }
    }
}

/// A record as one line: time, pod, kind, and what it says; then what was
/// missed before it.
fn line(record: &TrafficRecord) -> String {
    let time = record.time.as_ref().map(time).unwrap_or_else(|| "-".into());
    let what = match &record.record {
        Some(Record::Flow(flow)) => flow_line(flow),
        Some(Record::Drop(drop)) => drop_line(drop),
        Some(Record::Dns(dns)) => dns_line(dns),
        Some(Record::ServerName(name)) => name_line(name),
        None => "unknown".to_string(),
    };
    let mut line = format!("{time} {} {what}", record.pod_id);
    if record.missed > 0 {
        line.push_str(&format!(" (missed {} before)", record.missed));
    }
    line
}

fn time(time: &Timestamp) -> String {
    match chrono::DateTime::from_timestamp(time.seconds, time.nanos as u32) {
        Some(time) => time.to_rfc3339_opts(SecondsFormat::Millis, true),
        None => format!("{}.{:09}", time.seconds, time.nanos),
    }
}

fn direction(outbound: bool) -> &'static str {
    if outbound {
        "out"
    } else {
        "in"
    }
}

fn flow_line(flow: &Flow) -> String {
    let tuple = tuple(flow.tuple.as_ref());
    let direction = direction(flow.outbound);
    if !flow.ended {
        return format!("flow {tuple} {direction}");
    }
    let duration = match (&flow.start, &flow.end) {
        (Some(start), Some(end)) => {
            let nanos =
                (end.seconds - start.seconds) as f64 * 1e9 + (end.nanos - start.nanos) as f64;
            format!(" {:.1}s", nanos / 1e9)
        }
        _ => String::new(),
    };
    format!(
        "flow-end {tuple} {direction}{duration} sent {} B/{} pkts received {} B/{} pkts",
        flow.bytes_sent, flow.packets_sent, flow.bytes_received, flow.packets_received
    )
}

fn drop_line(drop: &DropRecord) -> String {
    let reason = match drop.reason() {
        DropReason::NoRule => "no-rule",
        DropReason::DeniedPrefix => "denied-prefix",
        DropReason::Spoofed => "spoofed",
        DropReason::Unspecified => "unspecified",
    };
    format!(
        "drop {} {} {reason}",
        tuple(drop.tuple.as_ref()),
        direction(drop.outbound)
    )
}

fn dns_line(dns: &DnsQuery) -> String {
    let kind = match dns.r#type {
        1 => "A".to_string(),
        5 => "CNAME".to_string(),
        12 => "PTR".to_string(),
        15 => "MX".to_string(),
        16 => "TXT".to_string(),
        28 => "AAAA".to_string(),
        33 => "SRV".to_string(),
        64 => "SVCB".to_string(),
        65 => "HTTPS".to_string(),
        other => format!("TYPE{other}"),
    };
    let rcode = match dns.rcode {
        0 => "NOERROR".to_string(),
        1 => "FORMERR".to_string(),
        2 => "SERVFAIL".to_string(),
        3 => "NXDOMAIN".to_string(),
        5 => "REFUSED".to_string(),
        other => format!("RCODE{other}"),
    };
    let mut line = format!("dns {} {kind} {rcode}", dns.name);
    if !dns.addresses.is_empty() {
        line.push(' ');
        line.push_str(&dns.addresses.join(","));
    }
    line
}

fn name_line(name: &ServerName) -> String {
    let source = match name.source() {
        ServerNameSource::Tls => "tls",
        ServerNameSource::Http => "http",
        ServerNameSource::Unspecified => "unspecified",
    };
    format!("name {source} {} {}", tuple(name.tuple.as_ref()), name.name)
}

/// `tcp [2001:db8::1]:40000 > 192.0.2.1:443`; ICMPv6 with its type and
/// identifier.
fn tuple(tuple: Option<&Tuple>) -> String {
    let Some(t) = tuple else {
        return "-".to_string();
    };
    let protocol = match t.protocol {
        1 => "icmp".to_string(),
        6 => "tcp".to_string(),
        17 => "udp".to_string(),
        58 => "icmpv6".to_string(),
        other => format!("proto{other}"),
    };
    if t.protocol == 1 || t.protocol == 58 {
        return format!(
            "{protocol} {} > {} type {} id {}",
            t.source_address, t.destination_address, t.destination_port, t.source_port
        );
    }
    format!(
        "{protocol} {} > {}",
        endpoint(&t.source_address, t.source_port),
        endpoint(&t.destination_address, t.destination_port)
    )
}

fn endpoint(address: &str, port: u32) -> String {
    match address.parse::<IpAddr>() {
        Ok(IpAddr::V6(_)) => format!("[{address}]:{port}"),
        _ => format!("{address}:{port}"),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn record(record: Record, missed: u64) -> TrafficRecord {
        TrafficRecord {
            time: Some(Timestamp {
                seconds: 1_760_000_000,
                nanos: 123_000_000,
            }),
            pod_id: "pod-a".to_string(),
            missed,
            record: Some(record),
        }
    }

    fn tcp() -> Tuple {
        Tuple {
            protocol: 6,
            source_address: "2001:db8::1".to_string(),
            source_port: 40000,
            destination_address: "192.0.2.1".to_string(),
            destination_port: 443,
        }
    }

    #[test]
    fn records_print_as_lines() {
        let flow = Flow {
            tuple: Some(tcp()),
            outbound: true,
            start: Some(Timestamp {
                seconds: 10,
                nanos: 0,
            }),
            end: Some(Timestamp {
                seconds: 12,
                nanos: 500_000_000,
            }),
            ended: true,
            bytes_sent: 1000,
            bytes_received: 2000,
            packets_sent: 10,
            packets_received: 12,
        };
        assert_eq!(
            line(&record(Record::Flow(flow), 3)),
            "2025-10-09T08:53:20.123Z pod-a flow-end tcp [2001:db8::1]:40000 > 192.0.2.1:443 \
             out 2.5s sent 1000 B/10 pkts received 2000 B/12 pkts (missed 3 before)"
        );
        let name = ServerName {
            tuple: Some(tcp()),
            source: ServerNameSource::Tls.into(),
            name: "example.com".to_string(),
        };
        assert_eq!(
            line(&record(Record::ServerName(name), 0)),
            "2025-10-09T08:53:20.123Z pod-a name tls tcp [2001:db8::1]:40000 > 192.0.2.1:443 example.com"
        );
        let drop = DropRecord {
            tuple: Some(Tuple {
                protocol: 58,
                destination_port: 128,
                source_port: 7,
                ..tcp()
            }),
            outbound: false,
            reason: DropReason::NoRule.into(),
        };
        assert_eq!(
            line(&record(Record::Drop(drop), 0)),
            "2025-10-09T08:53:20.123Z pod-a drop icmpv6 2001:db8::1 > 192.0.2.1 type 128 id 7 in no-rule"
        );
        let dns = DnsQuery {
            name: "example.com".to_string(),
            r#type: 28,
            rcode: 0,
            addresses: vec!["2001:db8::1".to_string()],
        };
        assert_eq!(
            line(&record(Record::Dns(dns), 0)),
            "2025-10-09T08:53:20.123Z pod-a dns example.com AAAA NOERROR 2001:db8::1"
        );
    }
}
