// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! The first bytes of the TCP connections that pods open, put together
//! from nflog's copies of their first segments, until they name a server.
//!
//! Copies come in the order the kernel logged them: a retransmission
//! repeats bytes, a reordered segment comes early, a lost one never. A
//! stream starts at its SYN's sequence number plus one; one whose SYN was
//! not seen starts at a segment that looks like the start of TLS or HTTP.
//! A stream is given up after [`MAX_BYTES`], [`MAX_SEGMENTS`] or
//! [`TIMEOUT`], and all streams together hold at most [`MAX_STREAMS`]
//! and [`MAX_BUFFERED`] bytes.

use crate::server_name::{self, Found, Source};
use std::collections::HashMap;
use std::net::IpAddr;
use std::time::{Duration, Instant};

/// The bytes of a stream that are read for a name. A post-quantum
/// ClientHello is about 1.8 KB.
pub const MAX_BYTES: usize = 8192;
/// Segments with payload per stream.
pub const MAX_SEGMENTS: u32 = 16;
/// Segments held back per stream until the bytes before them come.
const MAX_PENDING: usize = 8;
/// How long a stream is kept from its first copy on.
pub const TIMEOUT: Duration = Duration::from_secs(10);
/// Streams at once: connections opened within [`TIMEOUT`].
pub const MAX_STREAMS: usize = 16384;
/// Bytes held by all streams together.
pub const MAX_BUFFERED: usize = 16 << 20;

/// A stream: source address and port, destination address and port.
pub type Key = (IpAddr, u16, IpAddr, u16);

struct Stream {
    started: Instant,
    /// The sequence number of the stream's first byte.
    base: u32,
    data: Vec<u8>,
    /// Segments past the end of `data`: their offset and bytes.
    pending: Vec<(usize, Vec<u8>)>,
    segments: u32,
    /// Named, or given up; kept until it times out so that later copies
    /// do not start it again.
    done: bool,
}

impl Stream {
    fn new(now: Instant, base: u32) -> Self {
        Self {
            started: now,
            base,
            data: Vec::new(),
            pending: Vec::new(),
            segments: 0,
            done: false,
        }
    }

    fn buffered(&self) -> usize {
        self.data.len() + self.pending.iter().map(|(_, b)| b.len()).sum::<usize>()
    }

    fn finish(&mut self) {
        self.done = true;
        self.data = Vec::new();
        self.pending = Vec::new();
    }

    /// Adds a segment's bytes at `offset` from the stream's start; true
    /// if the stream's prefix grew.
    fn add(&mut self, offset: u32, payload: &[u8]) -> bool {
        // A segment that starts before the stream (one whose SYN was not
        // seen) counts from the stream's start.
        let (offset, payload) = match offset as i32 {
            o if o >= 0 => (o as usize, payload),
            o => match payload.get(o.unsigned_abs() as usize..) {
                Some(rest) if !rest.is_empty() => (0, rest),
                _ => return false,
            },
        };
        if offset >= MAX_BYTES {
            return false;
        }
        let payload = &payload[..payload.len().min(MAX_BYTES - offset)];
        if offset > self.data.len() {
            if self.pending.len() < MAX_PENDING {
                self.pending.push((offset, payload.to_vec()));
            }
            return false;
        }
        let before = self.data.len();
        append(&mut self.data, offset, payload);
        // Segments that came early, and now fit.
        while let Some(i) = self.pending.iter().position(|(o, _)| *o <= self.data.len()) {
            let (offset, bytes) = self.pending.swap_remove(i);
            append(&mut self.data, offset, &bytes);
        }
        self.data.len() > before
    }
}

/// Appends what `payload` at `offset` has beyond the end of `data`.
fn append(data: &mut Vec<u8>, offset: usize, payload: &[u8]) {
    let end = offset + payload.len();
    if end > data.len() {
        data.extend_from_slice(&payload[data.len() - offset..]);
    }
}

#[derive(Default)]
pub struct Streams {
    streams: HashMap<Key, Stream>,
    buffered: usize,
    swept: Option<Instant>,
}

