// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! The CRI log format a container's log is written in (ADR 0026), as
//! Kubernetes' runtimes write it: a line per entry,
//!
//! ```text
//! 2026-10-30T12:00:00.123456789Z stdout F the line's text
//! ```
//!
//! the time the guest read it (UTC, nanoseconds), the stream, `F` for a
//! line's end or `P` for a part the next entry of the stream goes on with,
//! and the text. A log's ID, which its offsets count in, is kept beside it
//! ([`id_path`]).

use chrono::{DateTime, SecondsFormat, Utc};
use feos_proto::container_service::log_entry::Source;
use std::io::{self, ErrorKind, Write};
use std::path::{Path, PathBuf};
use std::time::SystemTime;

/// The longest prefix a line of the format has, its text not counted.
pub const MAX_PREFIX: usize = 48;

/// A line of the format.
#[derive(Debug, PartialEq, Eq)]
pub struct Record<'a> {
    pub time_unix_nano: i64,
    pub source: Source,
    pub partial: bool,
    pub text: &'a [u8],
}

/// The prefix of an entry read at `time` from `source`.
pub fn prefix(time: SystemTime, source: Source, partial: bool) -> String {
    let time: DateTime<Utc> = time.into();
    let stream = match source {
        Source::Stderr => "stderr",
        _ => "stdout",
    };
    let tag = if partial { 'P' } else { 'F' };
    format!(
        "{} {stream} {tag} ",
        time.to_rfc3339_opts(SecondsFormat::Nanos, true)
    )
}

/// Appends the entry to `out`, with its newline.
pub fn write(out: &mut Vec<u8>, time: SystemTime, source: Source, partial: bool, text: &[u8]) {
    out.extend_from_slice(prefix(time, source, partial).as_bytes());
    out.extend_from_slice(text);
    out.push(b'\n');
}

/// The line, without its newline, as an entry of the format; none for a
/// line that is not one, as those written before it.
pub fn parse(line: &[u8]) -> Option<Record<'_>> {
    // The time: digits first, and short.
    if !line.first().is_some_and(u8::is_ascii_digit) {
        return None;
    }
    let space = line.iter().take(MAX_PREFIX).position(|&b| b == b' ')?;
    let time = std::str::from_utf8(&line[..space]).ok()?;
    let time = DateTime::parse_from_rfc3339(time)
        .ok()?
        .timestamp_nanos_opt()?;
    let rest = &line[space + 1..];
    let (source, rest) = match rest.split_at_checked(7)? {
        (b"stdout ", rest) => (Source::Stdout, rest),
        (b"stderr ", rest) => (Source::Stderr, rest),
        _ => return None,
    };
    let (partial, text) = match rest {
        [b'F'] => (false, &rest[1..]),
        [b'P'] => (true, &rest[1..]),
        [b'F', b' ', text @ ..] => (false, text),
        [b'P', b' ', text @ ..] => (true, text),
        _ => return None,
    };
    Some(Record {
        time_unix_nano: time,
        source,
        partial,
        text,
    })
}

/// Where the ID of the log at `log` is kept: beside it.
pub fn id_path(log: &Path) -> PathBuf {
    let mut path = log.as_os_str().to_owned();
    path.push(".id");
    PathBuf::from(path)
}

/// Whether `id` is one this module gives: 1 to 64 lowercase letters,
/// digits and "-".
fn valid_id(id: &str) -> bool {
    !id.is_empty()
        && id.len() <= 64
        && id
            .bytes()
            .all(|b| b.is_ascii_lowercase() || b.is_ascii_digit() || b == b'-')
}

/// The ID of the log at `log`; empty when it has none, as a log written
/// before ADR 0026.
pub fn read_id(log: &Path) -> String {
    match std::fs::read_to_string(id_path(log)) {
        Ok(id) if valid_id(id.trim()) => id.trim().to_string(),
        _ => String::new(),
    }
}

/// The ID of the log at `log`, made now if it has none: a random one,
/// written beside it by a rename, so that it is there whole or not at
/// all.
pub fn ensure_id(log: &Path) -> io::Result<String> {
    let id = read_id(log);
    if !id.is_empty() {
        return Ok(id);
    }
    let id = uuid::Uuid::new_v4().simple().to_string();
    let path = id_path(log);
    let temporary = path.with_extension("id.new");
    let mut file = std::fs::File::create(&temporary)?;
    file.write_all(id.as_bytes())?;
    file.sync_all()?;
    std::fs::rename(&temporary, &path)?;
    if let Some(dir) = path.parent() {
        match std::fs::File::open(dir).and_then(|dir| dir.sync_all()) {
            Err(e) if e.kind() != ErrorKind::NotFound => return Err(e),
            _ => {}
        }
    }
    Ok(id)
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::time::{Duration, UNIX_EPOCH};

    #[test]
    fn entries_are_written_and_read_back() {
        let time = UNIX_EPOCH + Duration::new(1_761_825_600, 123_456_789);
        let mut out = Vec::new();
        write(&mut out, time, Source::Stdout, false, b"the line's text");
        assert_eq!(
            out,
            b"2025-10-30T12:00:00.123456789Z stdout F the line's text\n"
        );
        let record = parse(&out[..out.len() - 1]).unwrap();
        assert_eq!(
            record,
            Record {
                time_unix_nano: 1_761_825_600_123_456_789,
                source: Source::Stdout,
                partial: false,
                text: b"the line's text",
            }
        );
        // The prefix has a fixed length, whatever the nanoseconds.
        let mut out = Vec::new();
        write(&mut out, UNIX_EPOCH, Source::Stderr, true, b"");
        assert_eq!(out, b"1970-01-01T00:00:00.000000000Z stderr P \n");
        assert!(out.len() - 1 <= MAX_PREFIX);
        let record = parse(&out[..out.len() - 1]).unwrap();
        assert_eq!((record.source, record.partial), (Source::Stderr, true));
        assert!(record.text.is_empty());
    }

    #[test]
    fn other_lines_are_not_entries() {
        for line in [
            &b""[..],
            b"hello",
            b"2026-10-30 is a date",
            b"2026-10-30T12:00:00Z",
            b"2026-10-30T12:00:00Z stdin F x",
            b"2026-10-30T12:00:00Z stdout X x",
            b"2026-10-30T12:00:00Z stdout Fx",
            b"2026-13-30T12:00:00Z stdout F x",
            b"\xff\xfe stdout F x",
        ] {
            assert!(parse(line).is_none(), "{:?}", String::from_utf8_lossy(line));
        }
        // Other runtimes' times parse too, and a text may be empty.
        let record = parse(b"2026-10-30T12:00:00+01:00 stderr F").unwrap();
        assert_eq!(record.time_unix_nano, 1_793_358_000_000_000_000);
        assert!(record.text.is_empty());
        assert_eq!(
            parse(b"2026-10-30T12:00:00.5Z stdout F  x").unwrap().text,
            b" x"
        );
    }

    #[test]
    fn a_log_keeps_its_id() {
        let dir = tempfile::tempdir().unwrap();
        let log = dir.path().join("container.log");
        assert_eq!(read_id(&log), "");
        let id = ensure_id(&log).unwrap();
        assert_eq!(id.len(), 32);
        assert_eq!(read_id(&log), id);
        assert_eq!(ensure_id(&log).unwrap(), id);
        assert_eq!(id_path(&log), dir.path().join("container.log.id"));
        // One that does not read is made anew.
        std::fs::write(id_path(&log), "Not/An/Id").unwrap();
        assert_eq!(read_id(&log), "");
        assert_ne!(ensure_id(&log).unwrap(), id);
    }
}
