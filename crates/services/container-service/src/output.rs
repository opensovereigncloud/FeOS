// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! A container's output (ADR 0026): its stdout and stderr are FIFOs in its
//! log directory, `logs/stdout` and `logs/stderr`, which the task service
//! opens for the container's process. This process, the guest's PID 1 or
//! feosd, reads them and writes every line to `logs/container.log` in the
//! CRI format (crate::cri), with the time it read the line; a line over
//! [`MAX_LINE`] goes in parts of that many bytes, `P`, and one left
//! unfinished for [`IDLE_FLUSH`] goes as it is, a `P` too, which the next
//! entry of its stream goes on with. The log gets its ID before it is
//! made.
//!
//! The FIFOs are opened for reading and writing, so that a read never sees
//! their end, and the task service keeps them open as well while the
//! container is there: a container never writes into a FIFO without a
//! reader, whatever restarts. A guest that upgrades itself execs, and the
//! one it becomes reads them again when its dispatcher starts
//! (`Outputs::start` of every container the runtime knows); meanwhile the
//! pipe holds what the container writes, and the container waits once it
//! is full. A suspended pod's reader is in its snapshot as it was. Only the
//! part of an unfinished line this process had read is lost when it
//! execs. A write to the log that fails is logged and the line dropped:
//! the container is never held up by its log.

use crate::cri;
use crate::logs::{IDLE_FLUSH, MAX_LINE};
use crate::runtime::bundle;
use feos_proto::container_service::log_entry::Source;
use log::{info, warn};
use nix::sys::stat::Mode;
use std::collections::HashMap;
use std::fs::OpenOptions;
use std::io::{self, ErrorKind};
use std::os::fd::AsFd;
use std::os::unix::fs::{FileExt, FileTypeExt};
use std::path::{Path, PathBuf};
use std::sync::{Arc, Mutex};
use std::time::{Duration, SystemTime};
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::unix::pipe;
use tokio::sync::{mpsc, oneshot};
use tokio::task::AbortHandle;
use tokio::time::{sleep_until, timeout, Instant};

/// What one read takes from a FIFO: a pipe's buffer.
const READ: usize = 64 * 1024;
/// How long a reader waits after a read failed.
const READ_RETRY: Duration = Duration::from_secs(1);
/// How often a failing write to the log is said.
const WRITE_WARN: Duration = Duration::from_secs(60);
/// How long the end of an exited container's output may take to be
/// written.
const FINISH_TIMEOUT: Duration = Duration::from_secs(5);

/// The FIFOs of container `id`: its stdout's and its stderr's.
pub fn fifos(id: &str) -> (PathBuf, PathBuf) {
    let dir = bundle::container_dir(id).join("logs");
    (dir.join("stdout"), dir.join("stderr"))
}

/// A container's reader.
struct Reader {
    task: AbortHandle,
    /// Asks it to write what the FIFOs hold and the unfinished lines.
    finish: mpsc::Sender<oneshot::Sender<()>>,
}

/// The readers of the containers' output, by container.
#[derive(Clone, Default)]
pub struct Outputs {
    running: Arc<Mutex<HashMap<String, Reader>>>,
}

