// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

use chrono::{DateTime, Local, TimeZone};
use log::{error, info, warn};
use sntpc::{NtpContext, StdTimestampGen};
use std::net::{IpAddr, Ipv4Addr, Ipv6Addr, SocketAddr};
use std::sync::Mutex;
use std::time::Duration;
use tokio::net::UdpSocket;
use tokio::time::{sleep, timeout};

const FALLBACK_NTP_SERVER: &str = "pool.ntp.org";
const SYNC_INTERVAL: Duration = Duration::from_secs(86400); // 24 Hours
const RETRY_INTERVAL: Duration = Duration::from_secs(300); // 5 Minutes
const NTP_PORT: u16 = 123;
/// How long a server has to answer.
const QUERY_TIMEOUT: Duration = Duration::from_secs(5);

/// The NTP servers of the DHCPv4 lease, which comes and changes on its own
/// schedule: each sync takes those it names then.
static DHCPV4_SERVERS: Mutex<Vec<Ipv4Addr>> = Mutex::new(Vec::new());

/// The NTP servers DHCPv4 named.
pub fn set_ipv4_servers(servers: &[Ipv4Addr]) {
    *DHCPV4_SERVERS.lock().unwrap_or_else(|e| e.into_inner()) = servers.to_vec();
}

pub struct TimeSyncWorker {
    /// Those DHCPv6 named.
    ntp_servers: Vec<Ipv6Addr>,
}

impl TimeSyncWorker {
    pub fn new(ntp_servers: Vec<Ipv6Addr>) -> Self {
        Self { ntp_servers }
    }

    pub async fn run(self) {
        info!("TimeSyncWorker: Started.");

        // Initial sync
        self.perform_sync_loop().await;
    }

    async fn perform_sync_loop(&self) {
        loop {
            match self.synchronize_time().await {
                Ok(_) => {
                    info!(
                        "TimeSyncWorker: Time synchronization successful. Sleeping for 24 hours."
                    );
                    sleep(SYNC_INTERVAL).await;
                }
                Err(e) => {
                    error!(
                        "TimeSyncWorker: Time synchronization failed: {e}. Retrying in 5 minutes."
                    );
                    sleep(RETRY_INTERVAL).await;
                }
            }
        }
    }

    async fn synchronize_time(&self) -> Result<(), String> {
        let dhcpv4 = DHCPV4_SERVERS
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .clone();
        let servers = self
            .ntp_servers
            .iter()
            .map(|ip| (IpAddr::V6(*ip), "DHCPv6"))
            .chain(dhcpv4.into_iter().map(|ip| (IpAddr::V4(ip), "DHCPv4")));
        for (server_ip, source) in servers {
            info!("TimeSyncWorker: Attempting sync with {source} server: {server_ip}");
            let target = SocketAddr::from((server_ip, NTP_PORT));

            match self.query_ntp_server(target).await {
                Ok(_) => return Ok(()),
                Err(e) => {
                    warn!("TimeSyncWorker: Failed to sync with {server_ip}: {e}");
                }
            }
        }

        info!("TimeSyncWorker: Attempting sync with fallback server: {FALLBACK_NTP_SERVER}");

        match self.resolve_and_sync(FALLBACK_NTP_SERVER).await {
            Ok(_) => Ok(()),
            Err(e) => Err(format!("Failed to sync with fallback server: {e}")),
        }
    }

    async fn resolve_and_sync(&self, hostname: &str) -> Result<(), String> {
        use tokio::net::lookup_host;

        let server_with_port = format!("{hostname}:{NTP_PORT}");
        let mut addrs: Vec<SocketAddr> = lookup_host(&server_with_port)
            .await
            .map_err(|e| format!("Failed to resolve {hostname}: {e}"))?
            .collect();
        // IPv6 first; a node without it, or without a route, takes IPv4.
        addrs.sort_by_key(SocketAddr::is_ipv4);
        info!("TimeSyncWorker: Resolved {hostname} to {addrs:?}");

        let mut failure = format!("No address found for {hostname}");
        for target in addrs {
            match self.query_ntp_server(target).await {
                Ok(()) => return Ok(()),
                Err(e) => {
                    warn!("TimeSyncWorker: Failed to sync with {target}: {e}");
                    failure = e;
                }
            }
        }
        Err(failure)
    }

    async fn query_ntp_server(&self, target: SocketAddr) -> Result<(), String> {
        // Of the server's family: sntpc wants the answer from the address it
        // asked, which a socket of both families would give as IPv6.
        let local = match target {
            SocketAddr::V4(_) => SocketAddr::from((Ipv4Addr::UNSPECIFIED, 0)),
            SocketAddr::V6(_) => SocketAddr::from((Ipv6Addr::UNSPECIFIED, 0)),
        };
        let socket = UdpSocket::bind(local)
            .await
            .map_err(|e| format!("Failed to bind UDP socket: {e}"))?;
        let context = NtpContext::new(StdTimestampGen::default());

        let result = timeout(QUERY_TIMEOUT, sntpc::get_time(target, &socket, context))
            .await
            .map_err(|_| format!("No answer within {}s", QUERY_TIMEOUT.as_secs()))?
            .map_err(|e| format!("NTP query failed: {e:?}"))?;

        let seconds = result.sec();
        let sec_fraction = result.sec_fraction();

        let nanoseconds = sntpc::fraction_to_nanoseconds(sec_fraction);
        let server_time = Local
            .timestamp_opt(seconds as i64, nanoseconds)
            .single()
            .ok_or_else(|| "Failed to convert NTP time to local time".to_string())?;

        let offset_sec = result.offset() as f64 / 1_000_000.0;
        let delay_sec = result.roundtrip() as f64 / 1_000_000.0;

        info!(
            "TimeSyncWorker: {} ({:+.6}s offset) +/- {:.6}s delay, server: {} stratum: {}",
            server_time.format("%Y-%m-%d %H:%M:%S%.6f"),
            offset_sec,
            delay_sec,
            target.ip(),
            result.stratum()
        );

        self.set_system_time(server_time)
            .map_err(|e| format!("Failed to set system time: {e}"))?;

        Ok(())
    }

    fn set_system_time(&self, dt: DateTime<Local>) -> Result<(), std::io::Error> {
        use libc::{clock_settime, timespec, CLOCK_REALTIME};

        let ts = timespec {
            tv_sec: dt.timestamp(),
            tv_nsec: dt.timestamp_subsec_nanos() as _,
        };

        let ret = unsafe { clock_settime(CLOCK_REALTIME, &ts) };

        if ret != 0 {
            return Err(std::io::Error::last_os_error());
        }

        info!("TimeSyncWorker: System clock updated to: {dt}");
        Ok(())
    }
}
