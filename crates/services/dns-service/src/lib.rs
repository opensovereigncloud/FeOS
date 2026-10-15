// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! feosd's resolver for its pods (ADR 0010): DNS64 (RFC 6147) in front of
//! the upstream resolvers that a pod names, or the node's.
//!
//! It answers the pods it knows, told apart by the source address of a
//! query, which the pod's TAP pins, and refuses everyone else. It forwards
//! a query to the pod's upstreams in order, to the next when one does not
//! answer, and answers SERVFAIL when none does. An AAAA query for a name
//! without AAAA records gets the name's A records under the NAT64 prefix.
//! HTTPS records lose `ech`, so that clients send the server name in the
//! clear (ADR 0013), and `ipv4hint`, which pods cannot use. Every query of
//! a pod that is answered becomes a traffic record (ADR 0013).
//!
//! feosd binds the sockets and hands them over, so that they can come from
//! socket activation and outlive feosd.

mod dns64;
mod svcb;
#[cfg(test)]
mod tests;
mod upstream;

pub use dns64::WELL_KNOWN_PREFIX;

use feos_proto::network_service::{traffic_record, DnsQuery, TrafficRecord};
use hickory_proto::op::{Edns, Message, MessageType, OpCode, Query, ResponseCode};
use hickory_proto::rr::rdata::svcb::SvcParamKey;
use hickory_proto::rr::rdata::{A, AAAA};
use hickory_proto::rr::{DNSClass, Name, RData, RecordType};
use log::{debug, warn};
use std::collections::HashMap;
use std::io;
use std::net::{IpAddr, Ipv6Addr, SocketAddr};
use std::sync::{Arc, PoisonError, RwLock};
use std::time::{Duration, SystemTime};
use tokio::io::{AsyncRead, AsyncReadExt, AsyncWrite, AsyncWriteExt};
use tokio::net::{TcpListener, TcpStream, UdpSocket};
use tokio::sync::{broadcast, Mutex, OwnedSemaphorePermit, Semaphore};
use tokio::task::JoinSet;
use tokio::time;

/// The resolver's address on every node (ADR 0010).
pub const ADDRESS: Ipv6Addr = Ipv6Addr::new(0xfdfd, 0x07b5, 0x61d9, 0, 0, 0, 0, 0x53);
/// How long an upstream has to answer over UDP, by default.
pub const UPSTREAM_TIMEOUT: Duration = Duration::from_secs(2);

/// The largest UDP message the resolver sends, and asks upstreams for
/// (DNS flag day 2020).
const UDP_PAYLOAD: u16 = 1232;
const HEADER_LENGTH: usize = 12;
/// The largest query read from UDP.
const QUERY_BUFFER: usize = 4096;
/// Queries in flight, of all pods and of one.
const MAX_QUERIES: usize = 512;
const MAX_CLIENT_QUERIES: usize = 64;
/// TCP connections, of all pods and of one.
const MAX_CONNECTIONS: usize = 128;
const MAX_CLIENT_CONNECTIONS: usize = 8;
/// Queries in flight on one TCP connection.
const MAX_CONNECTION_QUERIES: usize = 16;
/// How long a TCP connection may take to send a query, and to take an
/// answer.
const TCP_TIMEOUT: Duration = Duration::from_secs(10);
/// Records kept for subscribers that fall behind.
const RECORDS: usize = 1024;

/// What the resolver does for all pods.
#[derive(Clone, Debug)]
pub struct Config {
    /// The NAT64 prefix, a /96 (ADR 0010); its last 32 bits are ignored.
    pub nat64_prefix: Ipv6Addr,
    /// AAAA records count as absent for every name (RFC 6147 5.1.4,
    /// exclusion), and HTTPS records lose `ipv6hint`: for nodes whose pods
    /// cannot reach the IPv6 internet, such as those of a ULA pod prefix.
    pub exclude_all_aaaa: bool,
    /// The upstreams of pods that name none.
    pub default_upstreams: Vec<SocketAddr>,
    /// How long an upstream has to answer over UDP before the next is
    /// asked; over TCP, twice that.
    pub upstream_timeout: Duration,
}

impl Default for Config {
    fn default() -> Self {
        Self {
            nat64_prefix: WELL_KNOWN_PREFIX,
            exclude_all_aaaa: false,
            default_upstreams: Vec::new(),
            upstream_timeout: UPSTREAM_TIMEOUT,
        }
    }
}

