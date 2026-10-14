// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

use crate::pull::Pulled;
use crate::{pull_dir, FileCommand, ImageInfo, IMAGE_DIR};
use feos_proto::image_service::ImageState;
use log::{error, info, warn};
use serde::{Deserialize, Serialize};
use std::collections::HashMap;
use std::os::fd::AsFd;
use std::os::unix::fs::MetadataExt;
use std::path::Path;
use tokio::{fs, sync::mpsc};

#[derive(Serialize, Deserialize)]
struct ImageMetadata {
    image_ref: String,
    #[serde(default)]
    digest: String,
    #[serde(default)]
    size_bytes: u64,
    #[serde(default)]
    exclusive: bool,
}

/// What uses the image in `image_dir`, if anything does: a container's root
/// is an overlay with the image's rootfs as a layer, and a VM's jail hard
/// links the image's files. Deleting the image would pull its files out
/// from under them.
fn in_use(image_dir: &Path) -> Option<String> {
    let mountinfo = std::fs::read_to_string("/proc/self/mountinfo").unwrap_or_default();
    if let Some(mount_point) = overlay_on(&mountinfo, image_dir) {
        return Some(format!("the overlay at {mount_point} has it as a layer"));
    }
    std::fs::read_dir(image_dir)
        .ok()?
        .flatten()
        .find(|entry| {
            entry
                .metadata()
                .is_ok_and(|meta| meta.is_file() && meta.nlink() > 1)
        })
        .map(|entry| {
            format!(
                "its {} is linked elsewhere, as into a VM's jail",
                entry.file_name().to_string_lossy()
            )
        })
}

/// The mount point of an overlay in `mountinfo` that has a layer in `dir`.
fn overlay_on(mountinfo: &str, dir: &Path) -> Option<String> {
    mountinfo.lines().find_map(|line| {
        // "<id> <parent> <dev> <root> <mount point> <options> ... - <type>
        // <source> <super options>"
        let (mount, filesystem) = line.split_once(" - ")?;
        let mut filesystem = filesystem.split(' ');
        if filesystem.next()? != "overlay" {
            return None;
        }
        let options = filesystem.nth(1)?;
        let lower = options
            .split(',')
            .find_map(|option| option.strip_prefix("lowerdir="))?;
        lower
            .split(':')
            .any(|layer| Path::new(layer).starts_with(dir))
            .then(|| mount.split(' ').nth(4).unwrap_or_default().to_string())
    })
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
                exclusive,
                pulled,
                responder,
            } => {
                info!("FileStore: Storing image {image_uuid}");
                let final_dir = Path::new(IMAGE_DIR).join(&image_uuid);
                let metadata = ImageMetadata {
                    image_ref,
                    digest: pulled.digest.clone(),
                    size_bytes: pulled.size_bytes,
                    exclusive,
                };
                let result =
                    Self::store_image_impl(&pull_dir(&image_uuid), &final_dir, pulled, &metadata)
                        .await;
                let _ = responder.send(result);
            }
            FileCommand::DeleteImage {
                image_uuid,
                responder,
            } => {
                info!("FileStore: Deleting image {image_uuid}");
                let image_dir = Path::new(IMAGE_DIR).join(&image_uuid);
                let result = match in_use(&image_dir) {
                    Some(user) => Err(std::io::Error::new(std::io::ErrorKind::ResourceBusy, user)),
                    None => fs::remove_dir_all(&image_dir).await,
                };
                let _ = responder.send(result);
            }
            FileCommand::ScanExistingImages { responder } => {
                info!("FileStore: Scanning for existing images...");
                let store = Self::scan_images_impl().await;
                let _ = responder.send(store);
            }
        }
    }

    /// Commits a pull: writes the config and the metadata next to the pulled
    /// files and renames the pull directory into place. The image is on disk,
    /// not only in the page cache, when this returns: a host may freeze and
    /// copy the data volume right after the pull.
    async fn store_image_impl(
        pull_dir: &Path,
        final_dir: &Path,
        pulled: Pulled,
        metadata: &ImageMetadata,
    ) -> Result<(), std::io::Error> {
        fs::write(pull_dir.join("config.json"), &pulled.config).await?;

        let metadata_json =
            serde_json::to_string_pretty(metadata).map_err(std::io::Error::other)?;
        fs::write(pull_dir.join("metadata.json"), metadata_json).await?;

        let _ = fs::remove_dir_all(final_dir).await;
        fs::rename(pull_dir, final_dir).await?;

        let store = std::fs::File::open(IMAGE_DIR)?;
        nix::unistd::syncfs(store.as_fd())?;
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
            // A pull that did not finish, e.g. because the guest rebooted.
            if path.extension().is_some_and(|ext| ext == "tmp") {
                info!("FileStore: Removing unfinished pull {}", path.display());
                let _ = fs::remove_dir_all(&path).await;
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
                                digest: metadata.digest,
                                size_bytes: metadata.size_bytes,
                                exclusive: metadata.exclusive,
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

#[cfg(test)]
mod tests {
    use super::*;

    const MOUNTINFO: &str = "\
22 1 0:21 / / rw,relatime - tmpfs tmpfs rw,mode=755
97 22 0:45 / /var/lib/feos/containers/c1/bundle/rootfs rw,relatime - overlay overlay rw,lowerdir=/var/lib/feos/images/aaa/rootfs,upperdir=/var/lib/feos/containers/c1/upper,workdir=/var/lib/feos/containers/c1/work
98 22 0:46 / /mnt rw,relatime - overlay overlay rw,lowerdir=/x:/var/lib/feos/images/bbb/rootfs,upperdir=/u,workdir=/w
";

    #[test]
    fn an_overlay_on_the_image_uses_it() {
        assert_eq!(
            overlay_on(MOUNTINFO, Path::new("/var/lib/feos/images/aaa")).as_deref(),
            Some("/var/lib/feos/containers/c1/bundle/rootfs")
        );
        // A lower layer of several.
        assert_eq!(
            overlay_on(MOUNTINFO, Path::new("/var/lib/feos/images/bbb")).as_deref(),
            Some("/mnt")
        );
        assert_eq!(
            overlay_on(MOUNTINFO, Path::new("/var/lib/feos/images/a")),
            None
        );
    }

    #[test]
    fn a_file_linked_elsewhere_uses_the_image() {
        let dir = tempfile::tempdir().unwrap();
        let image = dir.path().join("image");
        std::fs::create_dir(&image).unwrap();
        std::fs::write(image.join("vmlinuz"), b"kernel").unwrap();
        assert_eq!(in_use(&image), None);
        std::fs::hard_link(image.join("vmlinuz"), dir.path().join("jailed")).unwrap();
        assert!(in_use(&image).unwrap().contains("vmlinuz"));
    }
}
