use tonic::{transport::Server, Request, Response, Status};

use feos_grpc::feos_grpc_server::{FeosGrpc, FeosGrpcServer};
use feos_grpc::Empty;

pub mod feos_grpc {
    tonic::include_proto!("feos_grpc"); // The string specified here must match the proto package name
}

#[derive(Debug, Default)]
pub struct FeOSAPI {}

#[tonic::async_trait]
impl FeosGrpc for FeOSAPI {
    async fn ping(
        &self,
        request: Request<Empty>, // Accept request of type HelloRequest
    ) -> Result<Response<Empty>, Status> {
        // Return an instance of type HelloReply
        println!("Got a request: {:?}", request);

        let reply = feos_grpc::Empty {};

        Ok(Response::new(reply)) // Send back our formatted greeting
    }
}

pub async fn daemon_start() -> Result<(), Box<dyn std::error::Error>> {
    let addr = "[::]:1337".parse()?;
    let api = FeOSAPI::default();

    Server::builder()
        .add_service(FeosGrpcServer::new(api))
        .serve(addr)
        .await?;

    Ok(())
}
