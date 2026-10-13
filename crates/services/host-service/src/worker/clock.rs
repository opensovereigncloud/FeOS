// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! The wall clock as the host pushes it. A guest has no NTP and does not read
//! its RTC; its host sets the time after every boot and every resume.

use crate::error::HostError;
use feos_proto::host_service::{GetTimeResponse, SetTimeRequest, SetTimeResponse};
use log::{error, info};
use nix::sys::time::TimeSpec;
use nix::time::{clock_gettime, clock_settime, ClockId};
use prost_types::Timestamp;
use tokio::sync::oneshot;

pub async fn handle_set_time(
    req: SetTimeRequest,
    responder: oneshot::Sender<Result<SetTimeResponse, HostError>>,
) {
    let result = set_time(req.time);
    if result.is_ok() {
        info!("HostWorker: Clock set by host.");
    }
    if responder.send(result).is_err() {
        error!("HostWorker: Failed to send response for SetTime.");
    }
}

pub async fn handle_get_time(responder: oneshot::Sender<Result<GetTimeResponse, HostError>>) {
    let result = clock_gettime(ClockId::CLOCK_REALTIME)
        .map(|now| GetTimeResponse {
            time: Some(Timestamp {
                seconds: now.tv_sec(),
                nanos: now.tv_nsec() as i32,
            }),
        })
        .map_err(|e| HostError::Failed(format!("clock_gettime: {e}")));
    if responder.send(result).is_err() {
        error!("HostWorker: Failed to send response for GetTime.");
    }
}

fn set_time(time: Option<Timestamp>) -> Result<SetTimeResponse, HostError> {
    let time = time.ok_or_else(|| HostError::InvalidArgument("missing time".to_string()))?;
    if time.seconds < 0 || !(0..1_000_000_000).contains(&time.nanos) {
        return Err(HostError::InvalidArgument(format!(
            "invalid time: seconds={} nanos={}",
            time.seconds, time.nanos
        )));
    }
    let spec = TimeSpec::new(time.seconds, i64::from(time.nanos));
    clock_settime(ClockId::CLOCK_REALTIME, spec)
        .map_err(|e| HostError::Failed(format!("clock_settime: {e}")))?;
    Ok(SetTimeResponse {})
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn rejects_bad_timestamps() {
        for time in [
            None,
            Some(Timestamp {
                seconds: -1,
                nanos: 0,
            }),
            Some(Timestamp {
                seconds: 0,
                nanos: 1_000_000_000,
            }),
        ] {
            assert!(matches!(set_time(time), Err(HostError::InvalidArgument(_))));
        }
    }
}