/// The resolver. It serves the sockets it is given until the future of
/// `serve` is dropped.
pub struct Resolver {
    config: Config,
    clients: RwLock<HashMap<Ipv6Addr, Arc<Client>>>,
    records: broadcast::Sender<TrafficRecord>,
    queries: Arc<Semaphore>,
    connections: Arc<Semaphore>,
}

/// A pod, as the resolver knows it.
struct Client {
    pod_id: String,
    /// Empty: the default upstreams.
    upstreams: Vec<SocketAddr>,
    queries: Arc<Semaphore>,
    connections: Arc<Semaphore>,
}

#[derive(Clone, Copy, Debug, PartialEq)]
enum Transport {
    Udp,
    Tcp,
}

/// What a query of a pod holds while it is in flight.
type Permits = (OwnedSemaphorePermit, Option<OwnedSemaphorePermit>);

/// What the upstreams answered, changed as the resolver changes it.
struct Resolution {
    response: Message,
    /// Whether DNS64 made the answer.
    synthesized: bool,
}

impl Resolution {
    fn new(response: Message) -> Self {
        Self {
            response,
            synthesized: false,
        }
    }
}

impl Resolver {
    pub fn new(mut config: Config) -> Arc<Self> {
        let mut prefix = config.nat64_prefix.octets();
        prefix[12..].fill(0);
        config.nat64_prefix = prefix.into();
        let (records, _) = broadcast::channel(RECORDS);
        Arc::new(Self {
            config,
            clients: RwLock::default(),
            records,
            queries: Arc::new(Semaphore::new(MAX_QUERIES)),
            connections: Arc::new(Semaphore::new(MAX_CONNECTIONS)),
        })
    }

    /// Serves the pod `pod_id` at `address`, asking `upstreams`, or the
    /// default upstreams when there are none. Replaces what the resolver
    /// knew of `address`.
    pub fn set_client(&self, address: Ipv6Addr, pod_id: &str, upstreams: Vec<SocketAddr>) {
        let mut clients = self.clients.write().unwrap_or_else(PoisonError::into_inner);
        // A pod that is set again keeps its limits.
        let (queries, connections) = match clients.get(&address) {
            Some(client) => (client.queries.clone(), client.connections.clone()),
            None => (
                Arc::new(Semaphore::new(MAX_CLIENT_QUERIES)),
                Arc::new(Semaphore::new(MAX_CLIENT_CONNECTIONS)),
            ),
        };
        let client = Client {
            pod_id: pod_id.to_string(),
            upstreams,
            queries,
            connections,
        };
        clients.insert(address, Arc::new(client));
    }

    /// Stops serving the pod at `address`: its queries are refused from
    /// now on.
    pub fn remove_client(&self, address: Ipv6Addr) {
        self.clients
            .write()
            .unwrap_or_else(PoisonError::into_inner)
            .remove(&address);
    }

    /// The pods' queries from now on, one record for each that is
    /// answered. A receiver that falls behind misses records, and learns
    /// how many.
    pub fn subscribe(&self) -> broadcast::Receiver<TrafficRecord> {
        self.records.subscribe()
    }

    /// Serves queries on `udp` and `tcp`, both bound already, until the
    /// future is dropped. Returns only on an error the sockets do not
    /// recover from.
    pub async fn serve(self: Arc<Self>, udp: UdpSocket, tcp: TcpListener) -> io::Result<()> {
        tokio::try_join!(self.clone().serve_udp(udp), self.serve_tcp(tcp))?;
        Ok(())
    }

    async fn serve_udp(self: Arc<Self>, socket: UdpSocket) -> io::Result<()> {
        let socket = Arc::new(socket);
        let mut queries = JoinSet::new();
        let mut buffer = vec![0; QUERY_BUFFER];
        loop {
            while queries.try_join_next().is_some() {}
            let (length, peer) = match socket.recv_from(&mut buffer).await {
                Ok(received) => received,
                Err(error) => {
                    pause(error).await?;
                    continue;
                }
            };
            let client = self.client(source_address(peer));
            // Over its limits, a query is dropped; the client asks again.
            let Some(permits) = self.try_permits(client.as_deref()) else {
                debug!("dns: too many queries in flight, dropped one from {peer}");
                continue;
            };
            let query = buffer[..length].to_vec();
            let (resolver, socket) = (self.clone(), socket.clone());
            queries.spawn(async move {
                let _permits = permits;
                let answer = resolver
                    .answer(&query, client.as_deref(), Transport::Udp)
                    .await;
                if let Some(answer) = answer
                    && let Err(error) = socket.send_to(&answer, peer).await
                {
                    debug!("dns: answering {peer}: {error}");
                }
            });
        }
    }

