// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

use thiserror::Error;

pub type Result<T> = std::result::Result<T, Error>;

#[derive(Debug, Error)]
pub enum Error {
    #[error("io: {0}")]
    Io(#[from] std::io::Error),

    #[error("manifest corrupt: {0}")]
    CorruptManifest(String),

    #[error("chunk {0} not present locally and no source given")]
    ChunkMissing(String),

    #[error("chunk {0} corrupt: hash mismatch after fetch")]
    ChunkCorrupt(String),

    #[error("s3: {0}")]
    S3(String),

    #[error("seal aborted at step {0}")]
    SealAborted(&'static str),

    #[error("head manifest: {0}")]
    Head(String),
}

impl From<object_store::Error> for Error {
    fn from(e: object_store::Error) -> Self {
        Error::S3(e.to_string())
    }
}
