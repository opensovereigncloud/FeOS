# SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
# SPDX-License-Identifier: Apache-2.0

# The feos-node pod's image (ADR 0020): feosd, diskd, feosctl, Firecracker
# and the guest from FeOS's .deb packages (make packages), unpacked where
# the packages put them, and kmod for loading the ublk driver from the
# host's modules. dpkg-deb -x installs no dependencies: those of the
# packages come here, ca-certificates (diskd's, for TLS to the object
# store) among them. Build context: target/gardener/feos-node (Makefile).
FROM debian:trixie-20250929-slim
RUN apt-get update && \
    apt-get install -y --no-install-recommends kmod ca-certificates && \
    apt-get clean && rm -rf /var/lib/apt/lists/* && \
    test -s /etc/ssl/certs/ca-certificates.crt && \
    groupadd --system feos
COPY *.deb /tmp/debs/
RUN for deb in /tmp/debs/*.deb; do dpkg-deb -x "$deb" /; done && \
    rm -rf /tmp/debs /etc/apt/apt.conf.d/50feosd
COPY version /feos/version
COPY run /feos/run
ENTRYPOINT ["/feos/run"]
