// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

use chrono::{DateTime, Utc};
use log::{Level, LevelFilter, Log, Metadata, Record, SetLoggerError};
use std::collections::VecDeque;
use std::fmt;
use std::io::Write;
use std::sync::mpsc::sync_channel;
use std::time::Duration;
use termcolor::{Color, ColorChoice, ColorSpec, StandardStream, WriteColor};
use tokio::sync::{broadcast, mpsc, oneshot};

#[derive(Clone, Debug)]
pub struct LogEntry {
    pub seq: u64,
    pub timestamp: DateTime<Utc>,
    pub level: Level,
    pub target: String,
    pub message: String,
}

impl fmt::Display for LogEntry {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(
            f,
            "[{} {:<5} {}] {}",
            self.timestamp.format("%Y-%m-%d %H:%M:%S%.3f"),
            self.level,
            self.target,
            self.message
        )
    }
}

#[derive(Clone)]
pub struct LogHandle {
    history_requester: mpsc::Sender<HistoryRequest>,
    broadcast_sender: broadcast::Sender<LogEntry>,
}

pub struct LogReader {
    history_snapshot: VecDeque<LogEntry>,
    receiver: broadcast::Receiver<LogEntry>,
}

pub struct Builder {
    filter: LevelFilter,
    max_history: usize,
    broadcast_capacity: usize,
    mpsc_capacity: usize,
    log_to_stdout: bool,
}

impl Default for Builder {
    fn default() -> Self {
        Self {
            filter: LevelFilter::Info,
            max_history: 1000,
            broadcast_capacity: 1024,
            mpsc_capacity: 4096,
            log_to_stdout: true,
        }
    }
}

impl Builder {
    pub fn new() -> Self {
        Self::default()
    }

    pub fn filter_level(mut self, level: LevelFilter) -> Self {
        self.filter = level;
        self
    }

    pub fn max_history(mut self, size: usize) -> Self {
        self.max_history = size;
        self
    }

    pub fn log_to_stdout(mut self, enabled: bool) -> Self {
        self.log_to_stdout = enabled;
        self
    }

    pub fn init(self) -> Result<LogHandle, SetLoggerError> {
        let (log_tx, log_rx) = mpsc::channel::<LogMessage>(self.mpsc_capacity);
        let (history_tx, history_rx) = mpsc::channel(32);
        let (broadcast_tx, _) = broadcast::channel(self.broadcast_capacity);

        let logger_frontend = FeosLogger {
            sender: log_tx,
            filter: self.filter,
        };

        let actor = LoggerActor {
            log_receiver: log_rx,
            history_requester: history_rx,
            broadcast_sender: broadcast_tx.clone(),
            history: VecDeque::with_capacity(self.max_history),
            max_history: self.max_history,
            seq_counter: 0,
            log_to_stdout: self.log_to_stdout,
            stdout_writer: StandardStream::stdout(ColorChoice::Auto),
        };

        tokio::spawn(actor.run());

        let handle = LogHandle {
            history_requester: history_tx,
            broadcast_sender: broadcast_tx,
        };

        log::set_boxed_logger(Box::new(logger_frontend))?;
        log::set_max_level(self.filter);

        Ok(handle)
    }
}

impl LogHandle {
    pub async fn new_reader(&self) -> Result<LogReader, &'static str> {
        let (resp_tx, resp_rx) = oneshot::channel();
        if self.history_requester.send(resp_tx).await.is_err() {
            return Err("Logger actor has shut down");
        }

        let history_snapshot = match resp_rx.await {
            Ok(history) => history,
            Err(_) => return Err("Failed to receive history from logger actor"),
        };

        let receiver = self.broadcast_sender.subscribe();

        Ok(LogReader {
            history_snapshot,
            receiver,
        })
    }
}

impl LogReader {
    pub async fn next(&mut self) -> Option<LogEntry> {
        if let Some(entry) = self.history_snapshot.pop_front() {
            return Some(entry);
        }

        match self.receiver.recv().await {
            Ok(entry) => Some(entry),
            Err(broadcast::error::RecvError::Lagged(_)) => {
                eprintln!(
                    "[LOG READER WARNING] Reader lagged and missed messages. Closing stream."
                );
                None
            }
            Err(broadcast::error::RecvError::Closed) => None,
        }
    }
}

