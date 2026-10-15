// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! The server a client asks for in the first bytes of a TCP connection:
//! the server name indication of a TLS ClientHello (RFC 8446, RFC 6066),
//! or the Host of a plain HTTP request.
//!
//! Both are read from a prefix of the stream, as far as it has arrived.
//! A ClientHello may span several TLS records, and those several TCP
//! segments; its extensions come in any order. The answer is "more" until
//! the name is found or cannot come any more.

/// What the first bytes of a connection tell.
#[derive(Debug, PartialEq)]
pub enum Found {
    Name(Source, String),
    /// Not yet: the bytes so far are a prefix of a request that may name
    /// a server.
    More,
    /// Neither TLS nor HTTP, or one without a name.
    Nothing,
}

#[derive(Clone, Copy, Debug, PartialEq)]
pub enum Source {
    Tls,
    Http,
}

use std::net::Ipv6Addr;

const RECORD_HANDSHAKE: u8 = 22;
const HANDSHAKE_CLIENT_HELLO: u8 = 1;
const EXTENSION_SERVER_NAME: u16 = 0;
const NAME_TYPE_HOST_NAME: u8 = 0;
/// A TLS record's plaintext is at most 2^14 bytes; some slack for the
/// clients that get it wrong.
const MAX_RECORD: usize = (1 << 14) + 256;
/// The longest method of a request line we take for HTTP.
const MAX_METHOD: usize = 16;

pub fn find(data: &[u8]) -> Found {
    match data.first() {
        None => Found::More,
        Some(&RECORD_HANDSHAKE) => tls(data),
        Some(b'A'..=b'Z') => http(data),
        Some(_) => Found::Nothing,
    }
}

/// The ClientHello's server name: the handshake message is gathered from
/// the records that have arrived, and read as far as it goes.
fn tls(data: &[u8]) -> Found {
    let mut handshake = Vec::new();
    let mut rest = data;
    loop {
        if rest.len() >= 2 && (rest[0] != RECORD_HANDSHAKE || rest[1] != 3) {
            return Found::Nothing;
        }
        let Some(header) = rest.get(..5) else {
            break;
        };
        let len = u16::from_be_bytes([header[3], header[4]]) as usize;
        if len == 0 || len > MAX_RECORD {
            return Found::Nothing;
        }
        let body = &rest[5..];
        let take = len.min(body.len());
        handshake.extend_from_slice(&body[..take]);
        if take < len || message_len(&handshake).is_some_and(|n| handshake.len() >= n) {
            break;
        }
        rest = &body[len..];
    }
    match handshake.first() {
        None => return Found::More,
        Some(&HANDSHAKE_CLIENT_HELLO) => {}
        Some(_) => return Found::Nothing,
    }
    let Some(len) = message_len(&handshake) else {
        return Found::More;
    };
    let complete = handshake.len() >= len;
    match server_name(&handshake[4..len.min(handshake.len())], complete) {
        Some(Some(name)) => match valid(name) {
            Some(name) => Found::Name(Source::Tls, name),
            None => Found::Nothing,
        },
        Some(None) => Found::Nothing,
        None if complete => Found::Nothing,
        None => Found::More,
    }
}

/// The length of a handshake message with its header, once the header
/// is there.
fn message_len(handshake: &[u8]) -> Option<usize> {
    let header = handshake.get(..4)?;
    Some(4 + u32::from_be_bytes([0, header[1], header[2], header[3]]) as usize)
}

/// The host name in a ClientHello's body: Some(None) when it has none,
/// None when the body ends before that is known.
fn server_name(body: &[u8], complete: bool) -> Option<Option<&[u8]>> {
    let mut r = Reader(body);
    r.take(2 + 32)?; // legacy_version, random
    r.vector8()?; // legacy_session_id
    r.vector16()?; // cipher_suites
    r.vector8()?; // legacy_compression_methods
    if r.0.is_empty() {
        // A hello without extensions, which only TLS 1.2 and before allow.
        return complete.then_some(None);
    }
    let len = r.u16()? as usize;
    let mut extensions = Reader(&r.0[..len.min(r.0.len())]);
    let truncated = r.0.len() < len;
    while !extensions.0.is_empty() {
        let kind = extensions.u16()?;
        let data = extensions.vector16()?;
        if kind == EXTENSION_SERVER_NAME {
            return Some(host_name(data));
        }
    }
    if truncated {
        None
    } else {
        Some(None)
    }
}

