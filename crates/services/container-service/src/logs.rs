// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! A container's log, `logs/container.log` in its directory, as
//! `StreamContainerLogs` serves it.
//!
//! The guest writes the container's stdout and stderr to the one file, a
//! line of the CRI format per entry (crate::output, crate::cri), which goes
//! out as its text, with its time, source and whether it is a part. A line
//! of a log written before (ADR 0026), without the format's prefix, goes
//! out as it is, without a time or a source; one over [`MAX_LINE`] bytes
//! goes out in parts of at most that many, counted from where the line
//! starts, all but the last partial. Every entry carries the byte offset
//! of its line's start in the file and the offset after it, and the log's
//! ID (crate::cri::read_id).
//!
//! `tail_lines` counts the entries of the log: a line of the format as one,
//! one of before as many as it has parts. A tail is found reading back from
//! the end in blocks, never the whole file: a line of before that reaches
//! further back than the parts asked for could fill is cut, so that its
//! last parts are a full [`MAX_LINE`] each. Nor does a tail reach back
//! further than [`MAX_TAIL_BYTES`] from the end, however few lines that
//! is, so it never sends more bytes than that: a line of before that starts
//! further back is cut the same way, to its last whole parts within those
//! bytes, or left out when not one part fits; a line of the format is left
//! out.
//!
//! With `follow`, the stream polls the file, which is only ever appended
//! to, and ends when the container is deleted (its log file goes, or
//! another one takes its place) or the caller ends the call. A line that
//! stays unfinished for [`IDLE_FLUSH`] goes out as it is. `from_offset`
//! starts it at an offset an entry named, of the log `log_id` names.

use crate::cri::{self, MAX_PREFIX};
use crate::persistence::repository::ContainerRepository;
use feos_proto::container_service::{log_entry::Source, LogEntry};
use std::fs::File;
use std::io::{self, ErrorKind};
use std::os::unix::fs::{FileExt, MetadataExt};
use std::path::{Path, PathBuf};
use std::time::Duration;
use tokio::sync::mpsc;
use tokio::time::{sleep, Instant};
use tonic::Status;

/// The longest part of a line that goes out as one entry.
pub const MAX_LINE: usize = 64 * 1024;
/// The most lines `tail_lines` sends.
pub const MAX_TAIL_LINES: u32 = 10_000;
/// The most bytes of the log a `tail_lines` tail reaches back, from its end.
pub const MAX_TAIL_BYTES: u64 = 4 << 20;
/// What a tail reads at a time, going back from the end.
const BLOCK: usize = 64 * 1024;
/// What one read takes from the file going forward.
const READ_CHUNK: usize = 256 * 1024;
/// How often a follow looks for more.
const POLL: Duration = Duration::from_millis(200);
/// How long an unfinished line waits for its end while following.
pub const IDLE_FLUSH: Duration = Duration::from_secs(1);

/// The entries a log stream sends.
pub type LogSender = mpsc::Sender<Result<LogEntry, Status>>;

/// How many entries a line of before of `len` bytes goes out as.
fn parts(len: u64, max: u64) -> u64 {
    len.div_ceil(max).max(1)
}

/// How many entries the line goes out as: one if it is of the format.
fn units(len: u64, max: u64, record: bool) -> u64 {
    if record {
        1
    } else {
        parts(len, max)
    }
}

/// Whether the line from `start` to `end`, without its newline, is one of
/// the format: shorter than `room`, which [`Lines`] holds a line for at
/// most, and with the format's prefix.
fn is_record(file: &File, start: u64, end: u64, room: u64) -> io::Result<bool> {
    let len = end - start;
    if len >= room {
        return Ok(false);
    }
    let mut prefix = vec![0u8; (len as usize).min(MAX_PREFIX)];
    file.read_exact_at(&mut prefix, start)?;
    Ok(cri::parse(&prefix).is_some())
}

/// Whether `offset` is where a line starts: the file's start, or right
/// after a newline.
fn at_line_start(file: &File, offset: u64) -> io::Result<bool> {
    if offset == 0 {
        return Ok(true);
    }
    let mut before = [0u8; 1];
    file.read_exact_at(&mut before, offset - 1)?;
    Ok(before[0] == b'\n')
}

/// Where the last `lines` lines of a log of `len` bytes start, and how many
/// leading parts of the line there to leave out. Reads back from the end in
/// blocks of `block` bytes, never further than `bytes` from the end; a line
/// of the format counts as one line, one of before as many as it has parts
/// of at most `max` bytes. A line of before whose start lies further back
/// than `lines` parts reach is cut to its last `lines` × `max` bytes; one
/// whose start lies further back than `bytes` reach is cut to its last
/// whole parts within them, or left out when not one fits. A line of the
/// format that starts further back is left out.
pub(crate) fn tail_start(
    file: &File,
    len: u64,
    lines: u64,
    bytes: u64,
    block: usize,
    max: usize,
) -> io::Result<(u64, u64)> {
    if len == 0 || lines == 0 || bytes == 0 {
        return Ok((len, 0));
    }
    let max = max.max(1) as u64;
    let room = max + MAX_PREFIX as u64;
    let mut buf = vec![0u8; block.max(1)];
    // The lines still wanted, and the end of the line looked at, without
    // its newline.
    let mut need = lines;
    let mut line_end = len;
    // Bytes below hi are still to be looked at, down to floor.
    let floor = len.saturating_sub(bytes);
    let mut hi = len;
    let mut last_block = true;
    while hi > floor {
        let lo = hi.saturating_sub(buf.len() as u64).max(floor);
        let n = (hi - lo) as usize;
        let block = &mut buf[..n];
        file.read_exact_at(block, lo)?;
        let mut k = n;
        // A newline at the very end ends the last line.
        if last_block {
            last_block = false;
            if block.last() == Some(&b'\n') {
                line_end -= 1;
                k -= 1;
            }
        }
        while let Some(q) = block[..k].iter().rposition(|&b| b == b'\n') {
            let line_start = lo + q as u64 + 1;
            let record = is_record(file, line_start, line_end, room)?;
            if let Some(found) = settle(line_start, line_end, need, max, record) {
                return Ok(found);
            }
            need -= units(line_end - line_start, max, record);
            line_end = line_start - 1;
            k = q;
        }
        // The line reaches below this block: is it already long enough,
        // and too long to be one of the format?
        if line_end - lo > need * max && line_end - lo >= room {
            return Ok((line_end - need * max, 0));
        }
        hi = lo;
    }
    if floor == 0 {
        let record = is_record(file, 0, line_end, room)?;
        return Ok(settle(0, line_end, need, max, record).unwrap_or((0, 0)));
    }
    // The bytes end inside the line looked at, or just before it starts.
    if at_line_start(file, floor)? {
        let record = is_record(file, floor, line_end, room)?;
        return Ok(settle(floor, line_end, need, max, record).unwrap_or((floor, 0)));
    }
    // It started further back. One of the format is left out; it starts
    // within `room` of its end, if it is one.
    let reach = room.saturating_sub(line_end - floor);
    if reach > 0 {
        let lo = floor.saturating_sub(reach);
        let mut back = vec![0u8; (floor - lo) as usize];
        file.read_exact_at(&mut back, lo)?;
        let start = match back.iter().rposition(|&b| b == b'\n') {
            Some(q) => Some(lo + q as u64 + 1),
            None => (lo == 0).then_some(0),
        };
        if let Some(start) = start
            && is_record(file, start, line_end, room)?
        {
            return Ok(((line_end + 1).min(len), 0));
        }
    }
    // One of before: its last whole parts within the bytes, at most need
    // of them; without one, the tail starts after it.
    let whole = ((line_end - floor) / max).min(need);
    if whole > 0 {
        return Ok((line_end - whole * max, 0));
    }
    Ok(((line_end + 1).min(len), 0))
}

