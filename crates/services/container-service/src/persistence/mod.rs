// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

use feos_proto::container_service::{ContainerConfig, ContainerState};
use uuid::Uuid;

pub mod repository;

#[derive(Debug, thiserror::Error)]
pub enum PersistenceError {
    #[error("Failed to access a container record: {0}")]
    Io(#[from] std::io::Error),

    #[error("Malformed container record: {0}")]
    Format(#[from] serde_json::Error),

    #[error("Invalid state string '{0}' in a container record")]
    InvalidStateString(String),

    #[error("Invalid UUID '{0}' in a container record")]
    InvalidUuid(String),
}

#[derive(Debug, Clone)]
pub struct ContainerStatus {
    pub state: ContainerState,
    pub process_id: Option<i64>,
    /// How the container's process ended, 128 + the signal when signalled:
    /// set when the service saw it exit, cleared when it starts again or
    /// is adopted.
    pub exit_code: Option<i32>,
}

#[derive(Debug, Clone)]
pub struct ContainerRecord {
    pub container_id: String,
    pub image_uuid: Uuid,
    pub status: ContainerStatus,
    pub config: ContainerConfig,
}
