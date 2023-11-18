# FeOS

## Overview
FeOS is a revolutionary init system for Linux, designed specifically for hypervisors and servers that run containers. Unlike traditional systems that use sysvinit or systemd, FeOS boots directly from the Linux Kernel. Written in Rust for enhanced security and memory safety, FeOS is an ideal solution for multi-tenant environments, offering robust protection against common vulnerabilities like buffer overflows.

## Key Features
**Init System**: FeOS replaces traditional Linux init systems, booting directly from the Linux Kernel.  
**Container and VM Support**: Supports running VMs, Containers, and MicroVMs, including the ability to run containers within MicroVMs for enhanced security.  
**Rust-Based**: Developed in Rust, leveraging memory-safe programming to mitigate common vulnerabilities.  
**Compatibility**: Compatible with Rust-based container and VM runtimes like youki, rustvmm, and cloud-hypervisor.  
**Cluster Integration**: Designed for use in clustered environments, FeOS treats servers as hardware resources for executing programs.  
**gRPC API**: Exposes a gRPC API for external orchestrators to manage VMs, Containers, and MicroVMs.

## Installation and Setup
**Network Booting**: FeOS primarily boots via the network using a signed Unified Kernel Image (UKI) to support secure booting.  
**Initial Configuration**: Initial setup through the Kernel command line, configuring external network connectivity.  
**Ongoing Configuration**: Further configurations and management are handled via the gRPC API.

## Security and Isolation
**MicroVMs for Multi-Tenancy**: Running containers in MicroVMs ensures secure operation in multi-tenant environments.  
**SmartNIC Integration**: Compatible with SmartNICs (DPUs) for enhanced security, allowing Kubernetes controllers on DPUs to manage workloads on the host via the gRPC API.

## Logging and Monitoring
**Log Management**: The gRPC API also facilitates the transport of log output from applications running on the server.

## Use Cases
**Multi-Tenant Environments**: Ideal for environments requiring strong isolation between different tenants' workloads.  
**Clustered Server Environments**: Optimized for clustered server environments, providing efficient management and execution of containers and VMs.

## Contributing
Contributions are welcome! Guidelines for contributing to FeOS are provided to maintain the quality and integrity of the codebase.

## Licensing
FeOS is licensed under the [Apache License v2.0](LICENSE), ensuring open-source availability and collaborative development potential.

Copyright 2023 by Malte Janduda.
