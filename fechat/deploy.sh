#!/bin/sh
# SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
# SPDX-License-Identifier: Apache-2.0
#
# Build FeChat's two images, push them to oci.fecore.dev and roll both
# deployments in the namespace fechat to the new tag. Pushing needs a
# `docker login oci.fecore.dev`; kubectl must point at the cluster.
#
# Usage: fechat/deploy.sh [tag]    (default: 0.6.0)
#
# Give every deploy a new tag: the nodes keep an image they already have
# (imagePullPolicy: IfNotPresent), and setting the tag a deployment
# already runs changes nothing. DOCKER selects the client, e.g.
# DOCKER="sudo docker" or DOCKER=podman.
set -eu
cd "$(dirname "$0")"
TAG=${1:-0.6.0}
REG=oci.fecore.dev
DOCKER=${DOCKER:-docker}

$DOCKER build -q -t "$REG/fechat-app:$TAG" .
$DOCKER build -q -t "$REG/fechat-backend:$TAG" backend/
$DOCKER push "$REG/fechat-app:$TAG"
$DOCKER push "$REG/fechat-backend:$TAG"

kubectl -n fechat set image deploy/fechat-app fechat-app="$REG/fechat-app:$TAG"
kubectl -n fechat set image deploy/fechat-backend fechat-backend="$REG/fechat-backend:$TAG"
kubectl -n fechat rollout status deploy/fechat-app
kubectl -n fechat rollout status deploy/fechat-backend
echo "https://chat.fecode.dev runs tag $TAG"