/// The host name of a server_name extension, if it has a well-formed one.
fn host_name(extension: &[u8]) -> Option<&[u8]> {
    let mut names = Reader(Reader(extension).vector16()?);
    while !names.0.is_empty() {
        let name_type = names.u8()?;
        let name = names.vector16()?;
        if name_type == NAME_TYPE_HOST_NAME {
            return Some(name);
        }
    }
    None
}

struct Reader<'a>(&'a [u8]);

impl<'a> Reader<'a> {
    fn take(&mut self, n: usize) -> Option<&'a [u8]> {
        if self.0.len() < n {
            return None;
        }
        let (head, rest) = self.0.split_at(n);
        self.0 = rest;
        Some(head)
    }

    fn u8(&mut self) -> Option<u8> {
        Some(self.take(1)?[0])
    }

    fn u16(&mut self) -> Option<u16> {
        let b = self.take(2)?;
        Some(u16::from_be_bytes([b[0], b[1]]))
    }

    fn vector8(&mut self) -> Option<&'a [u8]> {
        let len = self.u8()? as usize;
        self.take(len)
    }

    fn vector16(&mut self) -> Option<&'a [u8]> {
        let len = self.u16()? as usize;
        self.take(len)
    }
}

/// The Host of an HTTP/1 request, as soon as its header line is in; the
/// host of an absolute request target (a proxy's) when there is none.
fn http(data: &[u8]) -> Found {
    let mut lines = Lines(data);
    let Some(request_line) = lines.next() else {
        // Until the line is in, its method must look like one.
        let method = data.split(|&b| b == b' ').next().unwrap_or_default();
        return match method.len() <= MAX_METHOD && method.iter().all(u8::is_ascii_uppercase) {
            true => Found::More,
            false => Found::Nothing,
        };
    };
    let mut parts = request_line.split(|&b| b == b' ');
    let (Some(method), Some(target), Some(version), None) =
        (parts.next(), parts.next(), parts.next(), parts.next())
    else {
        return Found::Nothing;
    };
    if method.is_empty()
        || method.len() > MAX_METHOD
        || !method.iter().all(u8::is_ascii_uppercase)
        || target.is_empty()
        || !version.starts_with(b"HTTP/1.")
    {
        return Found::Nothing;
    }
    for line in lines.by_ref() {
        if line.is_empty() {
            // The end of the headers, and no Host.
            return match absolute_host(target).and_then(host) {
                Some(name) => Found::Name(Source::Http, name),
                None => Found::Nothing,
            };
        }
        let Some((name, value)) = line.split_at_checked(5) else {
            continue;
        };
        if name.eq_ignore_ascii_case(b"host:") {
            return match host(value) {
                Some(name) => Found::Name(Source::Http, name),
                None => Found::Nothing,
            };
        }
    }
    Found::More
}

/// Lines that end in LF, without the LF and a CR before it; a last line
/// without its LF is not one yet.
struct Lines<'a>(&'a [u8]);

impl<'a> Iterator for Lines<'a> {
    type Item = &'a [u8];

    fn next(&mut self) -> Option<&'a [u8]> {
        let end = self.0.iter().position(|&b| b == b'\n')?;
        let line = &self.0[..end];
        self.0 = &self.0[end + 1..];
        Some(line.strip_suffix(b"\r").unwrap_or(line))
    }
}

/// The authority of `http://authority/...`.
fn absolute_host(target: &[u8]) -> Option<&[u8]> {
    let (scheme, rest) = target.split_at_checked(7)?;
    if !scheme.eq_ignore_ascii_case(b"http://") {
        return None;
    }
    let end = rest
        .iter()
        .position(|&b| b == b'/' || b == b'?')
        .unwrap_or(rest.len());
    let authority = &rest[..end];
    // Without userinfo.
    Some(match authority.iter().rposition(|&b| b == b'@') {
        Some(at) => &authority[at + 1..],
        None => authority,
    })
}