impl Streams {
    /// Takes a copy of a segment that a client sent: the server name once
    /// the stream's first bytes tell it, once per stream.
    pub fn segment(
        &mut self,
        now: Instant,
        key: Key,
        seq: u32,
        syn: bool,
        payload: &[u8],
    ) -> Option<(Source, String)> {
        if self
            .swept
            .is_none_or(|t| now.duration_since(t) >= Duration::from_secs(1))
        {
            self.expire(now);
        }
        let offset = if syn {
            // Data on a SYN (TCP Fast Open) follows the SYN's number.
            let base = seq.wrapping_add(1);
            if self.streams.get(&key).is_none_or(|s| s.base != base) {
                self.start(now, key, base)?;
            }
            0
        } else {
            if payload.is_empty() {
                return None;
            }
            if !self.streams.contains_key(&key) {
                if !looks_like_a_start(payload) {
                    return None;
                }
                self.start(now, key, seq)?;
            }
            seq.wrapping_sub(self.streams.get(&key)?.base)
        };
        if payload.is_empty() {
            return None;
        }
        let stream = self.streams.get_mut(&key)?;
        if stream.done {
            return None;
        }
        stream.segments += 1;
        let before = stream.buffered();
        let grew = stream.add(offset, payload);
        let after = stream.buffered();
        self.buffered = self.buffered + after - before;
        let found = if stream.segments > MAX_SEGMENTS || self.buffered > MAX_BUFFERED {
            Found::Nothing
        } else if !grew {
            Found::More
        } else {
            match server_name::find(&stream.data) {
                Found::More if stream.data.len() >= MAX_BYTES => Found::Nothing,
                found => found,
            }
        };
        match found {
            Found::More => None,
            Found::Nothing => {
                self.buffered = self.buffered.saturating_sub(stream.buffered());
                stream.finish();
                None
            }
            Found::Name(source, name) => {
                self.buffered = self.buffered.saturating_sub(stream.buffered());
                stream.finish();
                Some((source, name))
            }
        }
    }

    /// Starts a stream at `base`, in place of one the key had; None when
    /// there is no room.
    fn start(&mut self, now: Instant, key: Key, base: u32) -> Option<()> {
        if let Some(old) = self.streams.remove(&key) {
            self.buffered = self.buffered.saturating_sub(old.buffered());
        }
        // Full until the next sweep, at most a second away.
        if self.streams.len() >= MAX_STREAMS {
            return None;
        }
        self.streams.insert(key, Stream::new(now, base));
        Some(())
    }

    /// Forgets streams older than [`TIMEOUT`].
    pub fn expire(&mut self, now: Instant) {
        self.swept = Some(now);
        let buffered = &mut self.buffered;
        self.streams.retain(|_, stream| {
            let keep = now.duration_since(stream.started) < TIMEOUT;
            if !keep {
                *buffered = buffered.saturating_sub(stream.buffered());
            }
            keep
        });
    }

    #[cfg(test)]
    fn len(&self) -> usize {
        self.streams.len()
    }
}

