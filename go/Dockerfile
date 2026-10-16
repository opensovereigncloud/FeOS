# SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
# SPDX-License-Identifier: Apache-2.0

# The image of ferund, FeRun (ADR 0015): ferund built static, on a
# distroless base, as a non-root user. The build context is the
# repository's root, so that the management UI's build can join it:
#
#   docker build -f go/Dockerfile -t ferund .
#
# Without that build, the image serves the API without the UI (the
# placeholder go/internal/ui/dist/.keep is embedded).
#
# Termination grace: on SIGTERM an instance keeps serving its requests and
# finishing its ops for up to --shutdown-grace (10 minutes by default),
# then closes its node connections. A container deployment sets its
# termination grace period (Kubernetes: terminationGracePeriodSeconds) to
# at least --shutdown-grace plus 30 s; the default of 30 s would kill
# ferund long before the tool calls in flight end.

# The Go of go/go.mod's toolchain line.
FROM golang:1.26.8 AS build
WORKDIR /src/go
COPY go/go.mod go/go.sum ./
RUN go mod download
COPY go/ ./
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-X main.version=${VERSION}" -o /out/ferund ./cmd/ferund

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/ferund /ferund
# The front doors (REST, MCP, the UI, the ports) and the nodes' port.
EXPOSE 8100 7443
ENTRYPOINT ["/ferund"]
CMD ["serve"]