/// The name of a Host header's value, without its port; an IPv6 literal
/// without its brackets.
fn host(value: &[u8]) -> Option<String> {
    let value = value.trim_ascii();
    if let Some(literal) = value.strip_prefix(b"[") {
        let end = literal.iter().position(|&b| b == b']')?;
        let address: Ipv6Addr = std::str::from_utf8(&literal[..end]).ok()?.parse().ok()?;
        return Some(address.to_string());
    }
    let name = match value.iter().rposition(|&b| b == b':') {
        Some(colon) if value[colon + 1..].iter().all(u8::is_ascii_digit) => &value[..colon],
        Some(_) => return None,
        None => value,
    };
    valid(name)
}

/// A host name of letters, digits, '-', '_' and '.', of at most 255
/// bytes. Anything else is garbage and is not recorded.
fn valid(name: &[u8]) -> Option<String> {
    let ok = !name.is_empty()
        && name.len() <= 255
        && name
            .iter()
            .all(|&b| b.is_ascii_alphanumeric() || b == b'-' || b == b'_' || b == b'.');
    ok.then(|| String::from_utf8_lossy(name).into_owned())
}

#[cfg(test)]
pub mod tests {
    use super::*;
    use rustls::crypto::{ring, ActiveKeyExchange, SharedSecret, SupportedKxGroup};
    use rustls::pki_types::ServerName;
    use rustls::{ClientConfig, ClientConnection, NamedGroup, RootCertStore};
    use std::sync::Arc;

    /// A key exchange of X25519MLKEM768's size: its client share is 1216
    /// bytes, with X25519's share beside it. ring has no ML-KEM; a
    /// ClientHello needs the share, not the math.
    #[derive(Debug)]
    struct PostQuantum;

    struct PostQuantumShare([u8; 1216]);

    impl SupportedKxGroup for PostQuantum {
        fn start(&self) -> Result<Box<dyn ActiveKeyExchange>, rustls::Error> {
            let mut share = [0u8; 1216];
            for (i, b) in share.iter_mut().enumerate() {
                *b = (i * 7 + 3) as u8;
            }
            Ok(Box::new(PostQuantumShare(share)))
        }

        fn name(&self) -> NamedGroup {
            NamedGroup::X25519MLKEM768
        }
    }

    impl ActiveKeyExchange for PostQuantumShare {
        fn complete(self: Box<Self>, _: &[u8]) -> Result<SharedSecret, rustls::Error> {
            Err(rustls::Error::General("only for ClientHellos".into()))
        }

        fn hybrid_component(&self) -> Option<(NamedGroup, &[u8])> {
            Some((NamedGroup::X25519, &self.0[1184..]))
        }

        fn pub_key(&self) -> &[u8] {
            &self.0
        }

        fn group(&self) -> NamedGroup {
            NamedGroup::X25519MLKEM768
        }
    }

    static POST_QUANTUM: PostQuantum = PostQuantum;

    /// The first flight of a rustls client to `name`: its ClientHello in
    /// TLS records.
    pub fn client_hello(name: &str, post_quantum: bool) -> Vec<u8> {
        let mut provider = ring::default_provider();
        if post_quantum {
            provider.kx_groups.insert(0, &POST_QUANTUM);
        }
        let mut config = ClientConfig::builder_with_provider(Arc::new(provider))
            .with_safe_default_protocol_versions()
            .unwrap()
            .with_root_certificates(RootCertStore::empty())
            .with_no_client_auth();
        config.alpn_protocols = vec![b"h2".to_vec(), b"http/1.1".to_vec()];
        let name = ServerName::try_from(name.to_string()).unwrap();
        let mut client = ClientConnection::new(Arc::new(config), name).unwrap();
        let mut out = Vec::new();
        while client.wants_write() {
            client.write_tls(&mut out).unwrap();
        }
        out
    }

    /// Splits a flight's handshake message into records of at most `max`
    /// bytes, as some clients do.
    fn rerecord(flight: &[u8], max: usize) -> Vec<u8> {
        let len = u16::from_be_bytes([flight[3], flight[4]]) as usize;
        let mut out = Vec::new();
        for chunk in flight[5..5 + len].chunks(max) {
            out.extend_from_slice(&[22, 3, 1]);
            out.extend_from_slice(&(chunk.len() as u16).to_be_bytes());
            out.extend_from_slice(chunk);
        }
        out
    }

