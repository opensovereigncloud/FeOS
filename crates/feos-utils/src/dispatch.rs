// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

use tokio::sync::{mpsc, oneshot};
use tonic::{Response, Status};

/// Sends a command to a service's dispatcher and waits for its reply.
///
/// `command_constructor` builds the command around the reply channel. The
/// dispatcher's error type turns into the gRPC status.
pub async fn dispatch_and_wait<C, T, E>(
    dispatcher: &mpsc::Sender<C>,
    command_constructor: impl FnOnce(oneshot::Sender<Result<T, E>>) -> C,
) -> Result<Response<T>, Status>
where
    E: Into<Status>,
{
    let (resp_tx, resp_rx) = oneshot::channel();
    let cmd = command_constructor(resp_tx);

    dispatcher
        .send(cmd)
        .await
        .map_err(|e| Status::internal(format!("Failed to send command to dispatcher: {e}")))?;

    match resp_rx.await {
        Ok(Ok(result)) => Ok(Response::new(result)),
        Ok(Err(e)) => Err(e.into()),
        Err(_) => Err(Status::internal(
            "Dispatcher task dropped response channel.",
        )),
    }
}
