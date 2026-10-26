// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

use crate::persistence::{PersistenceError, VmRecord, VmStatus};
use feos_proto::microvm::{VmConfig, VmState};
use feos_utils::pod_id::PodId;
use log::info;
use prost::Message;
use sqlx::encode::IsNull;
use sqlx::error::BoxDynError;
use sqlx::sqlite::{
    Sqlite, SqliteArgumentValue, SqlitePool, SqlitePoolOptions, SqliteTypeInfo, SqliteValueRef,
};
use sqlx::{Decode, Encode, Type, TypeInfo, ValueRef};
use uuid::Uuid;

#[derive(Clone)]
pub struct VmRepository {
    pool: SqlitePool,
}

/// A VM's ID in the table: a UUID as its 16 bytes, a BLOB, as the VMs
/// made before short IDs have it, and a short ID as TEXT (ADR 0019).
#[derive(Debug, Clone, Copy)]
struct DbVmId(PodId);

impl Type<Sqlite> for DbVmId {
    fn type_info() -> SqliteTypeInfo {
        <Vec<u8> as Type<Sqlite>>::type_info()
    }

    fn compatible(ty: &SqliteTypeInfo) -> bool {
        <Vec<u8> as Type<Sqlite>>::compatible(ty) || <String as Type<Sqlite>>::compatible(ty)
    }
}

impl<'q> Encode<'q, Sqlite> for DbVmId {
    fn encode_by_ref(
        &self,
        args: &mut Vec<SqliteArgumentValue<'q>>,
    ) -> Result<IsNull, BoxDynError> {
        match self.0 {
            PodId::Uuid(id) => <Uuid as Encode<Sqlite>>::encode_by_ref(&id, args),
            id => <String as Encode<Sqlite>>::encode(id.to_string(), args),
        }
    }

    fn produces(&self) -> Option<SqliteTypeInfo> {
        Some(match self.0 {
            PodId::Uuid(_) => <Vec<u8> as Type<Sqlite>>::type_info(),
            _ => <String as Type<Sqlite>>::type_info(),
        })
    }
}

impl<'r> Decode<'r, Sqlite> for DbVmId {
    fn decode(value: SqliteValueRef<'r>) -> Result<Self, BoxDynError> {
        if value.type_info().name() == "BLOB" {
            let id = <Uuid as Decode<Sqlite>>::decode(value)?;
            return Ok(Self(PodId::Uuid(id)));
        }
        let id = <&str as Decode<Sqlite>>::decode(value)?;
        Ok(Self(PodId::parse(id)?))
    }
}

#[derive(sqlx::FromRow, Debug)]
struct DbVmRow {
    vm_id: DbVmId,
    image_uuid: Uuid,
    state: String,
    last_msg: String,
    pid: Option<i64>,
    config_blob: Vec<u8>,
}

fn string_to_vm_state(s: &str) -> Result<VmState, PersistenceError> {
    match s {
        "VM_STATE_CREATING" => Ok(VmState::Creating),
        "VM_STATE_CREATED" => Ok(VmState::Created),
        "VM_STATE_RUNNING" => Ok(VmState::Running),
        "VM_STATE_PAUSED" => Ok(VmState::Paused),
        "VM_STATE_STOPPED" => Ok(VmState::Stopped),
        "VM_STATE_CRASHED" => Ok(VmState::Crashed),
        "VM_STATE_SUSPENDED" => Ok(VmState::Suspended),
        "VM_STATE_UNSPECIFIED" => Ok(VmState::Unspecified),
        _ => Err(PersistenceError::InvalidStateString(s.to_string())),
    }
}

impl VmRepository {
    pub async fn connect(db_url: &str) -> Result<Self, PersistenceError> {
        let pool = SqlitePoolOptions::new()
            .max_connections(1)
            .connect(db_url)
            .await?;

        info!("Persistence: Running database migrations...");
        sqlx::migrate!("./migrations").run(&pool).await?;
        info!("Persistence: Database migrations completed.");

        Ok(Self { pool })
    }

    pub async fn get_vm(&self, vm_id: PodId) -> Result<Option<VmRecord>, PersistenceError> {
        let row_opt = sqlx::query_as::<_, DbVmRow>(
            "SELECT vm_id, image_uuid, state, last_msg, pid, config_blob FROM vms WHERE vm_id = ?1",
        )
        .bind(DbVmId(vm_id))
        .fetch_optional(&self.pool)
        .await?;

        if let Some(row) = row_opt {
            let config = VmConfig::decode(&*row.config_blob)?;
            let state = string_to_vm_state(&row.state)?;

            let record = VmRecord {
                vm_id: row.vm_id.0,
                image_uuid: row.image_uuid,
                status: VmStatus {
                    state,
                    last_msg: row.last_msg,
                    process_id: row.pid,
                },
                config,
            };
            Ok(Some(record))
        } else {
            Ok(None)
        }
    }

    pub async fn list_all_vms(&self) -> Result<Vec<VmRecord>, PersistenceError> {
        let rows = sqlx::query_as::<_, DbVmRow>(
            "SELECT vm_id, image_uuid, state, last_msg, pid, config_blob FROM vms",
        )
        .fetch_all(&self.pool)
        .await?;

        let mut records = Vec::with_capacity(rows.len());
        for row in rows {
            let config = VmConfig::decode(&*row.config_blob)?;
            let state = string_to_vm_state(&row.state)?;

            let record = VmRecord {
                vm_id: row.vm_id.0,
                image_uuid: row.image_uuid,
                status: VmStatus {
                    state,
                    last_msg: row.last_msg,
                    process_id: row.pid,
                },
                config,
            };
            records.push(record);
        }

        Ok(records)
    }