    /// A single-record ClientHello as a browser's may be: with an ECH
    /// GREASE extension (draft-ietf-tls-esni) of 250 bytes, and the
    /// server name extension last, since browsers shuffle extensions.
    pub fn browser_like(flight: &[u8]) -> Vec<u8> {
        let hello = &flight[9..];
        let mut r = Reader(hello);
        let mut head = Vec::new();
        head.extend_from_slice(r.take(34).unwrap());
        for vector in [Reader::vector8, Reader::vector16, Reader::vector8] {
            let before = r.0.len();
            vector(&mut r).unwrap();
            head.extend_from_slice(&hello[hello.len() - before..hello.len() - r.0.len()]);
        }
        let mut extensions = Reader(r.vector16().unwrap());
        let (mut sni, mut others) = (Vec::new(), Vec::new());
        while !extensions.0.is_empty() {
            let kind = extensions.u16().unwrap();
            let data = extensions.vector16().unwrap();
            let target = if kind == 0 { &mut sni } else { &mut others };
            target.extend_from_slice(&kind.to_be_bytes());
            target.extend_from_slice(&(data.len() as u16).to_be_bytes());
            target.extend_from_slice(data);
        }
        assert!(!sni.is_empty());
        others.extend_from_slice(&[0xfe, 0x0d, 0, 250]);
        others.extend((0..250u8).map(|i| i.wrapping_mul(13)));
        others.extend_from_slice(&sni);
        let mut body = head;
        body.extend_from_slice(&(others.len() as u16).to_be_bytes());
        body.extend_from_slice(&others);
        let mut message = vec![1];
        message.extend_from_slice(&(body.len() as u32).to_be_bytes()[1..]);
        message.extend_from_slice(&body);
        let mut out = vec![22, 3, 1];
        out.extend_from_slice(&(message.len() as u16).to_be_bytes());
        out.extend_from_slice(&message);
        out
    }

    fn name(name: &str) -> Found {
        Found::Name(Source::Tls, name.to_string())
    }

    #[test]
    fn a_client_hello_names_its_server() {
        let flight = client_hello("example.com", false);
        assert_eq!(find(&flight), name("example.com"));
        // Every prefix asks for more, until the name is in.
        let at = flight
            .windows(11)
            .position(|w| w == b"example.com")
            .unwrap()
            + 11;
        for len in 0..at {
            assert_eq!(find(&flight[..len]), Found::More, "{len} bytes");
        }
        assert_eq!(find(&flight[..at]), name("example.com"));
    }

    #[test]
    fn a_post_quantum_client_hello_spans_two_segments() {
        let flight = browser_like(&client_hello("pq.example.org", true));
        // Two segments of 1440 bytes (an MTU of 1500, less IPv6 and TCP).
        assert!(flight.len() > 1440, "{} bytes", flight.len());
        let at = flight
            .windows(14)
            .position(|w| w == b"pq.example.org")
            .unwrap();
        assert!(at > 1440, "the name is at {at}");
        assert_eq!(find(&flight[..1200]), Found::More);
        assert_eq!(find(&flight), name("pq.example.org"));
    }

    #[test]
    fn a_client_hello_may_span_records() {
        let flight = client_hello("records.example.net", true);
        for flight in [flight.clone(), browser_like(&flight)] {
            for max in [1, 7, 100, 512, 1000] {
                let split = rerecord(&flight, max);
                assert_eq!(
                    find(&split),
                    name("records.example.net"),
                    "records of {max}"
                );
                // More, until the records have the name.
                let found: Vec<_> = (0..split.len())
                    .step_by(7)
                    .map(|n| find(&split[..n]))
                    .collect();
                let first = found.iter().position(|f| *f != Found::More);
                let first = first.unwrap_or(found.len());
                assert!(first > 0);
                assert!(found[first..]
                    .iter()
                    .all(|f| *f == name("records.example.net")));
            }
        }
    }

    #[test]
    fn hellos_without_a_name_have_none() {
        let flight = client_hello("192.0.2.1", false);
        assert_eq!(find(&flight), Found::Nothing);
    }

