// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! `feos.network.v1.TrafficService`: the records as they are made.

use feos_proto::network_service::traffic_service_server::TrafficService;
use feos_proto::network_service::{StreamTrafficRecordsRequest, TrafficRecord};
use std::pin::Pin;
use tokio::sync::{broadcast, mpsc};
use tokio_stream::wrappers::ReceiverStream;
use tokio_stream::Stream;
use tonic::{Request, Response, Status};

/// Records a call may have on its way to the client.
const QUEUE: usize = 256;

/// Streams the records of a broadcast channel. A call that falls behind
/// loses records, and the next one it gets counts them in `missed`.
#[derive(Clone)]
pub struct TrafficApi {
    records: broadcast::Sender<TrafficRecord>,
}

impl TrafficApi {
    pub fn new(records: broadcast::Sender<TrafficRecord>) -> Self {
        Self { records }
    }
}

#[tonic::async_trait]
impl TrafficService for TrafficApi {
    type StreamTrafficRecordsStream =
        Pin<Box<dyn Stream<Item = Result<TrafficRecord, Status>> + Send>>;

    async fn stream_traffic_records(
        &self,
        request: Request<StreamTrafficRecordsRequest>,
    ) -> Result<Response<Self::StreamTrafficRecordsStream>, Status> {
        let pod_id = request.into_inner().pod_id;
        let (tx, rx) = mpsc::channel(QUEUE);
        tokio::spawn(forward(self.records.subscribe(), pod_id, tx));
        Ok(Response::new(Box::pin(ReceiverStream::new(rx))))
    }
}

/// Forwards the records of one pod, or of all when `pod_id` is empty,
/// until the call ends. What was missed, by this call or before a record
/// of another pod, goes with the next record it forwards.
async fn forward(
    mut records: broadcast::Receiver<TrafficRecord>,
    pod_id: String,
    tx: mpsc::Sender<Result<TrafficRecord, Status>>,
) {
    let mut missed = 0u64;
    loop {
        let record = tokio::select! {
            _ = tx.closed() => return,
            record = records.recv() => record,
        };
        match record {
            Ok(mut record) => {
                if !pod_id.is_empty() && record.pod_id != pod_id {
                    missed += record.missed;
                    continue;
                }
                record.missed += std::mem::take(&mut missed);
                if tx.send(Ok(record)).await.is_err() {
                    return;
                }
            }
            Err(broadcast::error::RecvError::Lagged(n)) => missed += n,
            Err(broadcast::error::RecvError::Closed) => return,
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use tokio_stream::StreamExt;

    fn record(pod_id: &str, missed: u64) -> TrafficRecord {
        TrafficRecord {
            pod_id: pod_id.to_string(),
            missed,
            ..Default::default()
        }
    }

    async fn call(
        api: &TrafficApi,
        pod_id: &str,
    ) -> <TrafficApi as TrafficService>::StreamTrafficRecordsStream {
        let request = Request::new(StreamTrafficRecordsRequest {
            pod_id: pod_id.to_string(),
        });
        api.stream_traffic_records(request)
            .await
            .unwrap()
            .into_inner()
    }

    #[tokio::test]
    async fn records_of_a_pod() {
        let (records, _) = broadcast::channel(16);
        let api = TrafficApi::new(records.clone());
        let mut all = call(&api, "").await;
        let mut a = call(&api, "a").await;
        records.send(record("a", 0)).unwrap();
        records.send(record("b", 2)).unwrap();
        records.send(record("a", 1)).unwrap();
        let got: Vec<_> = (&mut all).take(3).map(|r| r.unwrap()).collect().await;
        assert_eq!(got, [record("a", 0), record("b", 2), record("a", 1)]);
        // b's missed records may have been a's.
        let got: Vec<_> = (&mut a).take(2).map(|r| r.unwrap()).collect().await;
        assert_eq!(got, [record("a", 0), record("a", 3)]);
    }

    #[tokio::test]
    async fn a_slow_call_counts_what_it_missed() {
        let (records, _) = broadcast::channel(4);
        let api = TrafficApi::new(records.clone());
        // Fill the call's queue, then lag behind the channel.
        let mut stream = call(&api, "").await;
        let total = QUEUE + 4 + 10;
        for i in 0..total {
            records.send(record("a", 0)).unwrap();
            if i % 64 == 0 {
                tokio::task::yield_now().await;
            }
        }
        let mut received = 0;
        let mut missed = 0;
        while received + missed < total as u64 {
            let record = stream.next().await.unwrap().unwrap();
            received += 1;
            missed += record.missed;
        }
        assert!(missed > 0);
        assert_eq!(received + missed, total as u64);
    }

    /// The service over gRPC, as feosctl calls it.
    #[tokio::test]
    async fn a_client_streams_records() {
        use feos_proto::network_service::traffic_service_client::TrafficServiceClient;
        use feos_proto::network_service::traffic_service_server::TrafficServiceServer;

        let (records, _) = broadcast::channel(16);
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let address = listener.local_addr().unwrap();
        let server = tonic::transport::Server::builder()
            .add_service(TrafficServiceServer::new(TrafficApi::new(records.clone())))
            .serve_with_incoming(tokio_stream::wrappers::TcpListenerStream::new(listener));
        tokio::spawn(server);
        let mut client = TrafficServiceClient::connect(format!("http://{address}"))
            .await
            .unwrap();
        let request = StreamTrafficRecordsRequest {
            pod_id: "a".to_string(),
        };
        let mut stream = client
            .stream_traffic_records(request)
            .await
            .unwrap()
            .into_inner();
        // The call subscribes before it answers.
        records.send(record("b", 0)).unwrap();
        records.send(record("a", 0)).unwrap();
        assert_eq!(stream.message().await.unwrap(), Some(record("a", 0)));
    }
}