/// Whether a segment could be a stream's first: a TLS handshake record,
/// or an HTTP method.
fn looks_like_a_start(payload: &[u8]) -> bool {
    match payload {
        [22, 3, ..] | [22] => true,
        [first, ..] => first.is_ascii_uppercase(),
        [] => false,
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::server_name::tests::{browser_like, client_hello};
    use std::net::Ipv6Addr;

    const CLIENT: IpAddr = IpAddr::V6(Ipv6Addr::new(0x2001, 0xdb8, 0, 0, 0, 0, 0, 0x10));
    const SERVER: IpAddr = IpAddr::V6(Ipv6Addr::new(0x2001, 0xdb8, 0, 0, 0, 0, 0, 0x20));
    const KEY: Key = (CLIENT, 50000, SERVER, 443);
    const ISN: u32 = u32::MAX - 700;

    /// Feeds the segments of `data` (offset, length) after a SYN; the
    /// names found.
    fn feed(streams: &mut Streams, data: &[u8], segments: &[(usize, usize)]) -> Vec<String> {
        let now = Instant::now();
        let mut names = Vec::new();
        names.extend(streams.segment(now, KEY, ISN, true, &[]));
        for &(offset, len) in segments {
            let seq = ISN.wrapping_add(1).wrapping_add(offset as u32);
            let payload = &data[offset..(offset + len).min(data.len())];
            names.extend(streams.segment(now, KEY, seq, false, payload));
        }
        names.into_iter().map(|(_, name)| name).collect()
    }

    fn post_quantum() -> Vec<u8> {
        let flight = browser_like(&client_hello("two.segments.example", true));
        assert!(flight.len() > 1500);
        flight
    }

    #[test]
    fn segments_in_order() {
        let flight = post_quantum();
        let mut streams = Streams::default();
        let names = feed(&mut streams, &flight, &[(0, 1428), (1428, 1428)]);
        assert_eq!(names, ["two.segments.example"]);
        assert_eq!(streams.buffered, 0);
    }

    #[test]
    fn segments_out_of_order_and_twice() {
        let flight = post_quantum();
        let n = flight.len();
        let orders: [&[(usize, usize)]; 4] = [
            &[(1000, n), (0, 1000)],
            &[(1000, n), (500, 500), (0, 500)],
            &[(0, 700), (0, 700), (300, 900), (1200, n)],
            &[(600, 600), (1200, n), (600, 600), (0, 800)],
        ];
        for segments in orders {
            let mut streams = Streams::default();
            let names = feed(&mut streams, &flight, segments);
            assert_eq!(names, ["two.segments.example"], "{segments:?}");
            assert_eq!(streams.buffered, 0);
        }
    }

    #[test]
    fn a_name_is_recorded_once() {
        let flight = post_quantum();
        let mut streams = Streams::default();
        let names = feed(&mut streams, &flight, &[(0, 1000), (1000, 2000), (0, 2000)]);
        assert_eq!(names.len(), 1);
    }

    #[test]
    fn a_stream_without_its_syn() {
        let flight = client_hello("nosyn.example", false);
        let mut streams = Streams::default();
        let now = Instant::now();
        // The second segment alone does not start a stream.
        let at = (100..300)
            .find(|&i| !looks_like_a_start(&flight[i..]))
            .unwrap();
        let seq = 4800 + at as u32;
        assert_eq!(streams.segment(now, KEY, seq, false, &flight[at..]), None);
        assert_eq!(streams.len(), 0);
        assert_eq!(streams.segment(now, KEY, 4800, false, &flight[..at]), None);
        let found = streams.segment(now, KEY, seq, false, &flight[at..]);
        assert_eq!(found, Some((Source::Tls, "nosyn.example".to_string())));
    }

    #[test]
    fn http_over_tcp_fast_open() {
        let request = b"GET / HTTP/1.1\r\nHost: tfo.example\r\n\r\n";
        let mut streams = Streams::default();
        let found = streams.segment(Instant::now(), KEY, 77, true, request);
        assert_eq!(found, Some((Source::Http, "tfo.example".to_string())));
    }

    #[test]
    fn streams_are_given_up() {
        let mut streams = Streams::default();
        let now = Instant::now();
        // Not TLS or HTTP.
        streams.segment(now, KEY, 1, true, &[]);
        assert_eq!(streams.segment(now, KEY, 2, false, b"\x00\x01\x02"), None);
        assert_eq!(streams.buffered, 0);
        // Too many bytes without a name.
        let key = (CLIENT, 50001, SERVER, 80);
        streams.segment(now, key, 1, true, &[]);
        let request = [&b"GET / HTTP/1.1\r\nX: "[..], &[b'a'; MAX_BYTES]].concat();
        assert_eq!(streams.segment(now, key, 2, false, &request), None);
        assert_eq!(streams.buffered, 0);
        // A gap that never fills: too many segments.
        let key = (CLIENT, 50002, SERVER, 443);
        streams.segment(now, key, 1, true, &[]);
        for i in 0..MAX_SEGMENTS + 2 {
            streams.segment(now, key, 3000 + i * 10, false, &[1; 10]);
        }
        assert!(streams.streams[&key].done);
        assert_eq!(streams.buffered, 0);
        // And all of them time out.
        streams.expire(now + TIMEOUT);
        assert_eq!(streams.len(), 0);
    }

    #[test]
    fn a_new_syn_starts_the_stream_again() {
        let flight = client_hello("again.example", false);
        let mut streams = Streams::default();
        let now = Instant::now();
        streams.segment(now, KEY, 100, true, &[]);
        assert!(streams.segment(now, KEY, 101, false, &flight).is_some());
        assert!(streams.segment(now, KEY, 101, false, &flight).is_none());
        streams.segment(now, KEY, 9000, true, &[]);
        assert!(streams.segment(now, KEY, 9001, false, &flight).is_some());
    }

    #[test]
    fn the_number_of_streams_is_bounded() {
        let mut streams = Streams::default();
        let now = Instant::now();
        for port in 0..MAX_STREAMS as u32 + 10 {
            let key = (CLIENT, (port % 65536) as u16, SERVER, (port / 65536) as u16);
            streams.segment(now, key, 1, true, &[]);
            streams.segment(now, key, 2, false, b"\x16\x03\x01\x02\x00\x01");
        }
        assert_eq!(streams.len(), MAX_STREAMS);
        assert!(streams.buffered <= MAX_BUFFERED);
        streams.expire(now + TIMEOUT);
        assert_eq!((streams.len(), streams.buffered), (0, 0));
    }
}
