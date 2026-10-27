#!/bin/bash
# Guest kernel: Linux for FeOS in a Firecracker microVM.

export KERNEL_VERSION=7.2.6
export KERNEL_MAJOR=7.x
export KERNEL_SHA256=039aef84f2b0994aeda3f4fcfc3d02ec9d7a9bbb9020ea264c43f446c860f606
export KERNEL_CONFIG="feos-guest-linux-${KERNEL_VERSION}.config"

# The config starts from Firecracker's minimal microVM guest config of the
# Firecracker release in hack/firecracker/version; mk-config applies FeOS's
# changes on top and rebases it onto ${KERNEL_VERSION}.
export FC_GUEST_CONFIG_URL="https://raw.githubusercontent.com/firecracker-microvm/firecracker/v1.17.0/resources/guest_configs/microvm-kernel-ci-x86_64-6.1.config"