impl Outputs {
    fn running(&self) -> std::sync::MutexGuard<'_, HashMap<String, Reader>> {
        self.running.lock().unwrap_or_else(|e| e.into_inner())
    }

    /// Reads the output of container `id` from here on, unless it is read
    /// already: makes its FIFOs, and its log's ID, if they are not there.
    pub async fn start(&self, id: &str) -> Result<(), String> {
        if self
            .running()
            .get(id)
            .is_some_and(|reader| !reader.task.is_finished())
        {
            return Ok(());
        }
        let (stdout, stderr) = fifos(id);
        let log_path = bundle::log_file(id);
        let opened = {
            let (stdout, stderr, log_path) = (stdout.clone(), stderr.clone(), log_path.clone());
            tokio::task::spawn_blocking(move || prepare(&stdout, &stderr, &log_path))
                .await
                .map_err(|e| format!("output of {id}: {e}"))?
                .map_err(|e| format!("output of {id}: {e}"))?
        };
        let open = |path: &Path| {
            pipe::OpenOptions::new()
                .read_write(true)
                .open_receiver(path)
                .map_err(|e| format!("{}: {e}", path.display()))
        };
        let (out, err) = (open(&stdout)?, open(&stderr)?);
        let log = tokio::fs::File::from_std(opened);
        let mut running = self.running();
        if running
            .get(id)
            .is_some_and(|reader| !reader.task.is_finished())
        {
            return Ok(());
        }
        let (finish, finishes) = mpsc::channel(4);
        let task = tokio::spawn(copy(id.to_string(), out, err, log, finishes));
        let reader = Reader {
            task: task.abort_handle(),
            finish,
        };
        running.insert(id.to_string(), reader);
        Ok(())
    }

    /// Writes what container `id` wrote before its process exited: what
    /// its FIFOs hold, and its unfinished lines, at once. Waits
    /// [`FINISH_TIMEOUT`] at most.
    pub async fn finish(&self, id: &str) {
        let Some(finish) = self.running().get(id).map(|reader| reader.finish.clone()) else {
            return;
        };
        let (done, written) = oneshot::channel();
        if finish.send(done).await.is_err() {
            return;
        }
        if timeout(FINISH_TIMEOUT, written).await.is_err() {
            warn!("Output: The end of container {id}'s output was not written in time");
        }
    }

    /// Stops reading the output of container `id`, which is deleted.
    pub fn stop(&self, id: &str) {
        if let Some(reader) = self.running().remove(id) {
            reader.task.abort();
        }
    }
}

/// Makes the FIFOs and the log's ID, and opens the log for appending, its
/// last line ended: one that a crash or a guest of before left unfinished
/// does not run into the next.
fn prepare(stdout: &Path, stderr: &Path, log_path: &Path) -> io::Result<std::fs::File> {
    if let Some(dir) = log_path.parent() {
        std::fs::create_dir_all(dir)?;
    }
    for fifo in [stdout, stderr] {
        match std::fs::symlink_metadata(fifo) {
            Ok(meta) if meta.file_type().is_fifo() => continue,
            Ok(_) => std::fs::remove_file(fifo)?,
            Err(e) if e.kind() == ErrorKind::NotFound => {}
            Err(e) => return Err(e),
        }
        nix::unistd::mkfifo(fifo, Mode::S_IRUSR | Mode::S_IWUSR).map_err(io::Error::from)?;
    }
    cri::ensure_id(log_path)?;
    let log = OpenOptions::new()
        .create(true)
        .read(true)
        .append(true)
        .open(log_path)?;
    let len = log.metadata()?.len();
    if len > 0 {
        let mut last = [0u8; 1];
        log.read_exact_at(&mut last, len - 1)?;
        if last[0] != b'\n' {
            io::Write::write_all(&mut &log, b"\n")?;
        }
    }
    Ok(log)
}

/// The lines of one stream, as they come.
pub(crate) struct Assembler {
    source: Source,
    max: usize,
    /// The part of a line read so far.
    pending: Vec<u8>,
}

impl Assembler {
    pub(crate) fn new(source: Source, max: usize) -> Self {
        Self {
            source,
            max: max.max(1),
            pending: Vec::new(),
        }
    }

    /// Adds bytes read at `time`, writing the entries they finish to `out`:
    /// a line at its newline, a part once `max` bytes of a line came and
    /// more of it follows.
    pub(crate) fn push(&mut self, mut data: &[u8], time: SystemTime, out: &mut Vec<u8>) {
        while !data.is_empty() {
            let room = self.max - self.pending.len();
            // A newline right after a full part ends the line there.
            let window = &data[..(room + 1).min(data.len())];
            match window.iter().position(|&b| b == b'\n') {
                Some(i) => {
                    self.pending.extend_from_slice(&window[..i]);
                    cri::write(out, time, self.source, false, &self.pending);
                    self.pending.clear();
                    data = &data[i + 1..];
                }
                None if data.len() > room => {
                    self.pending.extend_from_slice(&data[..room]);
                    cri::write(out, time, self.source, true, &self.pending);
                    self.pending.clear();
                    data = &data[room..];
                }
                None => {
                    self.pending.extend_from_slice(data);
                    data = &[];
                }
            }
        }
    }