    async fn serve_tcp(self: Arc<Self>, listener: TcpListener) -> io::Result<()> {
        let mut connections = JoinSet::new();
        loop {
            while connections.try_join_next().is_some() {}
            let (stream, peer) = match listener.accept().await {
                Ok(accepted) => accepted,
                Err(error) => {
                    pause(error).await?;
                    continue;
                }
            };
            let source = source_address(peer);
            let Some(permits) = self.try_connection(source) else {
                debug!("dns: too many connections, closed one from {peer}");
                continue;
            };
            connections.spawn(self.clone().serve_connection(stream, source, permits));
        }
    }

    /// Answers the queries on a TCP connection, several at a time and in
    /// the order their answers come (RFC 7766), until the client closes it
    /// or stays idle.
    async fn serve_connection(
        self: Arc<Self>,
        stream: TcpStream,
        source: Ipv6Addr,
        _permits: Permits,
    ) {
        let (mut reader, writer) = stream.into_split();
        // None once an answer could not be written: the others are not.
        let writer = Arc::new(Mutex::new(Some(writer)));
        let mut queries = JoinSet::new();
        loop {
            while queries.try_join_next().is_some() {}
            while queries.len() >= MAX_CONNECTION_QUERIES {
                queries.join_next().await;
            }
            let query = match time::timeout(TCP_TIMEOUT, read_message(&mut reader)).await {
                Ok(Ok(Some(query))) => query,
                // Closed, broken, or idle.
                _ => break,
            };
            // Asked for each query: the pod may have gone meanwhile.
            let client = self.client(source);
            let Some(permits) = self.permits(client.as_deref()).await else {
                break;
            };
            let (resolver, writer) = (self.clone(), writer.clone());
            queries.spawn(async move {
                let _permits = permits;
                let Some(answer) = resolver
                    .answer(&query, client.as_deref(), Transport::Tcp)
                    .await
                else {
                    return;
                };
                let mut writer = writer.lock().await;
                let Some(stream) = writer.as_mut() else {
                    return;
                };
                let error = match time::timeout(TCP_TIMEOUT, write_message(stream, &answer)).await {
                    Ok(Ok(())) => return,
                    Ok(Err(error)) => error,
                    Err(_) => io::Error::from(io::ErrorKind::TimedOut),
                };
                debug!("dns: answering {source}: {error}");
                *writer = None;
            });
        }
        while queries.join_next().await.is_some() {}
    }

    fn client(&self, address: Ipv6Addr) -> Option<Arc<Client>> {
        self.clients
            .read()
            .unwrap_or_else(PoisonError::into_inner)
            .get(&address)
            .cloned()
    }

    /// Permits for a UDP query, None when the client or all are at their
    /// limit.
    fn try_permits(&self, client: Option<&Client>) -> Option<Permits> {
        let own = match client {
            Some(client) => Some(client.queries.clone().try_acquire_owned().ok()?),
            None => None,
        };
        Some((self.queries.clone().try_acquire_owned().ok()?, own))
    }

    /// Permits for a query over TCP, which waits for them.
    async fn permits(&self, client: Option<&Client>) -> Option<Permits> {
        let own = match client {
            Some(client) => Some(client.queries.clone().acquire_owned().await.ok()?),
            None => None,
        };
        Some((self.queries.clone().acquire_owned().await.ok()?, own))
    }

    /// Permits for a TCP connection, None when the client or all are at
    /// their limit.
    fn try_connection(&self, source: Ipv6Addr) -> Option<Permits> {
        let own = match self.client(source) {
            Some(client) => Some(client.connections.clone().try_acquire_owned().ok()?),
            None => None,
        };
        Some((self.connections.clone().try_acquire_owned().ok()?, own))
    }

