// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! A container's log, `logs/container.log` in its directory, as
//! `StreamContainerLogs` serves it.
//!
//! The task service appends the container's stdout and stderr to the one
//! file, so a line's source is not known. A line goes out without its
//! newline; one over [`MAX_LINE`] bytes goes out in parts of at most that
//! many, counted from where the line starts. `tail_lines` counts the lines
//! of the log, each as many lines as it has parts. A tail is found reading
//! back from the end in blocks, never the whole file: a line that reaches
//! further back than the parts asked for could fill is cut, so that its
//! last parts are a full [`MAX_LINE`] each. Nor does a tail reach back
//! further than [`MAX_TAIL_BYTES`] from the end, however few lines that
//! is, so it never sends more bytes than that: a line that starts further
//! back is cut the same way, to its last whole parts within those bytes,
//! or left out when not one part fits.
//!
//! With `follow`, the stream polls the file, which is only ever appended
//! to, and ends when the container is deleted (its log file goes, or
//! another one takes its place) or the caller ends the call. A line that
//! stays unfinished for [`IDLE_FLUSH`] goes out as it is.

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

/// How many entries a line of `len` bytes goes out as.
fn parts(len: u64, max: u64) -> u64 {
    len.div_ceil(max).max(1)
}

/// Where the last `lines` lines of a log of `len` bytes start, and how many
/// leading parts of the line there to leave out. Reads back from the end in
/// blocks of `block` bytes, never further than `bytes` from the end; a line
/// counts as many lines as it has parts of at most `max` bytes. A line
/// whose start lies further back than `lines` parts reach is cut to its
/// last `lines` × `max` bytes; one whose start lies further back than
/// `bytes` reach is cut to its last whole parts within them, or left out
/// when not one fits.
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
            if let Some(found) = settle(line_start, line_end, need, max) {
                return Ok(found);
            }
            need -= parts(line_end - line_start, max);
            line_end = line_start - 1;
            k = q;
        }
        // The line reaches below this block: is it already long enough?
        if line_end - lo > need * max {
            return Ok((line_end - need * max, 0));
        }
        hi = lo;
    }
    if floor == 0 {
        return Ok(settle(0, line_end, need, max).unwrap_or((0, 0)));
    }
    // The bytes end inside the line looked at, or just before it starts.
    let mut before = [0u8; 1];
    file.read_exact_at(&mut before, floor - 1)?;
    if before[0] == b'\n' {
        return Ok(settle(floor, line_end, need, max).unwrap_or((floor, 0)));
    }
    // It started further back: its last whole parts within the bytes, at
    // most need of them (more would have been settled above); without one,
    // the tail starts after it.
    let whole = (line_end - floor) / max;
    if whole > 0 {
        return Ok((line_end - whole * max, 0));
    }
    Ok(((line_end + 1).min(len), 0))
}

/// Where the tail starts if the line from `start` to `end` holds the last
/// of the `need` lines still wanted.
fn settle(start: u64, end: u64, need: u64, max: u64) -> Option<(u64, u64)> {
    let len = end - start;
    if len > need * max {
        return Some((end - need * max, 0));
    }
    let parts = parts(len, max);
    (parts >= need).then(|| (start, parts - need))
}

/// Cuts bytes into the entries of a log: at each newline, which is left
/// out, and after `max` bytes of a line.
pub(crate) struct Lines {
    max: usize,
    /// The part of a line read so far.
    pending: Vec<u8>,
    /// The line being read went out in part already: its newline does not
    /// end an empty line.
    parted: bool,
    /// Entries still to leave out, from the start of a tail.
    skip: u64,
}

impl Lines {
    pub(crate) fn new(max: usize, skip: u64) -> Self {
        Self {
            max: max.max(1),
            pending: Vec::new(),
            parted: false,
            skip,
        }
    }

    fn emit(&mut self, out: &mut Vec<Vec<u8>>) {
        let line = std::mem::take(&mut self.pending);
        if self.skip > 0 {
            self.skip -= 1;
        } else {
            out.push(line);
        }
    }

