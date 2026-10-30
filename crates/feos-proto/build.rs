// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

fn main() -> Result<(), Box<dyn std::error::Error>> {
    let proto_dir = "../../proto";

    tonic_build::configure()
        .protoc_arg("--experimental_allow_proto3_optional")
        .compile_protos(
            &[
                format!("{proto_dir}/feos/microvm/v1/microvm.proto"),
                format!("{proto_dir}/feos/pod/v1/pod.proto"),
                format!("{proto_dir}/feos/host/v1/host.proto"),
                format!("{proto_dir}/feos/image/v1/image.proto"),
                format!("{proto_dir}/feos/container/v1/container.proto"),
                format!("{proto_dir}/feos/task/v1/task.proto"),
                format!("{proto_dir}/feos/lifecycle/v1/lifecycle.proto"),
                format!("{proto_dir}/feos/tool/v1/tool.proto"),
                format!("{proto_dir}/feos/watch/v1/watch.proto"),
                format!("{proto_dir}/feos/portforward/v1/portforward.proto"),
                format!("{proto_dir}/feos/kube/v1/kube.proto"),
                format!("{proto_dir}/feos/ssh/v1/ssh.proto"),
                format!("{proto_dir}/feos/disk/v1/disk.proto"),
                format!("{proto_dir}/feos/network/v1/network.proto"),
                format!("{proto_dir}/feos/guest/v1/guest.proto"),
                format!("{proto_dir}/feos/volume/v1/volume.proto"),
                format!("{proto_dir}/feos/sink/v1/sink.proto"),
            ],
            &[proto_dir],
        )?;
    // The uplink's API names the pods' and the network's messages: they are
    // the modules above, not modules nested in feos.fleet.v1's. Its own
    // directory, since this pass writes the files it imports again.
    let fleet_dir = format!("{}/fleet", std::env::var("OUT_DIR")?);
    std::fs::create_dir_all(&fleet_dir)?;
    tonic_build::configure()
        .out_dir(&fleet_dir)
        .extern_path(".feos.pod.v1", "crate::pod_service")
        .extern_path(".feos.network.v1", "crate::network_service")
        .compile_protos(
            &[format!("{proto_dir}/feos/fleet/v1/fleet.proto")],
            &[proto_dir],
        )?;
    // OpenTelemetry's protocol (ADR 0026): the node exports its pods' logs
    // to the sink over OTLP's LogsService. Its packages nest, so they get
    // a directory and a module tree of their own (lib.rs).
    let otlp_dir = format!("{}/otlp", std::env::var("OUT_DIR")?);
    std::fs::create_dir_all(&otlp_dir)?;
    tonic_build::configure()
        .out_dir(&otlp_dir)
        .build_server(false)
        .compile_protos(
            &[format!(
                "{proto_dir}/opentelemetry/proto/collector/logs/v1/logs_service.proto"
            )],
            &[proto_dir],
        )?;
    Ok(())
}
