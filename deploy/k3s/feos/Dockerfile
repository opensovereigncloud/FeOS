# SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
# SPDX-License-Identifier: Apache-2.0

# The feos DaemonSet's image: FeOS's .deb packages (make packages) and the
# script that installs them on the node. Build context: target/packages,
# with this directory's install script copied in (deploy/k3s/Makefile).
FROM debian:trixie-20250929-slim
COPY *.deb /feos/debs/
COPY version /feos/version
COPY install /feos/install
ENTRYPOINT ["/feos/install"]