    /// Adds bytes, putting the entries they finish into `out`.
    pub(crate) fn push(&mut self, mut data: &[u8], out: &mut Vec<Vec<u8>>) {
        while !data.is_empty() {
            let room = self.max - self.pending.len();
            let window = &data[..room.min(data.len())];
            match window.iter().position(|&b| b == b'\n') {
                Some(i) => {
                    self.pending.extend_from_slice(&window[..i]);
                    if !self.pending.is_empty() || !self.parted {
                        self.emit(out);
                    }
                    self.parted = false;
                    data = &data[i + 1..];
                }
                None => {
                    self.pending.extend_from_slice(window);
                    data = &data[window.len()..];
                    if self.pending.len() == self.max {
                        self.emit(out);
                        self.parted = true;
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
    pub(crate) fn flush(&mut self, out: &mut Vec<Vec<u8>>) {
        if !self.pending.is_empty() {
            self.emit(out);
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
            offset: 0,
            lines: Lines::new(MAX_LINE, 0),
        }))
    }

    /// Reads on from where it was, up to `end` or the end of the file, at
    /// most [`READ_CHUNK`] bytes. Returns the entries finished and the
    /// bytes read; none at the end.
    fn read(&mut self, end: Option<u64>) -> io::Result<(Vec<Vec<u8>>, usize)> {
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

/// Opens the log at `path` and finds where the stream starts: the last
/// `tail_lines` lines (capped at [`MAX_TAIL_LINES`] and
/// [`MAX_TAIL_BYTES`]) when above 0, else the whole log with `tail`, else
/// its end.
pub(crate) fn open(path: &Path, tail_lines: u32, tail: bool) -> io::Result<Opened> {
    let Some(mut log) = LogFile::open(path)? else {
        return Ok(Opened { log: None, end: 0 });
    };
    let len = log.file.metadata()?.len();
    let (start, skip) = match tail_lines.min(MAX_TAIL_LINES) {
        0 if tail => (0, 0),
        0 => (len, 0),
        lines => tail_start(
            &log.file,
            len,
            lines.into(),
            MAX_TAIL_BYTES,
            BLOCK,
            MAX_LINE,
        )?,
    };
    log.offset = start;
    log.lines = Lines::new(MAX_LINE, skip);
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

async fn send(tx: &LogSender, entries: Vec<Vec<u8>>) -> Result<(), End> {
    for line in entries {
        let entry = LogEntry {
            line,
            source: Source::Unspecified as i32,
            ..Default::default()
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
        let (read, entries, n) = blocking(move || {
            let (entries, n) = log.read(end)?;
            Ok((log, entries, n))
        })
        .await?;
        log = read;
        send(tx, entries).await?;
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
    send(tx, out).await
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
        let mut split = Lines::new(max, skip);
        let mut out = Vec::new();
        split.push(&content[start as usize..], &mut out);
        split.flush(&mut out);
        out
    }

    /// All entries of `content`, with parts of at most `max` bytes.
    fn entries(content: &[u8], max: usize) -> Vec<Vec<u8>> {
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
        let opened = open(file.path(), u32::MAX, false).unwrap();
        let mut log = opened.log.unwrap();
        let (got, _) = log.read(Some(opened.end)).unwrap();
        assert_eq!(got.len(), MAX_TAIL_LINES as usize);
        assert_eq!(got[0], b"5");
    }

    /// The entries a stream without follow sends for the log at `path`.
    fn read_all(path: &Path, tail_lines: u32, tail: bool) -> Vec<Vec<u8>> {
        let opened = open(path, tail_lines, tail).unwrap();
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
            let opened = open(file.path(), tail_lines, tail).unwrap();
            let mut log = opened.log.unwrap();
            let (got, _) = log.read(Some(opened.end)).unwrap();
            strings(&got)
        };
        assert_eq!(read(0, true), ["one", "two", "three"]);
        assert!(read(0, false).is_empty());
        assert_eq!(read(1, false), ["three"]);
        assert_eq!(read(2, true), ["two", "three"]);

        let missing = open(Path::new("/nonexistent/container.log"), 5, true).unwrap();
        assert!(missing.log.is_none());
    }

    #[test]
    fn lines_cut_across_reads() {
        let mut split = Lines::new(4, 0);
        let mut out = Vec::new();
        for chunk in [&b"ab"[..], b"c\nde", b"fghij", b"\n", b"\nk"] {
            split.push(chunk, &mut out);
        }
        assert_eq!(strings(&out), ["abc", "defg", "hij", ""]);
        assert!(split.has_pending());
        split.flush(&mut out);
        assert_eq!(strings(&out), ["abc", "defg", "hij", "", "k"]);
        // The rest of a line that went out unfinished does not make an
        // empty line of its own.
        split.push(b"\nl\n", &mut out);
        assert_eq!(strings(&out[5..]), ["l"]);
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
        let opened = open(&path, 2, false).unwrap();
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
        let opened = open(&path, 0, true).unwrap();
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
            let opened = open(&path, 0, true).unwrap();
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
        let opened = open(&path, 0, false).unwrap();
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