/// Where the tail starts if the line from `start` to `end` holds the last
/// of the `need` lines still wanted; `record`: it is one of the format,
/// one line.
fn settle(start: u64, end: u64, need: u64, max: u64, record: bool) -> Option<(u64, u64)> {
    if record {
        return (need == 1).then_some((start, 0));
    }
    let len = end - start;
    if len > need * max {
        return Some((end - need * max, 0));
    }
    let parts = parts(len, max);
    (parts >= need).then(|| (start, parts - need))
}

/// An entry of the log, before it goes out.
#[derive(Clone, Debug, PartialEq, Eq)]
pub(crate) struct Part {
    /// Where its line, or its part of a line of before, starts in the
    /// file, and where the next entry starts.
    pub(crate) offset: u64,
    pub(crate) next_offset: u64,
    pub(crate) text: Vec<u8>,
    /// 0 and unspecified for a line of before.
    pub(crate) time_unix_nano: i64,
    pub(crate) source: Source,
    pub(crate) partial: bool,
}

/// Cuts bytes into the entries of a log: at each newline, which is left
/// out; a line of the format as one entry, one of before after every `max`
/// bytes.
pub(crate) struct Lines {
    max: usize,
    /// The most a line is held for: a part and the longest prefix. A line
    /// that reaches it is not one of the format.
    room: usize,
    /// The part of a line read so far.
    pending: Vec<u8>,
    /// The offset in the file after the bytes pushed.
    pos: u64,
    /// The line being read went out in part already, or started before the
    /// bytes pushed: its newline does not end an empty line, and it is not
    /// one of the format.
    parted: bool,
    /// Entries still to leave out, from the start of a tail.
    skip: u64,
}

impl Lines {
    #[cfg(test)]
    pub(crate) fn new(max: usize, skip: u64) -> Self {
        Self::at(max, skip, 0, false)
    }

    /// Cuts the bytes from offset `pos` on; `parted`: a line starts not
    /// there but before.
    pub(crate) fn at(max: usize, skip: u64, pos: u64, parted: bool) -> Self {
        let max = max.max(1);
        Self {
            max,
            room: max + MAX_PREFIX,
            pending: Vec::new(),
            pos,
            parted,
            skip,
        }
    }

    fn emit(&mut self, part: Part, out: &mut Vec<Part>) {
        if self.skip > 0 {
            self.skip -= 1;
        } else {
            out.push(part);
        }
    }

    /// The pending bytes of a line, from `start` to the newline before
    /// `next` or to `next`, as its entries.
    fn end(&mut self, start: u64, next: u64, out: &mut Vec<Part>) {
        let line = std::mem::take(&mut self.pending);
        if !self.parted
            && let Some(record) = cri::parse(&line)
        {
            let part = Part {
                offset: start,
                next_offset: next,
                text: record.text.to_vec(),
                time_unix_nano: record.time_unix_nano,
                source: record.source,
                partial: record.partial,
            };
            return self.emit(part, out);
        }
        if line.is_empty() && self.parted {
            return;
        }
        let count = line.len().div_ceil(self.max).max(1);
        for k in 0..count {
            let text = line
                .get(k * self.max..((k + 1) * self.max).min(line.len()))
                .unwrap_or_default();
            let offset = start + (k * self.max) as u64;
            let last = k + 1 == count;
            let part = Part {
                offset,
                next_offset: if last {
                    next
                } else {
                    offset + text.len() as u64
                },
                text: text.to_vec(),
                time_unix_nano: 0,
                source: Source::Unspecified,
                partial: !last,
            };
            self.emit(part, out);
        }
    }

    /// Sends the whole parts of a line that filled the room: it is one of
    /// before.
    fn cut(&mut self, out: &mut Vec<Part>) {
        let start = self.pos - self.pending.len() as u64;
        let whole = self.pending.len() / self.max * self.max;
        let rest = self.pending.split_off(whole);
        let line = std::mem::replace(&mut self.pending, rest);
        for (k, text) in line.chunks(self.max).enumerate() {
            let offset = start + (k * self.max) as u64;
            let part = Part {
                offset,
                next_offset: offset + text.len() as u64,
                text: text.to_vec(),
                time_unix_nano: 0,
                source: Source::Unspecified,
                partial: true,
            };
            self.emit(part, out);
        }
        self.parted = true;
    }

    /// Adds bytes, putting the entries they finish into `out`.
    pub(crate) fn push(&mut self, mut data: &[u8], out: &mut Vec<Part>) {
        while !data.is_empty() {
            let room = self.room - self.pending.len();
            let window = &data[..room.min(data.len())];
            match window.iter().position(|&b| b == b'\n') {
                Some(i) => {
                    self.pending.extend_from_slice(&window[..i]);
                    self.pos += i as u64;
                    let start = self.pos - self.pending.len() as u64;
                    self.pos += 1;
                    self.end(start, self.pos, out);
                    self.parted = false;
                    data = &data[i + 1..];
                }
                None => {
                    self.pending.extend_from_slice(window);
                    self.pos += window.len() as u64;
                    data = &data[window.len()..];
                    if self.pending.len() == self.room {
                        self.cut(out);
                    }
                }
            }
        }
    }

    /// Whether part of a line waits for its end.
    pub(crate) fn has_pending(&self) -> bool {
        !self.pending.is_empty()
    }