    /// The answer to the query `bytes` from `client`, unknown if None, on
    /// the wire. None for what is no query.
    async fn answer(
        &self,
        bytes: &[u8],
        client: Option<&Client>,
        transport: Transport,
    ) -> Option<Vec<u8>> {
        // Runts and responses get nothing.
        if bytes.len() < HEADER_LENGTH || bytes[2] & 0x80 != 0 {
            return None;
        }
        let Ok(request) = Message::from_vec(bytes) else {
            let code = match client {
                Some(_) => ResponseCode::FormErr,
                None => ResponseCode::Refused,
            };
            return Some(header_only(bytes, code));
        };
        let limit = match transport {
            Transport::Udp => usize::from(request.max_payload().clamp(512, UDP_PAYLOAD)),
            Transport::Tcp => usize::from(u16::MAX),
        };
        let Some(client) = client else {
            return encode(&respond(&request, Err(ResponseCode::Refused)), limit);
        };
        let (response, synthesized) = match check(&request) {
            Some(code) => (respond(&request, Err(code)), false),
            None => match self.resolve(&request, client, transport).await {
                Some(Resolution {
                    mut response,
                    synthesized,
                }) => {
                    let keys = self.stripped_keys();
                    let answers = svcb::strip(response.answers_mut(), keys);
                    let additionals = svcb::strip(response.additionals_mut(), keys);
                    // What was changed is no longer what was signed.
                    if answers || additionals {
                        response.set_authentic_data(false);
                    }
                    (respond(&request, Ok(response)), synthesized)
                }
                None => (respond(&request, Err(ResponseCode::ServFail)), false),
            },
        };
        if let [question] = request.queries() {
            self.record(client, question, &response, synthesized);
        }
        encode(&response, limit)
    }

    /// What the upstreams answer for the one question of `request`.
    /// None when none answers.
    async fn resolve(
        &self,
        request: &Message,
        client: &Client,
        transport: Transport,
    ) -> Option<Resolution> {
        let question = request.queries().first()?.clone();
        let prefix = self.config.nat64_prefix;
        if question.query_class() == DNSClass::IN {
            if dns64::is_ipv4only_arpa(question.name()) {
                return Some(Resolution {
                    response: dns64::ipv4only(&question, prefix),
                    synthesized: question.query_type() == RecordType::AAAA,
                });
            }
            // A validating client asked for the records as they are
            // (RFC 6147 5.5).
            let raw = dnssec_ok(request) && request.checking_disabled();
            if !raw && question.query_type() == RecordType::AAAA {
                return self
                    .resolve_aaaa(request, client, question, transport)
                    .await;
            }
            if !raw
                && question.query_type() == RecordType::PTR
                && let Some(name) = dns64::reverse(question.name(), prefix)
            {
                return self
                    .resolve_ptr(request, client, question, name, transport)
                    .await;
            }
        }
        let response = self.ask(request, client, question, transport).await?;
        Some(Resolution::new(response))
    }

    /// AAAA through DNS64: the name's AAAA records, or, when it has none,
    /// its A records under the NAT64 prefix (RFC 6147 5.1).
    async fn resolve_aaaa(
        &self,
        request: &Message,
        client: &Client,
        question: Query,
        transport: Transport,
    ) -> Option<Resolution> {
        let aaaa = if self.config.exclude_all_aaaa {
            None
        } else {
            let mut response = self
                .ask(request, client, question.clone(), transport)
                .await?;
            // Errors and names that do not exist pass as they are.
            if response.response_code() != ResponseCode::NoError {
                return Some(Resolution::new(response));
            }
            dns64::exclude(&mut response);
            if dns64::has_aaaa(&response) {
                return Some(Resolution::new(response));
            }
            Some(response)
        };
        let mut a_question = question;
        a_question.set_query_type(RecordType::A);
        let a = self.ask(request, client, a_question, transport).await;
        if let Some(a) = &a
            && a.response_code() == ResponseCode::NoError
        {
            let ttl = dns64::negative_ttl(aaaa.as_ref());
            if let Some(response) = dns64::synthesize(a, self.config.nat64_prefix, ttl) {
                return Some(Resolution {
                    response,
                    synthesized: true,
                });
            }
        }
        match (aaaa, a) {
            (Some(aaaa), _) => Some(Resolution::new(aaaa)),
            (None, Some(a)) => Some(Resolution::new(dns64::without_a(a))),
            (None, None) => None,
        }
    }

