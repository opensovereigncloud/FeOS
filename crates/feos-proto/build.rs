// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

fn main() -> Result<(), Box<dyn std::error::Error>> {
    let proto_dir = "../../proto";

    tonic_build::configure()
        .protoc_arg("--experimental_allow_proto3_optional")
        .compile_protos(
            &[
                format!("{proto_dir}/feos/vm/v1/vm.proto"),
                format!("{proto_dir}/feos/host/v1/host.proto"),
                format!("{proto_dir}/feos/image/v1/image.proto"),
                format!("{proto_dir}/feos/container/v1/container.proto"),
                format!("{proto_dir}/feos/task/v1/task.proto"),
                format!("{proto_dir}/feos/lifecycle/v1/lifecycle.proto"),
                format!("{proto_dir}/feos/tool/v1/tool.proto"),
            ],
            &[proto_dir],
        )?;
    Ok(())
}
