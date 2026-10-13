// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

use crate::persistence::{ContainerRecord, ContainerStatus, PersistenceError};
use feos_proto::container_service::{ContainerConfig, ContainerState};
use log::info;
use prost::Message;
use sqlx::sqlite::{SqlitePool, SqlitePoolOptions};
use uuid::Uuid;

#[derive(Clone)]
pub struct ContainerRepository {
    pool: SqlitePool,
}

#[derive(sqlx::FromRow, Debug)]
struct DbContainerRow {
    container_id: String,
    image_uuid: String,
    state: String,
    pid: Option<i64>,
    config_blob: Vec<u8>,
}

fn parse_uuid(s: &str) -> Result<Uuid, PersistenceError> {
    Uuid::parse_str(s).map_err(|_| PersistenceError::InvalidUuid(s.to_string()))
}

fn string_to_container_state(s: &str) -> Result<ContainerState, PersistenceError> {
    match s {
        "PULLING_IMAGE" => Ok(ContainerState::PullingImage),
        "CREATED" => Ok(ContainerState::Created),
        "RUNNING" => Ok(ContainerState::Running),
        "STOPPED" => Ok(ContainerState::Stopped),
        "CONTAINER_STATE_UNSPECIFIED" => Ok(ContainerState::Unspecified),
        _ => Err(PersistenceError::InvalidStateString(s.to_string())),
    }
}

fn container_state_to_string(state: ContainerState) -> &'static str {
    match state {
        ContainerState::PullingImage => "PULLING_IMAGE",
        ContainerState::Created => "CREATED",
        ContainerState::Running => "RUNNING",
        ContainerState::Stopped => "STOPPED",
        ContainerState::Unspecified => "CONTAINER_STATE_UNSPECIFIED",
    }
}

impl ContainerRepository {
    pub async fn connect(db_url: &str) -> Result<Self, PersistenceError> {
        let pool = SqlitePoolOptions::new()
            .max_connections(1)
            .connect(db_url)
            .await?;

        info!("Persistence: Running container-service database migrations...");
        sqlx::migrate!("./migrations").run(&pool).await?;
        info!("Persistence: Database migrations completed for container-service.");

        Ok(Self { pool })
    }

    pub async fn get_container(
        &self,
        container_id: &str,
    ) -> Result<Option<ContainerRecord>, PersistenceError> {
        let row_opt = sqlx::query_as::<_, DbContainerRow>(
            "SELECT container_id, image_uuid, state, pid, config_blob FROM containers WHERE container_id = ?1",
        )
        .bind(container_id)
        .fetch_optional(&self.pool)
        .await?;

        if let Some(row) = row_opt {
            let config = ContainerConfig::decode(&*row.config_blob)?;
            let state = string_to_container_state(&row.state)?;

            let record = ContainerRecord {
                container_id: row.container_id,
                image_uuid: parse_uuid(&row.image_uuid)?,
                status: ContainerStatus {
                    state,
                    process_id: row.pid,
                },
                config,
            };
            Ok(Some(record))
        } else {
            Ok(None)
        }
    }

    pub async fn list_all_containers(&self) -> Result<Vec<ContainerRecord>, PersistenceError> {
        let rows = sqlx::query_as::<_, DbContainerRow>(
            "SELECT container_id, image_uuid, state, pid, config_blob FROM containers",
        )
        .fetch_all(&self.pool)
        .await?;

        let mut records = Vec::with_capacity(rows.len());
        for row in rows {
            let config = ContainerConfig::decode(&*row.config_blob)?;
            let state = string_to_container_state(&row.state)?;

            let record = ContainerRecord {
                container_id: row.container_id,
                image_uuid: parse_uuid(&row.image_uuid)?,
                status: ContainerStatus {
                    state,
                    process_id: row.pid,
                },
                config,
            };
            records.push(record);
        }

        Ok(records)
    }

    pub async fn save_container(
        &self,
        container: &ContainerRecord,
    ) -> Result<(), PersistenceError> {
        let mut config_blob = Vec::new();
        container.config.encode(&mut config_blob)?;

        let state_str = container_state_to_string(container.status.state);

        sqlx::query(
            r#"
            INSERT OR REPLACE INTO containers (container_id, image_uuid, state, pid, config_blob)
            VALUES (?1, ?2, ?3, ?4, ?5)
            "#,
        )
        .bind(&container.container_id)
        .bind(container.image_uuid.to_string())
        .bind(state_str)
        .bind(container.status.process_id)
        .bind(config_blob)
        .execute(&self.pool)
        .await?;

        Ok(())
    }

    pub async fn update_container_state(
        &self,
        container_id: &str,
        new_state: ContainerState,
    ) -> Result<bool, PersistenceError> {
        let state_str = container_state_to_string(new_state);

        let result = sqlx::query(
            r#"
            UPDATE containers
            SET state = ?1
            WHERE container_id = ?2
            "#,
        )
        .bind(state_str)
        .bind(container_id)
        .execute(&self.pool)
        .await?;

        Ok(result.rows_affected() > 0)
    }

    pub async fn update_container_pid(
        &self,
        container_id: &str,
        pid: i64,
    ) -> Result<(), PersistenceError> {
        sqlx::query("UPDATE containers SET pid = ?1 WHERE container_id = ?2")
            .bind(pid)
            .bind(container_id)
            .execute(&self.pool)
            .await?;
        Ok(())
    }

    pub async fn delete_container(&self, container_id: &str) -> Result<(), PersistenceError> {
        let result = sqlx::query("DELETE FROM containers WHERE container_id = ?1")
            .bind(container_id)
            .execute(&self.pool)
            .await?;

        if result.rows_affected() == 0 {
            log::warn!(
                "Attempted to delete container {container_id} from DB, but no record was found."
            );
        }

        Ok(())
    }
}
