// SPDX-FileCopyrightText: 2023 SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

use crate::{FileCommand, ImageInfo, PulledImageData, IMAGE_DIR};
use feos_proto::image_service::ImageState;
use flate2::read::GzDecoder;
use log::{error, info, warn};
use oci_distribution::manifest;
use serde::{Deserialize, Serialize};
use std::collections::HashMap;
use std::io::Cursor;
use std::path::Path;
use tar::Archive;
use tokio::{fs, sync::mpsc};

const INITRAMFS_MEDIA_TYPE: &str = "application/vnd.ironcore.image.initramfs.v1alpha1.initramfs";
const VMLINUZ_MEDIA_TYPE: &str = "application/vnd.ironcore.image.vmlinuz.v1alpha1.vmlinuz";
const ROOTFS_MEDIA_TYPE: &str = "application/vnd.ironcore.image.rootfs.v1alpha1.rootfs";

#[derive(Serialize, Deserialize)]
struct ImageMetadata {
    image_ref: String,
}

pub struct FileStore {
    command_rx: mpsc::Receiver<FileCommand>,
    command_tx: mpsc::Sender<FileCommand>,
}

impl Default for FileStore {
    fn default() -> Self {
        Self::new()
    }
}

impl FileStore {
    pub fn new() -> Self {
        let (command_tx, command_rx) = mpsc::channel(32);
        Self {
            command_rx,
            command_tx,
        }
    }

    pub fn get_command_sender(&self) -> mpsc::Sender<FileCommand> {
        self.command_tx.clone()
    }

    pub async fn run(mut self) {
        info!("FileStore: Running and waiting for file commands.");
        while let Some(cmd) = self.command_rx.recv().await {
            self.handle_command(cmd).await;
        }
        info!("FileStore: Channel closed, shutting down.");
    }

    async fn handle_command(&mut self, cmd: FileCommand) {
        match cmd {
            FileCommand::StoreImage {
                image_uuid,
                image_ref,
                image_data,
                responder,
            } => {
                info!("FileStore: Storing image {image_uuid}");
                let final_dir = Path::new(IMAGE_DIR).join(&image_uuid);
                let result = Self::store_image_impl(&final_dir, image_data, &image_ref).await;
                let _ = responder.send(result);
            }
            FileCommand::DeleteImage {
                image_uuid,
                responder,
            } => {
                info!("FileStore: Deleting image {image_uuid}");
                let image_dir = Path::new(IMAGE_DIR).join(&image_uuid);
                let result = fs::remove_dir_all(&image_dir).await;
                let _ = responder.send(result);
            }
            FileCommand::ScanExistingImages { responder } => {
                info!("FileStore: Scanning for existing images...");
                let store = Self::scan_images_impl().await;
                let _ = responder.send(store);
            }
        }
    }

    async fn store_image_impl(
        final_dir: &Path,
        image_data: PulledImageData,
        image_ref: &str,
    ) -> Result<(), std::io::Error> {
        fs::create_dir_all(final_dir).await?;

        for layer in image_data.layers {
            match layer.media_type.as_str() {
                manifest::IMAGE_LAYER_GZIP_MEDIA_TYPE
                | manifest::IMAGE_DOCKER_LAYER_GZIP_MEDIA_TYPE => {
                    let rootfs_path = final_dir.join("rootfs");
                    if !rootfs_path.exists() {
                        fs::create_dir_all(&rootfs_path).await?;
                    }
                    let cursor = Cursor::new(layer.data);
                    let decoder = GzDecoder::new(cursor);
                    let mut archive = Archive::new(decoder);
                    tokio::task::block_in_place(move || archive.unpack(&rootfs_path))?;
                }
                ROOTFS_MEDIA_TYPE => {
                    let path = final_dir.join("disk.image");
                    fs::write(path, layer.data).await?;
                }
                INITRAMFS_MEDIA_TYPE => {
                    let path = final_dir.join("initramfs");
                    fs::write(path, layer.data).await?;
                }
                VMLINUZ_MEDIA_TYPE => {
                    let path = final_dir.join("vmlinuz");
                    fs::write(path, layer.data).await?;
                }
                _ => {
                    warn!(
                        "FileStore: Skipping layer with unsupported media type: {}",
                        layer.media_type
                    );
                }
            }
        }

        fs::write(final_dir.join("config.json"), image_data.config).await?;

        let metadata = ImageMetadata {
            image_ref: image_ref.to_string(),
        };
        let metadata_json =
            serde_json::to_string_pretty(&metadata).map_err(std::io::Error::other)?;
        fs::write(final_dir.join("metadata.json"), metadata_json).await?;
        Ok(())
    }

    async fn scan_images_impl() -> HashMap<String, ImageInfo> {
        let mut store = HashMap::new();
        let mut entries = match fs::read_dir(IMAGE_DIR).await {
            Ok(entries) => entries,
            Err(e) => {
                error!("FileStore: Failed to read image directory {IMAGE_DIR}: {e}");
                return store;
            }
        };

        while let Some(entry) = entries.next_entry().await.ok().flatten() {
            let path = entry.path();
            if !path.is_dir() {
                continue;
            }

            if let Some(uuid) = path.file_name().and_then(|s| s.to_str()) {
                let metadata_path = path.join("metadata.json");
                let disk_image_path = path.join("disk.image");
                let rootfs_path = path.join("rootfs");

                if metadata_path.exists() && (disk_image_path.exists() || rootfs_path.exists()) {
                    if let Ok(content) = fs::read_to_string(&metadata_path).await {
                        if let Ok(metadata) = serde_json::from_str::<ImageMetadata>(&content) {
                            let image_info = ImageInfo {
                                image_uuid: uuid.to_string(),
                                image_ref: metadata.image_ref,
                                state: ImageState::Ready as i32,
                            };
                            store.insert(uuid.to_string(), image_info);
                        } else {
                            warn!("FileStore: Could not parse metadata for {uuid}");
                        }
                    } else {
                        warn!("FileStore: Could not read metadata for {uuid}");
                    }
                }
            }
        }
        info!(
            "FileStore: Filesystem scan complete. Found {} images.",
            store.len()
        );
        store
    }
}