    /// Sends the unfinished line as it is: at the end of the log, or when
    /// it waited long enough.
    pub(crate) fn flush(&mut self, out: &mut Vec<Part>) {
        if !self.pending.is_empty() {
            let start = self.pos - self.pending.len() as u64;
            self.end(start, self.pos, out);
            self.parted = true;
        }
    }
}

/// An open log file and how far it was read.
pub(crate) struct LogFile {
    file: File,
    /// The file's device and inode: another file at its path, or none, is
    /// a deleted container.
    identity: (u64, u64),
    /// Its ID, which the entries carry.
    log_id: String,
    offset: u64,
    lines: Lines,
}

impl LogFile {
    /// Opens the log at `path`; none while it does not exist.
    fn open(path: &Path) -> io::Result<Option<Self>> {
        let file = match File::open(path) {
            Ok(file) => file,
            Err(e) if e.kind() == ErrorKind::NotFound => return Ok(None),
            Err(e) => return Err(e),
        };
        let meta = file.metadata()?;
        Ok(Some(Self {
            file,
            identity: (meta.dev(), meta.ino()),
            log_id: cri::read_id(path),
            offset: 0,
            lines: Lines::at(MAX_LINE, 0, 0, false),
        }))
    }

    /// Reads on from where it was, up to `end` or the end of the file, at
    /// most [`READ_CHUNK`] bytes. Returns the entries finished and the
    /// bytes read; none at the end.
    fn read(&mut self, end: Option<u64>) -> io::Result<(Vec<Part>, usize)> {
        let mut want = READ_CHUNK;
        if let Some(end) = end {
            want = want.min(end.saturating_sub(self.offset) as usize);
        }
        let mut buf = vec![0u8; want];
        let n = loop {
            match self.file.read_at(&mut buf, self.offset) {
                Err(e) if e.kind() == ErrorKind::Interrupted => continue,
                result => break result?,
            }
        };
        self.offset += n as u64;
        let mut out = Vec::new();
        self.lines.push(&buf[..n], &mut out);
        Ok((out, n))
    }
}

/// A log opened for a stream: the file, if it exists yet, and where the
/// previous logs end.
pub(crate) struct Opened {
    log: Option<LogFile>,
    end: u64,
}

/// Where a stream starts.
#[derive(Clone, Debug, Default)]
pub(crate) struct Start {
    pub(crate) tail_lines: u32,
    pub(crate) tail: bool,
    /// With follow: the offset and the log it is of; wins over the tails
    /// when either is set.
    pub(crate) from_offset: u64,
    pub(crate) log_id: String,
}

/// Opens the log at `path` and finds where the stream starts: at
/// `from_offset` if it is of the log there and within it, else at 0, when
/// it or `log_id` is set; the last `tail_lines` lines (capped at
/// [`MAX_TAIL_LINES`] and [`MAX_TAIL_BYTES`]) when above 0, else the whole
/// log with `tail`, else its end.
pub(crate) fn open(path: &Path, start: &Start) -> io::Result<Opened> {
    let Some(mut log) = LogFile::open(path)? else {
        return Ok(Opened { log: None, end: 0 });
    };
    let len = log.file.metadata()?.len();
    let (offset, skip) = if start.from_offset > 0 || !start.log_id.is_empty() {
        let same = start.log_id.is_empty() || start.log_id == log.log_id;
        match same && start.from_offset <= len {
            true => (start.from_offset, 0),
            false => (0, 0),
        }
    } else {
        match start.tail_lines.min(MAX_TAIL_LINES) {
            0 if start.tail => (0, 0),
            0 => (len, 0),
            lines => tail_start(
                &log.file,
                len,
                lines.into(),
                MAX_TAIL_BYTES,
                BLOCK,
                MAX_LINE,
            )?,
        }
    };
    log.offset = offset;
    let parted = !at_line_start(&log.file, offset)?;
    log.lines = Lines::at(MAX_LINE, skip, offset, parted);
    Ok(Opened {
        log: Some(log),
        end: len,
    })
}

/// Runs `f` on the blocking pool: the log's reads block.
async fn blocking<T: Send + 'static>(
    f: impl FnOnce() -> io::Result<T> + Send + 'static,
) -> Result<T, Status> {
    tokio::task::spawn_blocking(f)
        .await
        .map_err(|e| Status::internal(format!("log reader: {e}")))?
        .map_err(|e| Status::internal(format!("container log: {e}")))
}

/// Why a stream ends before it is done.
enum End {
    /// The caller ended the call.
    Gone,
    Failed(Status),
}

impl From<Status> for End {
    fn from(status: Status) -> Self {
        End::Failed(status)
    }
}

async fn send(tx: &LogSender, log_id: &str, parts: Vec<Part>) -> Result<(), End> {
    for part in parts {
        let entry = LogEntry {
            line: part.text,
            source: part.source as i32,
            offset: part.offset,
            next_offset: part.next_offset,
            time_unix_nano: part.time_unix_nano,
            partial: part.partial,
            log_id: log_id.to_string(),
        };
        tx.send(Ok(entry)).await.map_err(|_| End::Gone)?;
    }
    Ok(())
}

/// Reads `log` on up to `end` or the end of the file, on the blocking pool,
/// and sends what it finishes. Returns the log and whether it read any.
async fn read_through(
    mut log: LogFile,
    end: Option<u64>,
    tx: &LogSender,
) -> Result<(LogFile, bool), End> {
    let mut read_any = false;
    loop {
        let (read, parts, n) = blocking(move || {
            let (parts, n) = log.read(end)?;
            Ok((log, parts, n))
        })
        .await?;
        log = read;
        send(tx, &log.log_id, parts).await?;
        if n == 0 {
            return Ok((log, read_any));
        }
        read_any = true;
    }
}

/// Sends the unfinished line of `log` as it is.
async fn flush(log: &mut LogFile, tx: &LogSender) -> Result<(), End> {
    let mut out = Vec::new();
    log.lines.flush(&mut out);
    send(tx, &log.log_id, out).await
}

/// Whether the file at `path` is no longer the one of `identity`.
async fn replaced(path: &Path, identity: (u64, u64)) -> bool {
    match tokio::fs::metadata(path).await {
        Ok(meta) => (meta.dev(), meta.ino()) != identity,
        Err(e) => e.kind() == ErrorKind::NotFound,
    }
}

/// Sends the log of container `id` from where `opened` starts, and with
/// `follow` what is appended to it, until the container is deleted or the
/// caller ends the call. A failure ends the stream with its status.
pub(crate) async fn stream(
    opened: Opened,
    follow: bool,
    id: String,
    path: PathBuf,
    repository: ContainerRepository,
    tx: LogSender,
) {
    if let Err(End::Failed(status)) = run(opened, follow, &id, &path, &repository, &tx).await {
        log::warn!("Logs: The log stream of container {id} failed: {status}");
        let _ = tx.send(Err(status)).await;
    }
}