    /// Whether part of a line waits for its end.
    pub(crate) fn has_pending(&self) -> bool {
        !self.pending.is_empty()
    }

    /// Writes the unfinished line as a part, which the next entry of the
    /// stream goes on with.
    pub(crate) fn flush(&mut self, time: SystemTime, out: &mut Vec<u8>) {
        if !self.pending.is_empty() {
            cri::write(out, time, self.source, true, &self.pending);
            self.pending.clear();
        }
    }
}

/// One stream's FIFO and lines.
struct Stream {
    fifo: pipe::Receiver,
    lines: Assembler,
    buf: Vec<u8>,
    /// When its unfinished line goes as it is.
    flush_at: Option<Instant>,
    /// It failed to read until then.
    paused: Option<Instant>,
}

impl Stream {
    fn new(fifo: pipe::Receiver, source: Source) -> Self {
        Self {
            fifo,
            lines: Assembler::new(source, MAX_LINE),
            buf: vec![0u8; READ],
            flush_at: None,
            paused: None,
        }
    }

    /// Takes what a read got, putting the entries it finishes into `out`.
    fn take(&mut self, id: &str, read: io::Result<usize>, out: &mut Vec<u8>) {
        match read {
            // A FIFO open for writing here too has no end; 0 is not
            // expected, and waits like a failure.
            Ok(0) => self.paused = Some(Instant::now() + READ_RETRY),
            Ok(n) => {
                self.paused = None;
                self.lines.push(&self.buf[..n], SystemTime::now(), out);
                self.flush_at = self
                    .lines
                    .has_pending()
                    .then(|| Instant::now() + IDLE_FLUSH);
            }
            Err(e) => {
                warn!("Output: Reading the output of container {id} failed: {e}");
                self.paused = Some(Instant::now() + READ_RETRY);
            }
        }
    }

    fn flush(&mut self, out: &mut Vec<u8>) {
        self.lines.flush(SystemTime::now(), out);
        self.flush_at = None;
    }

    /// Takes what the FIFO holds now, and the unfinished line.
    fn drain(&mut self, id: &str, out: &mut Vec<u8>) {
        // Read as it is, not as the runtime last saw it ready.
        loop {
            match nix::unistd::read(self.fifo.as_fd(), &mut self.buf) {
                Ok(0) => break,
                Ok(n) => self.lines.push(&self.buf[..n], SystemTime::now(), out),
                Err(nix::errno::Errno::EAGAIN) => break,
                Err(nix::errno::Errno::EINTR) => continue,
                Err(e) => {
                    warn!("Output: Reading the output of container {id} failed: {e}");
                    break;
                }
            }
        }
        self.flush(out);
    }
}

/// Reads from `fifo`, once `paused` passed.
async fn read(
    fifo: &mut pipe::Receiver,
    buf: &mut [u8],
    paused: Option<Instant>,
) -> io::Result<usize> {
    if let Some(until) = paused {
        sleep_until(until).await;
    }
    fifo.read(buf).await
}

/// Waits until `at`, if it is set.
async fn due(at: Option<Instant>) {
    match at {
        Some(at) => sleep_until(at).await,
        None => std::future::pending().await,
    }
}

/// What a stream's reader came to.
enum Event {
    Read(Source, io::Result<usize>),
    Due(Source),
    /// The container exited: what it wrote goes now.
    Finish(oneshot::Sender<()>),
}