    /// PTR for an address under the NAT64 prefix: the PTR records of the
    /// IPv4 address in it, `name` (RFC 6147 5.3.1).
    async fn resolve_ptr(
        &self,
        request: &Message,
        client: &Client,
        question: Query,
        name: Name,
        transport: Transport,
    ) -> Option<Resolution> {
        let mut ipv4 = question.clone();
        ipv4.set_name(name.clone());
        let mut response = self.ask(request, client, ipv4, transport).await?;
        dns64::rename(&mut response, &name, question.name());
        let synthesized = !response.answers().is_empty();
        Some(Resolution {
            response,
            synthesized,
        })
    }

    /// Asks the client's upstreams `question` on behalf of `request`.
    async fn ask(
        &self,
        request: &Message,
        client: &Client,
        question: Query,
        transport: Transport,
    ) -> Option<Message> {
        let upstreams = match client.upstreams.is_empty() {
            true => &self.config.default_upstreams,
            false => &client.upstreams,
        };
        let query = upstream::query(request, question);
        let tcp = transport == Transport::Tcp;
        upstream::ask(upstreams, &query, tcp, self.config.upstream_timeout).await
    }

    /// The parameters that SVCB and HTTPS records lose.
    fn stripped_keys(&self) -> &'static [SvcParamKey] {
        match self.config.exclude_all_aaaa {
            true => &[
                SvcParamKey::Ipv4Hint,
                SvcParamKey::EchConfigList,
                SvcParamKey::Ipv6Hint,
            ],
            false => &[SvcParamKey::Ipv4Hint, SvcParamKey::EchConfigList],
        }
    }

    /// Publishes the record of an answered query.
    fn record(&self, client: &Client, question: &Query, response: &Message, synthesized: bool) {
        let addresses = response
            .answers()
            .iter()
            .filter_map(|record| match record.data() {
                RData::A(A(address)) => Some(address.to_string()),
                RData::AAAA(AAAA(address)) => Some(address.to_string()),
                _ => None,
            })
            .collect();
        let record = TrafficRecord {
            time: Some(SystemTime::now().into()),
            pod_id: client.pod_id.clone(),
            missed: 0,
            record: Some(traffic_record::Record::Dns(DnsQuery {
                name: record_name(question.name()),
                r#type: u16::from(question.query_type()).into(),
                rcode: u16::from(response.response_code()).into(),
                addresses,
                synthesized,
            })),
        };
        // Nobody may be listening.
        let _ = self.records.send(record);
    }
}

/// The error a request gets before it is resolved, if any.
fn check(request: &Message) -> Option<ResponseCode> {
    if request.op_code() != OpCode::Query {
        return Some(ResponseCode::NotImp);
    }
    let [question] = request.queries() else {
        return Some(ResponseCode::FormErr);
    };
    if request
        .extensions()
        .as_ref()
        .is_some_and(|edns| edns.version() != 0)
    {
        return Some(ResponseCode::BADVERS);
    }
    // Zone transfers are not for pods.
    if matches!(question.query_type(), RecordType::AXFR | RecordType::IXFR) {
        return Some(ResponseCode::Refused);
    }
    None
}

/// The response to `request`: what the upstreams answered, or an error.
fn respond(request: &Message, upstream: Result<Message, ResponseCode>) -> Message {
    let mut response = Message::new();
    response
        .set_id(request.id())
        .set_message_type(MessageType::Response)
        .set_op_code(request.op_code())
        .set_recursion_desired(request.recursion_desired())
        .set_recursion_available(true)
        .set_checking_disabled(request.checking_disabled());
    response.add_queries(request.queries().iter().cloned());
    match upstream {
        Ok(mut upstream) => {
            response
                .set_response_code(upstream.response_code())
                .set_authentic_data(upstream.authentic_data());
            *response.answers_mut() = upstream.take_answers();
            *response.name_servers_mut() = upstream.take_name_servers();
            *response.additionals_mut() = upstream.take_additionals();
        }
        Err(code) => {
            response.set_response_code(code);
        }
    }
    // EDNS of our own, if the client spoke it; the upstream's options were
    // for us.
    match request.extensions() {
        Some(edns) => {
            let mut ours = Edns::new();
            ours.set_max_payload(UDP_PAYLOAD)
                .set_dnssec_ok(edns.flags().dnssec_ok);
            response.set_edns(ours);
        }
        // A code that needs EDNS cannot be told.
        None if response.response_code().high() != 0 => {
            response.set_response_code(ResponseCode::ServFail);
        }
        None => {}
    }
    response
}