    pub async fn save_vm(&self, vm: &VmRecord) -> Result<(), PersistenceError> {
        let mut config_blob = Vec::new();
        vm.config.encode(&mut config_blob)?;

        let state_str = format!("VM_STATE_{:?}", vm.status.state).to_uppercase();
        let id = DbVmId(vm.vm_id);

        sqlx::query_unchecked!(
            r#"
            INSERT OR REPLACE INTO vms (vm_id, image_uuid, state, last_msg, pid, config_blob)
            VALUES (?1, ?2, ?3, ?4, ?5, ?6)
            "#,
            id,
            vm.image_uuid,
            state_str,
            vm.status.last_msg,
            vm.status.process_id,
            config_blob,
        )
        .execute(&self.pool)
        .await?;

        Ok(())
    }

    pub async fn update_vm_status(
        &self,
        vm_id: PodId,
        new_state: VmState,
        message: &str,
    ) -> Result<bool, PersistenceError> {
        let state_str = format!("VM_STATE_{new_state:?}").to_uppercase();
        let id = DbVmId(vm_id);

        let result = sqlx::query!(
            r#"
            UPDATE vms
            SET state = ?1, last_msg = ?2
            WHERE vm_id = ?3
            "#,
            state_str,
            message,
            id,
        )
        .execute(&self.pool)
        .await?;

        Ok(result.rows_affected() > 0)
    }

    pub async fn update_vm_pid(&self, vm_id: PodId, pid: i64) -> Result<(), PersistenceError> {
        let id = DbVmId(vm_id);
        sqlx::query!("UPDATE vms SET pid = ?1 WHERE vm_id = ?2", pid, id)
            .execute(&self.pool)
            .await?;
        Ok(())
    }

    pub async fn delete_vm(&self, vm_id: PodId) -> Result<(), PersistenceError> {
        let id = DbVmId(vm_id);
        let result = sqlx::query!("DELETE FROM vms WHERE vm_id = ?1", id)
            .execute(&self.pool)
            .await?;

        if result.rows_affected() == 0 {
            log::warn!("Attempted to delete VM {vm_id} from DB, but no record was found.");
        }

        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn record(vm_id: PodId) -> VmRecord {
        VmRecord {
            vm_id,
            image_uuid: Uuid::nil(),
            status: VmStatus {
                state: VmState::Running,
                last_msg: "up".to_string(),
                process_id: Some(7),
            },
            config: VmConfig::default(),
        }
    }

    #[tokio::test]
    async fn vms_with_uuids_and_short_ids_live_side_by_side() {
        let repository = VmRepository::connect("sqlite::memory:").await.unwrap();
        // A VM saved before short IDs: its ID a UUID's 16 bytes.
        let old: Uuid = "8d99e180-1694-4d94-a42e-097769b89510".parse().unwrap();
        sqlx::query(
            "INSERT INTO vms (vm_id, image_uuid, state, last_msg, pid, config_blob) \
             VALUES (?1, ?2, 'VM_STATE_RUNNING', 'up', 7, x'')",
        )
        .bind(old)
        .bind(Uuid::nil())
        .execute(&repository.pool)
        .await
        .unwrap();
        let short = PodId::parse("k7m2q9x4c1b").unwrap();
        repository.save_vm(&record(short)).await.unwrap();
        // A UUID saved now is stored as before.
        let new: PodId = "11111111-2222-3333-4444-555555555555".parse().unwrap();
        repository.save_vm(&record(new)).await.unwrap();
        let types: Vec<(String,)> = sqlx::query_as("SELECT typeof(vm_id) FROM vms ORDER BY rowid")
            .fetch_all(&repository.pool)
            .await
            .unwrap();
        assert_eq!(
            types.iter().map(|(t,)| t.as_str()).collect::<Vec<_>>(),
            ["blob", "text", "blob"]
        );

        let mut ids: Vec<String> = repository
            .list_all_vms()
            .await
            .unwrap()
            .into_iter()
            .map(|vm| vm.vm_id.to_string())
            .collect();
        ids.sort();
        assert_eq!(
            ids,
            [
                "11111111-2222-3333-4444-555555555555",
                "8d99e180-1694-4d94-a42e-097769b89510",
                "k7m2q9x4c1b"
            ]
        );

        let old = PodId::Uuid(old);
        for id in [old, short] {
            let vm = repository.get_vm(id).await.unwrap().unwrap();
            assert_eq!(vm.vm_id, id);
            assert!(repository
                .update_vm_status(id, VmState::Paused, "paused")
                .await
                .unwrap());
            repository.update_vm_pid(id, 9).await.unwrap();
            let vm = repository.get_vm(id).await.unwrap().unwrap();
            assert_eq!(
                (vm.status.state, vm.status.process_id),
                (VmState::Paused, Some(9))
            );
        }
        // Upper case reads as the same UUID.
        let upper = PodId::parse("8D99E180-1694-4D94-A42E-097769B89510").unwrap();
        assert!(repository.get_vm(upper).await.unwrap().is_some());

        repository.delete_vm(old).await.unwrap();
        repository.delete_vm(short).await.unwrap();
        assert!(repository.get_vm(old).await.unwrap().is_none());
        assert!(repository.get_vm(short).await.unwrap().is_none());
        assert_eq!(repository.list_all_vms().await.unwrap().len(), 1);
    }
}