/// Copies the container's output into its log, until it is aborted.
async fn copy(
    id: String,
    stdout: pipe::Receiver,
    stderr: pipe::Receiver,
    mut log: tokio::fs::File,
    mut finishes: mpsc::Receiver<oneshot::Sender<()>>,
) {
    info!("Output: Reading the output of container {id}");
    let mut out = Stream::new(stdout, Source::Stdout);
    let mut err = Stream::new(stderr, Source::Stderr);
    let mut entries = Vec::new();
    let mut warned: Option<Instant> = None;
    loop {
        let event = tokio::select! {
            read = read(&mut out.fifo, &mut out.buf, out.paused) => Event::Read(Source::Stdout, read),
            read = read(&mut err.fifo, &mut err.buf, err.paused) => Event::Read(Source::Stderr, read),
            () = due(out.flush_at) => Event::Due(Source::Stdout),
            () = due(err.flush_at) => Event::Due(Source::Stderr),
            Some(done) = finishes.recv() => Event::Finish(done),
        };
        let mut finished = None;
        match event {
            Event::Read(Source::Stderr, read) => err.take(&id, read, &mut entries),
            Event::Read(_, read) => out.take(&id, read, &mut entries),
            Event::Due(Source::Stderr) => err.flush(&mut entries),
            Event::Due(_) => out.flush(&mut entries),
            Event::Finish(done) => {
                out.drain(&id, &mut entries);
                err.drain(&id, &mut entries);
                finished = Some(done);
            }
        }
        if entries.is_empty() {
            if let Some(done) = finished {
                let _ = done.send(());
            }
            continue;
        }
        let written = match log.write_all(&entries).await {
            Ok(()) => log.flush().await,
            Err(e) => Err(e),
        };
        if let Err(e) = written
            && warned.is_none_or(|at| at.elapsed() >= WRITE_WARN)
        {
            warn!("Output: Writing the log of container {id} failed: {e}; its lines are dropped");
            warned = Some(Instant::now());
        }
        entries.clear();
        if let Some(done) = finished {
            let _ = done.send(());
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::logs::Lines;
    use std::time::UNIX_EPOCH;

    /// The entries `chunks` make, read back.
    fn entries(chunks: &[&[u8]], max: usize, flush: bool) -> Vec<(String, bool)> {
        let mut lines = Assembler::new(Source::Stderr, max);
        let mut out = Vec::new();
        for chunk in chunks {
            lines.push(chunk, UNIX_EPOCH, &mut out);
        }
        if flush {
            lines.flush(UNIX_EPOCH, &mut out);
        }
        out.split(|&b| b == b'\n')
            .filter(|line| !line.is_empty())
            .map(|line| {
                let record = cri::parse(line).unwrap();
                assert_eq!(record.source, Source::Stderr);
                (
                    String::from_utf8(record.text.to_vec()).unwrap(),
                    record.partial,
                )
            })
            .collect()
    }

    fn full(text: &str) -> (String, bool) {
        (text.to_string(), false)
    }

    fn part(text: &str) -> (String, bool) {
        (text.to_string(), true)
    }

    #[test]
    fn lines_become_entries_across_reads() {
        assert_eq!(
            entries(&[b"one\ntw", b"o\n\nthr"], 64, false),
            [full("one"), full("two"), full("")]
        );
        assert_eq!(
            entries(&[b"one\ntw", b"o\n\nthr"], 64, true),
            [full("one"), full("two"), full(""), part("thr")]
        );
        // A flushed part, and the rest of its line after it.
        let mut lines = Assembler::new(Source::Stdout, 64);
        let mut out = Vec::new();
        lines.push(b"progress", UNIX_EPOCH, &mut out);
        assert!(lines.has_pending());
        lines.flush(UNIX_EPOCH, &mut out);
        assert!(!lines.has_pending());
        lines.push(b" done\n", UNIX_EPOCH, &mut out);
        assert_eq!(
            String::from_utf8(out).unwrap(),
            "1970-01-01T00:00:00.000000000Z stdout P progress\n\
             1970-01-01T00:00:00.000000000Z stdout F  done\n"
        );
    }

    #[test]
    fn long_lines_go_in_parts() {
        assert_eq!(
            entries(&[b"abcdefghij\n"], 4, false),
            [part("abcd"), part("efgh"), full("ij")]
        );
        // A line of exactly the most is one entry, also when its newline
        // comes in the next read.
        assert_eq!(
            entries(&[b"abcd\nx\n"], 4, false),
            [full("abcd"), full("x")]
        );
        assert_eq!(entries(&[b"abcd", b"\n"], 4, false), [full("abcd")]);
        assert_eq!(
            entries(&[b"abcd", b"e\n"], 4, false),
            [part("abcd"), full("e")]
        );
        assert_eq!(entries(&[b"abcdefgh"], 4, false), [part("abcd")]);
    }

    #[test]
    fn the_reader_gives_back_what_the_container_wrote() {
        // Written by the assembler, cut by the reader: the text, with
        // every part where the line goes on.
        let mut text = vec![b'z'; 3 * MAX_LINE + 17];
        text.extend(b"\nshort\n\nlast");
        let mut lines = Assembler::new(Source::Stdout, MAX_LINE);
        let mut out = Vec::new();
        for chunk in text.chunks(5000) {
            lines.push(chunk, SystemTime::now(), &mut out);
        }
        lines.flush(SystemTime::now(), &mut out);
        let mut reader = Lines::at(MAX_LINE, 0, 0, false);
        let mut parts = Vec::new();
        reader.push(&out, &mut parts);
        let mut back = Vec::new();
        for part in &parts {
            assert_eq!(part.source, Source::Stdout);
            assert!(part.time_unix_nano > 0);
            back.extend_from_slice(&part.text);
            if !part.partial {
                back.push(b'\n');
            }
        }
        assert_eq!(parts.len(), 7);
        assert_eq!(back, [&text[..], b""].concat());
        assert_eq!(parts.last().unwrap().next_offset, out.len() as u64);
    }

    #[tokio::test]
    async fn a_container_writes_through_its_fifos() {
        let dir = tempfile::tempdir().unwrap();
        let (stdout, stderr, log) = (
            dir.path().join("stdout"),
            dir.path().join("stderr"),
            dir.path().join("container.log"),
        );
        // A log a crash left unfinished.
        std::fs::write(&log, b"cut off").unwrap();
        let file = prepare(&stdout, &stderr, &log).unwrap();
        let id = cri::read_id(&log);
        assert_eq!(id.len(), 32);
        assert_eq!(std::fs::read(&log).unwrap(), b"cut off\n");
        let open = |path: &Path| {
            pipe::OpenOptions::new()
                .read_write(true)
                .open_receiver(path)
                .unwrap()
        };
        let (finish, finishes) = mpsc::channel(1);
        let task = tokio::spawn(copy(
            "main".to_string(),
            open(&stdout),
            open(&stderr),
            tokio::fs::File::from_std(file),
            finishes,
        ));
        // Made again, the FIFOs and the ID stay.
        prepare(&stdout, &stderr, &log).unwrap();
        assert_eq!(cri::read_id(&log), id);
        let write = |path: &Path, data: &[u8]| {
            let mut fifo = OpenOptions::new().write(true).open(path).unwrap();
            io::Write::write_all(&mut fifo, data).unwrap();
        };
        write(&stdout, b"to stdout\n");
        write(&stderr, b"to stderr\nunfinished");
        // Its exit: written at once, the unfinished line too.
        let (done, written) = oneshot::channel();
        finish.send(done).await.unwrap();
        written.await.unwrap();
        let content = std::fs::read(&log).unwrap();
        assert_eq!(
            content
                .split(|&b| b == b'\n')
                .filter_map(cri::parse)
                .count(),
            3
        );
        write(&stdout, b"more\n");
        let deadline = Instant::now() + Duration::from_secs(10);
        let lines = loop {
            let content = std::fs::read(&log).unwrap();
            let lines: Vec<_> = content
                .split(|&b| b == b'\n')
                .filter_map(cri::parse)
                .map(|r| {
                    (
                        r.source,
                        String::from_utf8_lossy(r.text).into_owned(),
                        r.partial,
                    )
                })
                .collect();
            if lines.len() == 4 || Instant::now() > deadline {
                break lines;
            }
            tokio::time::sleep(Duration::from_millis(50)).await;
        };
        task.abort();
        let mut lines = lines;
        lines.sort();
        assert_eq!(
            lines,
            [
                (Source::Stdout, "more".to_string(), false),
                (Source::Stdout, "to stdout".to_string(), false),
                (Source::Stderr, "to stderr".to_string(), false),
                (Source::Stderr, "unfinished".to_string(), true),
            ]
        );
    }
}