/// `response` on the wire: whole if it fits in `limit` bytes, else
/// truncated, for the client to ask again over TCP.
fn encode(response: &Message, limit: usize) -> Option<Vec<u8>> {
    match response.to_vec() {
        Ok(bytes) if bytes.len() <= limit => Some(bytes),
        Ok(_) => response.truncate().to_vec().ok(),
        Err(error) => {
            debug!("dns: encoding an answer: {error}");
            let mut failure = response.truncate();
            failure
                .set_truncated(false)
                .set_response_code(ResponseCode::ServFail);
            failure.to_vec().ok()
        }
    }
}

/// A response of a header alone, to a query that cannot be read.
fn header_only(query: &[u8], code: ResponseCode) -> Vec<u8> {
    let mut header = vec![0; HEADER_LENGTH];
    header[..2].copy_from_slice(&query[..2]);
    // QR, and the query's opcode and RD.
    header[2] = 0x80 | (query[2] & 0x79);
    // RA, and the code.
    header[3] = 0x80 | code.low();
    header
}

/// Whether the client of `request` wants DNSSEC records (RFC 3225).
fn dnssec_ok(request: &Message) -> bool {
    request
        .extensions()
        .as_ref()
        .is_some_and(|edns| edns.flags().dnssec_ok)
}

/// A name as records have it: in lower case, without the final dot.
fn record_name(name: &Name) -> String {
    let name = name.to_lowercase().to_ascii();
    match name.strip_suffix('.') {
        Some(relative) if !relative.is_empty() => relative.to_string(),
        _ => name,
    }
}

/// The address a peer is known by; an IPv4 peer by its mapped address.
fn source_address(peer: SocketAddr) -> Ipv6Addr {
    match peer.ip() {
        IpAddr::V6(address) => address,
        IpAddr::V4(address) => address.to_ipv6_mapped(),
    }
}

/// Waits a moment after a socket error that may pass, such as running out
/// of file descriptors, or skips one of a single connection. Returns the
/// errors of a socket that is broken.
async fn pause(error: io::Error) -> io::Result<()> {
    match error.raw_os_error() {
        Some(libc::EBADF | libc::ENOTSOCK | libc::EINVAL | libc::EOPNOTSUPP) => Err(error),
        Some(libc::ECONNABORTED | libc::ECONNRESET | libc::EPROTO | libc::EPERM) => {
            debug!("dns: {error}");
            Ok(())
        }
        _ => {
            warn!("dns: {error}");
            time::sleep(Duration::from_millis(100)).await;
            Ok(())
        }
    }
}

/// Reads a message of DNS over TCP, after its length (RFC 1035 4.2.2).
/// None when the stream ends before one.
async fn read_message<R: AsyncRead + Unpin>(reader: &mut R) -> io::Result<Option<Vec<u8>>> {
    let mut length = [0; 2];
    match reader.read_exact(&mut length).await {
        Ok(_) => {}
        Err(error) if error.kind() == io::ErrorKind::UnexpectedEof => return Ok(None),
        Err(error) => return Err(error),
    }
    let length = usize::from(u16::from_be_bytes(length));
    if length == 0 {
        return Err(io::Error::new(
            io::ErrorKind::InvalidData,
            "an empty message",
        ));
    }
    let mut message = vec![0; length];
    reader.read_exact(&mut message).await?;
    Ok(Some(message))
}

/// Writes a message of DNS over TCP, after its length.
async fn write_message<W: AsyncWrite + Unpin>(writer: &mut W, message: &[u8]) -> io::Result<()> {
    let length = u16::try_from(message.len())
        .map_err(|_| io::Error::new(io::ErrorKind::InvalidData, "a message over 64 KiB"))?;
    let mut framed = Vec::with_capacity(2 + message.len());
    framed.extend_from_slice(&length.to_be_bytes());
    framed.extend_from_slice(message);
    writer.write_all(&framed).await
}