async fn run(
    opened: Opened,
    follow: bool,
    id: &str,
    path: &Path,
    repository: &ContainerRepository,
    tx: &LogSender,
) -> Result<(), End> {
    // The previous logs.
    let mut log = match opened.log {
        Some(log) => Some(read_through(log, Some(opened.end), tx).await?.0),
        None => None,
    };
    if !follow {
        if let Some(log) = &mut log {
            flush(log, tx).await?;
        }
        return Ok(());
    }

    let mut last_read = Instant::now();
    loop {
        tokio::select! {
            _ = tx.closed() => return Ok(()),
            _ = sleep(POLL) => {}
        }
        let Some(file) = log.take() else {
            // No log yet, while the image is pulled.
            let record = repository
                .get_container(id)
                .await
                .map_err(|e| Status::internal(e.to_string()))?;
            if record.is_none() {
                return Ok(());
            }
            let open_path = path.to_path_buf();
            log = blocking(move || LogFile::open(&open_path)).await?;
            last_read = Instant::now();
            continue;
        };
        let (mut file, appended) = read_through(file, None, tx).await?;
        if appended {
            last_read = Instant::now();
        } else if replaced(path, file.identity).await {
            // The container was deleted: what it wrote before goes out
            // still.
            let (mut file, _) = read_through(file, None, tx).await?;
            return flush(&mut file, tx).await;
        } else if file.lines.has_pending() && last_read.elapsed() >= IDLE_FLUSH {
            flush(&mut file, tx).await?;
        }
        log = Some(file);
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::io::Write;

    fn file_with(content: &[u8]) -> tempfile::NamedTempFile {
        let mut file = tempfile::NamedTempFile::new().unwrap();
        file.write_all(content).unwrap();
        file.flush().unwrap();
        file
    }

    /// The entries of the last `lines` lines of `content`, read back in
    /// blocks of `block` bytes, with parts of at most `max` bytes.
    fn tail(content: &[u8], lines: u64, block: usize, max: usize) -> Vec<Vec<u8>> {
        let file = file_with(content);
        tail_of(file.as_file(), content, lines, u64::MAX, block, max)
    }

    /// The same of `content` in `file`, reaching back at most `bytes`.
    fn tail_of(
        file: &File,
        content: &[u8],
        lines: u64,
        bytes: u64,
        block: usize,
        max: usize,
    ) -> Vec<Vec<u8>> {
        let len = content.len() as u64;
        let (start, skip) = tail_start(file, len, lines, bytes, block, max).unwrap();
        let parted = !at_line_start(file, start).unwrap();
        let mut split = Lines::at(max, skip, start, parted);
        let mut out = Vec::new();
        split.push(&content[start as usize..], &mut out);
        split.flush(&mut out);
        texts(out)
    }

    fn texts(parts: Vec<Part>) -> Vec<Vec<u8>> {
        parts.into_iter().map(|part| part.text).collect()
    }

    /// All entries of `content`, with parts of at most `max` bytes.
    fn entries(content: &[u8], max: usize) -> Vec<Vec<u8>> {
        texts(parts_of(content, max))
    }

    /// The same, as parts.
    fn parts_of(content: &[u8], max: usize) -> Vec<Part> {
        let mut split = Lines::new(max, 0);
        let mut out = Vec::new();
        split.push(content, &mut out);
        split.flush(&mut out);
        out
    }

    fn strings(entries: &[Vec<u8>]) -> Vec<String> {
        entries
            .iter()
            .map(|e| String::from_utf8_lossy(e).into_owned())
            .collect()
    }

    #[test]
    fn a_tail_is_the_last_lines() {
        let content = b"one\ntwo\nthree\nfour\n";
        for block in [1, 2, 3, 5, 7, 64 * 1024] {
            assert_eq!(
                strings(&tail(content, 2, block, MAX_LINE)),
                ["three", "four"]
            );
            assert_eq!(
                strings(&tail(content, 4, block, MAX_LINE)),
                ["one", "two", "three", "four"]
            );
            assert_eq!(
                strings(&tail(content, 50, block, MAX_LINE)),
                ["one", "two", "three", "four"],
                "more lines asked for than there are, block {block}"
            );
        }
    }

    #[test]
    fn a_tail_ends_with_an_unfinished_line() {
        for block in [1, 2, 4, 64 * 1024] {
            assert_eq!(strings(&tail(b"a\nb\nc", 2, block, MAX_LINE)), ["b", "c"]);
            assert_eq!(strings(&tail(b"only", 1, block, MAX_LINE)), ["only"]);
        }
    }

    #[test]
    fn empty_lines_count() {
        for block in [1, 3, 64 * 1024] {
            assert_eq!(strings(&tail(b"a\n\n", 1, block, MAX_LINE)), [""]);
            assert_eq!(strings(&tail(b"\n", 5, block, MAX_LINE)), [""]);
            assert_eq!(strings(&tail(b"a\n\nb\n", 2, block, MAX_LINE)), ["", "b"]);
            assert_eq!(strings(&tail(b"\n\n\n", 2, block, MAX_LINE)), ["", ""]);
        }
    }

    #[test]
    fn an_empty_log_has_no_tail() {
        let file = file_with(b"");
        assert_eq!(
            tail_start(file.as_file(), 0, 10, MAX_TAIL_BYTES, BLOCK, MAX_LINE).unwrap(),
            (0, 0)
        );
        assert!(tail(b"", 10, BLOCK, MAX_LINE).is_empty());
    }

    #[test]
    fn long_lines_go_out_in_parts() {
        // 200 KiB and a bit: four parts of 64 KiB, 64 KiB, 64 KiB and 8 KiB.
        let mut long = vec![b'x'; 3 * MAX_LINE];
        long.extend(vec![b'y'; 8 * 1024]);
        let mut content = b"before\n".to_vec();
        content.extend_from_slice(&long);
        content.extend_from_slice(b"\nafter\n");

        let all = entries(&content, MAX_LINE);
        let lens: Vec<_> = all.iter().map(Vec::len).collect();
        assert_eq!(lens, [6, MAX_LINE, MAX_LINE, MAX_LINE, 8 * 1024, 5]);
        assert_eq!(all[1..5].concat(), long);

        // A tail counts the parts, and cuts them as from the line's start
        // when it reaches it.
        assert_eq!(tail(&content, 5, BLOCK, MAX_LINE), all[1..]);
        assert_eq!(tail(&content, 6, BLOCK, MAX_LINE), all);
        // With fewer, the long line reaches back further than its parts
        // asked for: it is cut to its last parts of 64 KiB.
        let four = tail(&content, 4, BLOCK, MAX_LINE);
        let lens: Vec<_> = four.iter().map(Vec::len).collect();
        assert_eq!(lens, [MAX_LINE, MAX_LINE, MAX_LINE, 5]);
        assert_eq!(four[..3].concat(), long[long.len() - 3 * MAX_LINE..]);
        assert_eq!(four, model(&content, 4, MAX_LINE));
        let two = tail(&content, 2, BLOCK, MAX_LINE);
        assert_eq!(two.len(), 2);
        assert_eq!(two[0], long[long.len() - MAX_LINE..]);
        assert_eq!(two[1], b"after");
    }

    #[test]
    fn a_line_of_exactly_the_most_is_one_entry() {
        let mut content = vec![b'x'; MAX_LINE];
        content.extend_from_slice(b"\nnext\n");
        let all = entries(&content, MAX_LINE);
        assert_eq!(all.len(), 2);
        assert_eq!(all[0].len(), MAX_LINE);
        assert_eq!(tail(&content, 2, 1000, MAX_LINE), all);
        // And without a newline after it.
        assert_eq!(entries(&vec![b'x'; MAX_LINE], MAX_LINE).len(), 1);
    }

    #[test]
    fn a_line_longer_than_the_tail_reaches_is_cut() {
        // One line of 10 parts and a bit, without a newline: a tail of 2
        // reads back 2 parts and no further.
        let mut content = vec![b'a'; 5];
        for c in b'0'..=b'9' {
            content.extend(vec![c; 100]);
        }
        let max = 100;
        let file = file_with(&content);
        let len = content.len() as u64;
        assert_eq!(
            tail_start(file.as_file(), len, 2, u64::MAX, 7, max).unwrap(),
            (len - 200, 0)
        );
        assert_eq!(
            tail(&content, 2, 7, max),
            [vec![b'8'; 100], vec![b'9'; 100]]
        );
        // Whatever the block size, the same.
        for block in [1, 13, 100, 101, 4096] {
            assert_eq!(tail(&content, 2, block, max).len(), 2, "block {block}");
        }
    }

    /// The tail as the module's documentation says it, in memory.
    fn model(content: &[u8], lines: usize, max: usize) -> Vec<Vec<u8>> {
        model_in(content, lines, usize::MAX, max)
    }

    /// The same, reaching back at most `bytes` from the end.
    fn model_in(content: &[u8], lines: usize, bytes: usize, max: usize) -> Vec<Vec<u8>> {
        if content.is_empty() {
            return Vec::new();
        }
        let floor = content.len().saturating_sub(bytes);
        let mut all: Vec<&[u8]> = content.split(|&b| b == b'\n').collect();
        if content.ends_with(b"\n") {
            all.pop();
        }
        let parts = |line: &[u8]| -> Vec<Vec<u8>> {
            if line.is_empty() {
                vec![Vec::new()]
            } else {
                line.chunks(max).map(<[u8]>::to_vec).collect()
            }
        };
        let mut need = lines;
        let mut picked = Vec::new();
        // Where the line looked at ends, at its newline if it has one.
        let mut end = content.len() - usize::from(content.ends_with(b"\n"));
        for line in all.iter().rev() {
            if need == 0 {
                break;
            }
            let start = end - line.len();
            end = start.saturating_sub(1);
            // A line of the format is one entry, or none when it starts
            // further back than the bytes reach.
            let record = (line.len() < max + MAX_PREFIX)
                .then(|| cri::parse(line))
                .flatten();
            if let Some(record) = record {
                if start < floor {
                    break;
                }
                picked.push(vec![record.text.to_vec()]);
                need -= 1;
                continue;
            }
            if start < floor {
                // It starts further back than the bytes reach: its last
                // whole parts within them, if one fits.
                let whole = ((start + line.len()).saturating_sub(floor) / max).min(need);
                if whole > 0 {
                    picked.push(parts(&line[line.len() - whole * max..]));
                }
                break;
            }
            if line.len() > need * max {
                picked.push(parts(&line[line.len() - need * max..]));
                break;
            }
            let p = parts(line);
            if p.len() >= need {
                picked.push(p[p.len() - need..].to_vec());
                break;
            }
            need -= p.len();
            picked.push(p);
        }
        picked.into_iter().rev().flatten().collect()
    }

    #[test]
    fn tails_are_the_same_whatever_the_blocks() {
        // Lines of pseudo-random lengths, some empty, some over the most.
        let max = 16;
        let mut content = Vec::new();
        let mut seed: u32 = 1;
        for i in 0..200 {
            seed = seed.wrapping_mul(1_103_515_245).wrapping_add(12345);
            let len = (seed >> 16) % 40;
            content.extend(std::iter::repeat_n(b'a' + (i % 26) as u8, len as usize));
            content.push(b'\n');
        }
        content.extend_from_slice(b"tail");
        let all = entries(&content, max);
        assert_eq!(model(&content, 100_000, max), all);
        for lines in [1, 2, 3, 10, 57, 199, 200, 1000] {
            let want = model(&content, lines, max);
            assert!(want.len() <= lines);
            for block in [1, 2, 5, 16, 17, 64, 4096] {
                let got = tail(&content, lines as u64, block, max);
                assert_eq!(got, want, "{lines} lines, block {block}");
            }
        }
        // Where no line is cut, a tail is the end of all entries.
        let short: Vec<u8> = content
            .split(|&b| b == b'\n')
            .map(|line| &line[..line.len().min(max)])
            .collect::<Vec<_>>()
            .join(&b'\n');
        let all = entries(&short, max);
        for lines in [1, 7, 150, 300] {
            let want = &all[all.len().saturating_sub(lines)..];
            assert_eq!(tail(&short, lines as u64, 3, max), want, "{lines} lines");
        }
    }

    #[test]
    fn a_tail_reaches_back_no_further_than_its_bytes() {
        // "aa" at 0, newline at 2; ten b at 3, newline at 13; "cc" at 14,
        // newline at 16.
        let content = b"aa\nbbbbbbbbbb\ncc\n";
        let file = file_with(content);
        let read = |lines, bytes, block| {
            strings(&tail_of(file.as_file(), content, lines, bytes, block, 4))
        };
        for block in [1, 3, 4, 64] {
            // The bytes end just before a line: it is the first.
            assert_eq!(read(10, 3, block), ["cc"], "block {block}");
            assert_eq!(read(10, 14, block), ["bbbb", "bbbb", "bb", "cc"]);
            // They end at a newline, or in a line too short for one whole
            // part: the line is left out.
            assert_eq!(read(10, 4, block), ["cc"], "block {block}");
            assert_eq!(read(10, 15, block), ["bbbb", "bbbb", "bb", "cc"]);
            assert_eq!(read(10, 16, block), ["bbbb", "bbbb", "bb", "cc"]);
            // They end in a long line: its last whole parts within them,
            // cut as from its end, as a line the lines do not reach.
            assert_eq!(read(10, 8, block), ["bbbb", "cc"], "block {block}");
            assert_eq!(read(10, 12, block), ["bbbb", "bbbb", "cc"]);
            assert_eq!(read(10, 13, block), ["bbbb", "bbbb", "cc"]);
            // The lines still count: no more parts than they ask for.
            assert_eq!(read(2, 12, block), ["bbbb", "cc"], "block {block}");
            assert_eq!(read(1, 12, block), ["cc"], "block {block}");
            // All bytes: all of the log; none: none of it.
            assert_eq!(read(10, 17, block), ["aa", "bbbb", "bbbb", "bb", "cc"]);
            assert_eq!(read(10, 1000, block), ["aa", "bbbb", "bbbb", "bb", "cc"]);
            assert!(read(10, 0, block).is_empty());
            // An empty last line takes its newline.
            assert!(read(10, 1, block).is_empty());
            let empty = b"a\n\n";
            let file = file_with(empty);
            let got = tail_of(file.as_file(), empty, 10, 1, block, 4);
            assert_eq!(strings(&got), [""], "block {block}");
        }
        // An unfinished last line is cut the same way.
        let content = b"aa\nbbbbbbbbbb";
        let file = file_with(content);
        for block in [1, 5, 64] {
            let read = |bytes| strings(&tail_of(file.as_file(), content, 10, bytes, block, 4));
            assert_eq!(read(6), ["bbbb"], "block {block}");
            assert!(read(3).is_empty(), "block {block}");
            assert_eq!(read(11), ["bbbb", "bbbb", "bb"], "block {block}");
        }
    }

    #[test]
    fn tails_capped_in_bytes_are_the_same_whatever_the_blocks() {
        // Lines of pseudo-random lengths, some empty, some over the most.
        let max = 8;
        let mut content = Vec::new();
        let mut seed: u32 = 7;
        for i in 0..60 {
            seed = seed.wrapping_mul(1_103_515_245).wrapping_add(12345);
            let len = (seed >> 16) % 30;
            content.extend(std::iter::repeat_n(b'a' + (i % 26) as u8, len as usize));
            content.push(b'\n');
        }
        content.extend_from_slice(b"unfinished");
        let file = file_with(&content);
        for bytes in 0..=content.len() + 1 {
            for lines in [1, 2, 5, 40, 1000] {
                let want = model_in(&content, lines, bytes, max);
                let sent: usize = want.iter().map(Vec::len).sum();
                assert!(sent <= bytes && want.len() <= lines);
                for block in [1, 3, 8, 9, 4096] {
                    let got = tail_of(
                        file.as_file(),
                        &content,
                        lines as u64,
                        bytes as u64,
                        block,
                        max,
                    );
                    assert_eq!(got, want, "{lines} lines, {bytes} bytes, block {block}");
                }
            }
        }
        // With all of its bytes, a tail is the tail by lines alone.
        let len = content.len();
        for lines in [1, 2, 5, 40, 1000] {
            assert_eq!(
                model_in(&content, lines, len, max),
                model(&content, lines, max)
            );
        }
    }

    #[test]
    fn the_tail_is_capped() {
        let content: Vec<u8> = (0..MAX_TAIL_LINES + 5)
            .flat_map(|i| format!("{i}\n").into_bytes())
            .collect();
        let file = file_with(&content);
        let opened = open(file.path(), &tail_of_lines(u32::MAX, false)).unwrap();
        let mut log = opened.log.unwrap();
        let (got, _) = log.read(Some(opened.end)).unwrap();
        assert_eq!(got.len(), MAX_TAIL_LINES as usize);
        assert_eq!(got[0].text, b"5");
    }

    /// The entries a stream without follow sends for the log at `path`.
    fn read_all(path: &Path, tail_lines: u32, tail: bool) -> Vec<Vec<u8>> {
        texts(read_parts(path, &tail_of_lines(tail_lines, tail)))
    }

    fn tail_of_lines(tail_lines: u32, tail: bool) -> Start {
        Start {
            tail_lines,
            tail,
            ..Default::default()
        }
    }

    /// The parts a stream without follow sends for the log at `path`.
    fn read_parts(path: &Path, start: &Start) -> Vec<Part> {
        let opened = open(path, start).unwrap();
        let mut log = opened.log.unwrap();
        let mut out = Vec::new();
        loop {
            let (got, n) = log.read(Some(opened.end)).unwrap();
            out.extend(got);
            if n == 0 {
                break;
            }
        }
        log.lines.flush(&mut out);
        out
    }

    #[test]
    fn the_tail_is_capped_in_bytes() {
        // A few short lines, 100 of 64 KiB, and a few short ones again:
        // 6.4 MiB in all, so 10 000 lines would be the whole log.
        let mut content = Vec::new();
        for i in 0..3 {
            content.extend(format!("first {i}\n").into_bytes());
        }
        for i in 0..100u8 {
            content.extend(vec![b'0' + i % 75; MAX_LINE]);
            content.push(b'\n');
        }
        for i in 0..3 {
            content.extend(format!("last {i}\n").into_bytes());
        }
        let file = file_with(&content);
        let all = entries(&content, MAX_LINE);
        assert_eq!(all.len(), 106);

        let got = read_all(file.path(), MAX_TAIL_LINES, false);
        let bytes: usize = got.iter().map(Vec::len).sum();
        assert!(
            bytes as u64 <= MAX_TAIL_BYTES,
            "{bytes} bytes in a tail, more than {MAX_TAIL_BYTES}"
        );
        // The last lines of the log, whole: the line the budget ends in is
        // left out, as not one part of it fits.
        assert_eq!(got, all[all.len() - 66..]);
        assert_eq!(got.last().unwrap(), b"last 2");
        // The whole log still goes out with tail.
        assert_eq!(read_all(file.path(), 0, true), all);
    }

    #[test]
    fn open_starts_where_the_request_says() {
        let file = file_with(b"one\ntwo\nthree\n");
        let read = |tail_lines, tail| {
            let opened = open(file.path(), &tail_of_lines(tail_lines, tail)).unwrap();
            let mut log = opened.log.unwrap();
            let (got, _) = log.read(Some(opened.end)).unwrap();
            strings(&texts(got))
        };
        assert_eq!(read(0, true), ["one", "two", "three"]);
        assert!(read(0, false).is_empty());
        assert_eq!(read(1, false), ["three"]);
        assert_eq!(read(2, true), ["two", "three"]);

        let missing = open(
            Path::new("/nonexistent/container.log"),
            &tail_of_lines(5, true),
        )
        .unwrap();
        assert!(missing.log.is_none());
    }

    /// A line of the format, read at `second`.
    fn record(second: u64, source: Source, partial: bool, text: &[u8]) -> Vec<u8> {
        let time = std::time::UNIX_EPOCH + Duration::from_secs(second);
        let mut out = Vec::new();
        cri::write(&mut out, time, source, partial, text);
        out
    }

    #[test]
    fn lines_of_the_format_go_out_as_their_text() {
        let mut content = b"old line\n".to_vec();
        let first = content.len() as u64;
        content.extend(record(1, Source::Stdout, false, b"hello"));
        let second = content.len() as u64;
        content.extend(record(2, Source::Stderr, true, b"a part"));
        let third = content.len() as u64;
        content.extend(record(3, Source::Stderr, false, b""));
        let end = content.len() as u64;
        let parts = parts_of(&content, MAX_LINE);
        assert_eq!(
            parts,
            [
                Part {
                    offset: 0,
                    next_offset: first,
                    text: b"old line".to_vec(),
                    time_unix_nano: 0,
                    source: Source::Unspecified,
                    partial: false,
                },
                Part {
                    offset: first,
                    next_offset: second,
                    text: b"hello".to_vec(),
                    time_unix_nano: 1_000_000_000,
                    source: Source::Stdout,
                    partial: false,
                },
                Part {
                    offset: second,
                    next_offset: third,
                    text: b"a part".to_vec(),
                    time_unix_nano: 2_000_000_000,
                    source: Source::Stderr,
                    partial: true,
                },
                Part {
                    offset: third,
                    next_offset: end,
                    text: Vec::new(),
                    time_unix_nano: 3_000_000_000,
                    source: Source::Stderr,
                    partial: false,
                },
            ]
        );
        // A line of before over the most goes out in parts, all but the
        // last partial, each where it starts.
        let mut long = vec![b'x'; 2 * MAX_LINE + 5];
        long.push(b'\n');
        let parts = parts_of(&long, MAX_LINE);
        let at: Vec<_> = parts
            .iter()
            .map(|p| (p.offset, p.next_offset, p.partial))
            .collect();
        let max = MAX_LINE as u64;
        assert_eq!(
            at,
            [
                (0, max, true),
                (max, 2 * max, true),
                (2 * max, 2 * max + 6, false)
            ]
        );
    }

    #[test]
    fn a_full_part_of_the_format_is_one_entry() {
        let text = vec![b'y'; MAX_LINE];
        let mut content = record(1, Source::Stdout, true, &text);
        content.extend(record(2, Source::Stdout, false, b"rest"));
        let file = file_with(&content);
        let all = read_parts(file.path(), &tail_of_lines(0, true));
        assert_eq!(all.len(), 2);
        assert_eq!(all[0].text, text);
        assert!(all[0].partial);
        assert_eq!(all[1].text, b"rest");
        assert_eq!(all[1].next_offset, content.len() as u64);
        // A tail counts it as one line.
        assert_eq!(read_all(file.path(), 1, false), [b"rest".to_vec()]);
        assert_eq!(read_all(file.path(), 2, false), [text, b"rest".to_vec()]);
    }

    #[test]
    fn tails_over_both_formats_are_the_same_whatever_the_blocks() {
        // Lines of before and of the format, of pseudo-random lengths.
        let max = 16;
        let mut content = Vec::new();
        let mut seed: u32 = 11;
        for i in 0..60u64 {
            seed = seed.wrapping_mul(1_103_515_245).wrapping_add(12345);
            let len = ((seed >> 16) % 40) as usize;
            let byte = b'a' + (i % 26) as u8;
            if (seed >> 8).is_multiple_of(3) {
                let source = if i % 2 == 0 {
                    Source::Stdout
                } else {
                    Source::Stderr
                };
                content.extend(record(i, source, false, &vec![byte; len % max]));
            } else {
                content.extend(std::iter::repeat_n(byte, len));
                content.push(b'\n');
            }
        }
        let file = file_with(&content);
        for bytes in 0..=content.len() + 1 {
            for lines in [1, 2, 5, 40, 1000] {
                let want = model_in(&content, lines, bytes, max);
                for block in [1, 3, 17, 4096] {
                    let got = tail_of(
                        file.as_file(),
                        &content,
                        lines as u64,
                        bytes as u64,
                        block,
                        max,
                    );
                    assert_eq!(got, want, "{lines} lines, {bytes} bytes, block {block}");
                }
            }
        }
    }

    #[test]
    fn a_follow_starts_at_an_offset_of_its_log() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("container.log");
        let id = cri::ensure_id(&path).unwrap();
        let mut content = record(1, Source::Stdout, false, b"one");
        let second = content.len() as u64;
        content.extend(record(2, Source::Stdout, false, b"two"));
        std::fs::write(&path, &content).unwrap();
        let from = |from_offset, log_id: &str| {
            let start = Start {
                tail_lines: 1,
                from_offset,
                log_id: log_id.to_string(),
                ..Default::default()
            };
            strings(&texts(read_parts(&path, &start)))
        };
        // From an offset of the log, whatever the tail says.
        assert_eq!(from(second, &id), ["two"]);
        assert_eq!(from(second, ""), ["two"]);
        assert_eq!(from(0, &id), ["one", "two"]);
        // Of another log, or past its end: from its start.
        assert_eq!(from(second, "another"), ["one", "two"]);
        assert_eq!(from(content.len() as u64 + 1, &id), ["one", "two"]);
        // At its end: nothing yet.
        assert!(from(content.len() as u64, &id).is_empty());
        // The entries carry the log's ID.
        let (tx, mut rx) = mpsc::channel(4);
        let rt = tokio::runtime::Builder::new_current_thread()
            .build()
            .unwrap();
        let opened = open(&path, &tail_of_lines(1, false)).unwrap();
        let log = opened.log.unwrap();
        rt.block_on(async {
            let (_, read) = read_through(log, Some(opened.end), &tx).await.ok().unwrap();
            assert!(read);
            let entry = rx.recv().await.unwrap().unwrap();
            assert_eq!(entry.log_id, id);
            assert_eq!(entry.offset, second);
            assert_eq!(entry.next_offset, content.len() as u64);
            assert_eq!(entry.time_unix_nano, 2_000_000_000);
            assert_eq!(entry.source, Source::Stdout as i32);
        });
    }

    #[test]
    fn a_follow_from_inside_a_long_line_of_before_goes_on_with_it() {
        let mut content = vec![b'x'; 10];
        content.extend(b"\nnext\n");
        let file = file_with(&content);
        // The offset after the first of the line's parts (of 4 bytes).
        let mut split = Lines::at(4, 0, 4, true);
        let mut out = Vec::new();
        split.push(&content[4..], &mut out);
        let at: Vec<_> = out
            .iter()
            .map(|p| (p.offset, String::from_utf8_lossy(&p.text).into_owned()))
            .collect();
        assert_eq!(
            at,
            [
                (4, "xxxx".to_string()),
                (8, "xx".to_string()),
                (11, "next".to_string())
            ]
        );
        assert!(!at_line_start(file.as_file(), 4).unwrap());
        assert!(at_line_start(file.as_file(), 11).unwrap());
    }

    #[test]
    fn lines_cut_across_reads() {
        let mut split = Lines::new(4, 0);
        let mut out = Vec::new();
        for chunk in [&b"ab"[..], b"c\nde", b"fghij", b"\n", b"\nk"] {
            split.push(chunk, &mut out);
        }
        assert_eq!(strings(&texts(out.clone())), ["abc", "defg", "hij", ""]);
        assert!(split.has_pending());
        split.flush(&mut out);
        assert_eq!(
            strings(&texts(out.clone())),
            ["abc", "defg", "hij", "", "k"]
        );
        // The rest of a line that went out unfinished does not make an
        // empty line of its own.
        split.push(b"\nl\n", &mut out);
        assert_eq!(strings(&texts(out[5..].to_vec())), ["l"]);
        // Every entry says where it starts and where the next one does.
        let at: Vec<_> = out
            .iter()
            .map(|p| (p.offset, p.next_offset, p.partial))
            .collect();
        assert_eq!(
            at,
            [
                (0, 4, false),
                (4, 8, true),
                (8, 12, false),
                (12, 13, false),
                (13, 14, false),
                (15, 17, false)
            ]
        );
    }

    async fn collect(mut rx: mpsc::Receiver<Result<LogEntry, Status>>) -> Vec<String> {
        let mut got = Vec::new();
        while let Some(entry) = rx.recv().await {
            let entry = entry.unwrap();
            assert_eq!(entry.source, Source::Unspecified as i32);
            got.push(String::from_utf8(entry.line).unwrap());
        }
        got
    }

    #[tokio::test]
    async fn a_stream_without_follow_ends_at_the_end() {
        let dir = tempfile::tempdir().unwrap();
        let repository = ContainerRepository::connect(dir.path()).await.unwrap();
        let path = dir.path().join("container.log");
        std::fs::write(&path, "a\nb\nunfinished").unwrap();
        let (tx, rx) = mpsc::channel(4);
        let opened = open(&path, &tail_of_lines(2, false)).unwrap();
        tokio::spawn(stream(
            opened,
            false,
            "main".to_string(),
            path,
            repository,
            tx,
        ));
        assert_eq!(collect(rx).await, ["b", "unfinished"]);
    }

    #[tokio::test]
    async fn a_follow_ends_when_the_log_goes() {
        let dir = tempfile::tempdir().unwrap();
        let repository = ContainerRepository::connect(dir.path()).await.unwrap();
        let path = dir.path().join("container.log");
        std::fs::write(&path, "old\n").unwrap();
        let (tx, mut rx) = mpsc::channel(4);
        let opened = open(&path, &tail_of_lines(0, true)).unwrap();
        tokio::spawn(stream(
            opened,
            true,
            "main".to_string(),
            path.clone(),
            repository,
            tx,
        ));
        let next = |entry: Option<Result<LogEntry, Status>>| {
            String::from_utf8(entry.unwrap().unwrap().line).unwrap()
        };
        assert_eq!(next(rx.recv().await), "old");
        let mut file = std::fs::OpenOptions::new()
            .append(true)
            .open(&path)
            .unwrap();
        file.write_all(b"new\npart").unwrap();
        assert_eq!(next(rx.recv().await), "new");
        // An unfinished line goes out once it waited.
        let started = Instant::now();
        assert_eq!(next(rx.recv().await), "part");
        assert!(started.elapsed() >= IDLE_FLUSH - POLL);
        // Written before the container went, and the container going.
        file.write_all(b"last\n").unwrap();
        std::fs::remove_file(&path).unwrap();
        assert_eq!(collect(rx).await, ["last"]);
    }

    #[tokio::test]
    async fn a_follow_waits_for_the_log_and_ends_with_the_container() {
        use crate::persistence::{ContainerRecord, ContainerStatus};
        use feos_proto::container_service::{ContainerConfig, ContainerState};
        let dir = tempfile::tempdir().unwrap();
        let repository = ContainerRepository::connect(dir.path()).await.unwrap();
        let record = ContainerRecord {
            container_id: "main".to_string(),
            image_uuid: uuid::Uuid::nil(),
            status: ContainerStatus {
                state: ContainerState::PullingImage,
                process_id: None,
                exit_code: None,
            },
            config: ContainerConfig::default(),
        };
        repository.save_container(&record).await.unwrap();
        let path = dir.path().join("main").join("container.log");
        let follow = || {
            let (tx, rx) = mpsc::channel(4);
            let opened = open(&path, &tail_of_lines(0, true)).unwrap();
            tokio::spawn(stream(
                opened,
                true,
                "main".to_string(),
                path.clone(),
                repository.clone(),
                tx,
            ));
            rx
        };
        // The log comes once the container is created.
        let mut rx = follow();
        std::fs::write(&path, "first\n").unwrap();
        let entry = rx.recv().await.unwrap().unwrap();
        assert_eq!(entry.line, b"first");
        std::fs::remove_file(&path).unwrap();
        assert!(rx.recv().await.is_none());
        // A container deleted before it had a log.
        let rx = follow();
        repository.delete_container("main").await.unwrap();
        assert!(collect(rx).await.is_empty());
    }

    #[tokio::test]
    async fn a_follow_ends_when_the_caller_goes() {
        let dir = tempfile::tempdir().unwrap();
        let repository = ContainerRepository::connect(dir.path()).await.unwrap();
        let path = dir.path().join("container.log");
        std::fs::write(&path, "").unwrap();
        let (tx, rx) = mpsc::channel(4);
        let opened = open(&path, &tail_of_lines(0, false)).unwrap();
        let task = tokio::spawn(stream(
            opened,
            true,
            "main".to_string(),
            path,
            repository,
            tx,
        ));
        drop(rx);
        tokio::time::timeout(Duration::from_secs(5), task)
            .await
            .expect("the follow did not end")
            .unwrap();
    }
}