    #[test]
    fn garbage_is_nothing() {
        assert_eq!(find(b"\x16\x02\x00"), Found::Nothing);
        assert_eq!(find(b"\x16\x03\x01\x00\x00"), Found::Nothing);
        assert_eq!(
            find(b"\x16\x03\x01\x00\x04\x02\x00\x00\x00"),
            Found::Nothing
        );
        assert_eq!(find(b"\x00\x01"), Found::Nothing);
        assert_eq!(find(b"SSH-2.0-OpenSSH_9.6\r\n"), Found::Nothing);
        // A complete hello whose extensions claim more than there is.
        let mut flight = client_hello("example.com", false);
        let len = flight.len();
        flight[len - 1] ^= 0xff;
        let hello = [&flight[..], &[0x16, 0x03, 0x01]].concat();
        let _ = find(&hello);
        // Names that are not host names.
        let flight = client_hello("example.com", false);
        let at = flight
            .windows(11)
            .position(|w| w == b"example.com")
            .unwrap();
        let mut bad = flight.clone();
        bad[at + 3] = b' ';
        assert_eq!(find(&bad), Found::Nothing);
        // Every byte flipped, every prefix: no panic.
        for i in 0..flight.len() {
            let mut bad = flight.clone();
            bad[i] ^= 0xa5;
            let _ = find(&bad);
            let _ = find(&flight[..i]);
        }
    }

    fn http_name(name: &str) -> Found {
        Found::Name(Source::Http, name.to_string())
    }

    #[test]
    fn http_requests_name_their_host() {
        let request = b"GET /index.html HTTP/1.1\r\nUser-Agent: curl/8\r\nHost: www.example.com\r\nAccept: */*\r\n\r\n";
        assert_eq!(find(request), http_name("www.example.com"));
        let at = request
            .windows(17)
            .position(|w| w == b"www.example.com\r\n")
            .unwrap()
            + 16;
        for len in 0..at {
            assert_eq!(find(&request[..len]), Found::More, "{len} bytes");
        }
        assert_eq!(
            find(b"POST / HTTP/1.1\nhOsT:  api.example.com:8080 \n\n"),
            http_name("api.example.com")
        );
        assert_eq!(
            find(b"GET / HTTP/1.1\r\nHost: [2001:db8::1]:80\r\n\r\n"),
            http_name("2001:db8::1")
        );
        assert_eq!(
            find(b"GET / HTTP/1.1\r\nHost: 192.0.2.1\r\n\r\n"),
            http_name("192.0.2.1")
        );
    }

    #[test]
    fn http_requests_without_host() {
        assert_eq!(
            find(b"GET / HTTP/1.0\r\nAccept: */*\r\n\r\n"),
            Found::Nothing
        );
        assert_eq!(find(b"GET / HTTP/1.0\r\nAccept: */*\r\n"), Found::More);
        assert_eq!(
            find(b"GET http://user@proxy.example.com:3128/x HTTP/1.0\r\n\r\n"),
            http_name("proxy.example.com")
        );
        assert_eq!(find(b"GET / HTTP/1.1\r\nHost: \r\n\r\n"), Found::Nothing);
        assert_eq!(find(b"GET / HTTP/1.1\r\nHost: a b\r\n\r\n"), Found::Nothing);
        assert_eq!(
            find(b"GET / HTTP/1.1\r\nHost: a<b>:c:80\r\n\r\n"),
            Found::Nothing
        );
        assert_eq!(
            find(b"GET / HTTP/1.1\r\nHost: [x]:80\r\n\r\n"),
            Found::Nothing
        );
    }

    #[test]
    fn other_protocols_are_not_http() {
        assert_eq!(find(b"PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n"), Found::Nothing);
        assert_eq!(find(b"EHLO mail.example.com\r\n"), Found::Nothing);
        assert_eq!(find(b"Get / HTTP/1.1\r\n"), Found::Nothing);
        assert_eq!(find(b"ABCDEFGHIJKLMNOPQRSTUVWXYZ"), Found::Nothing);
        assert_eq!(find(b"GET"), Found::More);
        assert_eq!(find(b"GET /"), Found::More);
        assert_eq!(find(b" GET"), Found::Nothing);
    }
}
