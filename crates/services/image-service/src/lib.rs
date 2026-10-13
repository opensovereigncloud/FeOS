// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

use crate::error::ImageServiceError;
use feos_proto::image_service::{
    DeleteImageRequest, DeleteImageResponse, ImageInfo, ImageState, ImageStatusResponse,
    ListImagesRequest, ListImagesResponse, PullImageRequest, PullImageResponse,
    WatchImageStatusRequest,
};
use std::collections::HashMap;
use tokio::sync::{mpsc, oneshot};
use tonic::Status;
pub mod api;
pub mod dispatcher;
pub mod error;
pub mod filestore;
pub mod pull;
pub mod unpack;
pub mod worker;

pub const IMAGE_DIR: &str = "/var/lib/feos/images";

/// Where a pull of `image_uuid` writes before it is committed.
pub fn pull_dir(image_uuid: &str) -> std::path::PathBuf {
    std::path::Path::new(IMAGE_DIR).join(format!("{image_uuid}.tmp"))
}
pub const IMAGE_SERVICE_SOCKET: &str = "/run/feos/image.sock";

#[derive(Debug, Clone)]
pub struct ImageStateEvent {
    pub image_uuid: String,
    pub state: ImageState,
    pub message: String,
}

#[derive(Debug)]
pub enum Command {
    PullImage(
        PullImageRequest,
        oneshot::Sender<Result<PullImageResponse, ImageServiceError>>,
    ),
    WatchImageStatus(
        WatchImageStatusRequest,
        mpsc::Sender<Result<ImageStatusResponse, Status>>,
    ),
    ListImages(
        ListImagesRequest,
        oneshot::Sender<Result<ListImagesResponse, ImageServiceError>>,
    ),
    DeleteImage(
        DeleteImageRequest,
        oneshot::Sender<Result<DeleteImageResponse, ImageServiceError>>,
    ),
}

#[derive(Debug)]
pub enum OrchestratorCommand {
    PullImage {
        image_ref: String,
        exclusive: bool,
        responder: oneshot::Sender<Result<PullImageResponse, ImageServiceError>>,
    },
    FinalizePull {
        image_uuid: String,
        image_ref: String,
        pulled: pull::Pulled,
    },
    FailPull {
        image_uuid: String,
        error: ImageServiceError,
    },
    WatchImageStatus {
        image_uuid: String,
        stream_sender: mpsc::Sender<Result<ImageStatusResponse, Status>>,
    },
    ListImages {
        responder: oneshot::Sender<Result<ListImagesResponse, ImageServiceError>>,
    },
    DeleteImage {
        image_uuid: String,
        responder: oneshot::Sender<Result<DeleteImageResponse, ImageServiceError>>,
    },
}

#[derive(Debug)]
pub enum FileCommand {
    /// Commits a pull whose files are in [`pull_dir`].
    StoreImage {
        image_uuid: String,
        image_ref: String,
        pulled: pull::Pulled,
        responder: oneshot::Sender<Result<(), std::io::Error>>,
    },
    DeleteImage {
        image_uuid: String,
        responder: oneshot::Sender<Result<(), std::io::Error>>,
    },
    ScanExistingImages {
        responder: oneshot::Sender<HashMap<String, ImageInfo>>,
    },
}