type HistoryRequest = oneshot::Sender<VecDeque<LogEntry>>;

/// How long a flush waits for the actor, which may run on the thread that
/// waits: then it writes the records later, or never.
const FLUSH_TIMEOUT: Duration = Duration::from_secs(1);

enum LogMessage {
    Record {
        level: Level,
        target: String,
        message: String,
    },
    /// Answered once the records before it are written.
    Flush(std::sync::mpsc::SyncSender<()>),
}

struct FeosLogger {
    sender: mpsc::Sender<LogMessage>,
    filter: LevelFilter,
}

impl Log for FeosLogger {
    fn enabled(&self, metadata: &Metadata) -> bool {
        metadata.level() <= self.filter
    }

    fn log(&self, record: &Record) {
        if !self.enabled(record.metadata()) {
            return;
        }

        let msg = LogMessage::Record {
            level: record.level(),
            target: record.target().to_string(),
            message: format!("{}", record.args()),
        };

        if self.sender.try_send(msg).is_err() {
            eprintln!("[LOGGER WARNING] Log channel is full. Dropping log message.");
        }
    }

    /// Waits until the actor has written what was logged before, as an
    /// exec, which would drop it, needs.
    fn flush(&self) {
        let (done, written) = sync_channel(1);
        if self.sender.try_send(LogMessage::Flush(done)).is_ok() {
            let _ = written.recv_timeout(FLUSH_TIMEOUT);
        }
    }
}

struct LoggerActor {
    log_receiver: mpsc::Receiver<LogMessage>,
    history_requester: mpsc::Receiver<HistoryRequest>,
    broadcast_sender: broadcast::Sender<LogEntry>,
    history: VecDeque<LogEntry>,
    max_history: usize,
    seq_counter: u64,
    log_to_stdout: bool,
    stdout_writer: StandardStream,
}

impl LoggerActor {
    async fn run(mut self) {
        loop {
            tokio::select! {
                Some(msg) = self.log_receiver.recv() => {
                    let (level, target, message) = match msg {
                        LogMessage::Record { level, target, message } => (level, target, message),
                        LogMessage::Flush(done) => {
                            let _ = self.stdout_writer.flush();
                            let _ = done.send(());
                            continue;
                        }
                    };
                    self.seq_counter += 1;

                    let entry = LogEntry {
                        seq: self.seq_counter,
                        timestamp: Utc::now(),
                        level,
                        target,
                        message,
                    };

                    if self.log_to_stdout {
                        let _ = self.write_log_entry_to_stdout(&entry);
                    }

                    self.history.push_back(entry.clone());
                    if self.history.len() > self.max_history {
                        self.history.pop_front();
                    }

                    let _ = self.broadcast_sender.send(entry);
                },

                Some(responder) = self.history_requester.recv() => {
                    let _ = responder.send(self.history.clone());
                },

                else => { break; }
            }
        }
    }

    fn write_log_entry_to_stdout(&mut self, entry: &LogEntry) -> std::io::Result<()> {
        let mut level_spec = ColorSpec::new();
        match entry.level {
            Level::Error => level_spec.set_fg(Some(Color::Red)).set_bold(true),
            Level::Warn => level_spec.set_fg(Some(Color::Yellow)).set_bold(true),
            Level::Info => level_spec.set_fg(Some(Color::Green)).set_bold(true),
            Level::Debug => level_spec.set_fg(Some(Color::Blue)).set_bold(true),
            Level::Trace => level_spec.set_fg(Some(Color::Magenta)).set_bold(true),
        };

        write!(
            &mut self.stdout_writer,
            "[{} ",
            entry.timestamp.format("%Y-%m-%dT%H:%M:%SZ")
        )?;

        self.stdout_writer.set_color(&level_spec)?;
        write!(&mut self.stdout_writer, "{:<5}", entry.level.to_string())?;

        self.stdout_writer.reset()?;
        writeln!(
            &mut self.stdout_writer,
            " {target}] {message}",
            target = entry.target,
            message = entry.message
        )?;
        Ok(())
    }
}
