-- SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
-- SPDX-License-Identifier: Apache-2.0

-- ferund's part of the sink (ADR 0026, docs/architecture/sink.md).
--
-- sandboxes.log_labels are the labels of the pod's container logs, which
-- its create sends (CreatePodRequest.log_labels): set at the insert of an
-- environment's instance, an environment job's run and a tenant's job's
-- pod, fixed, '{}' for every other row (its logs are not shipped).
-- jobs.log_retention_days is what a tenant's job asked its log be kept
-- (ferun.retention_days); NULL: the sink's default.
--
-- service_tokens are the long-lived tokens of the platform's own services
-- (kind sink: an instance of ferun-sink), stored by the SHA-256 of the
-- token; a revoke deletes the row. cert_window and cert_count are the
-- rate limit of POST /api/v1/service-certificates: the certificates
-- issued under the token since cert_window, an hour at most.
--
-- Additive: an instance of the previous version inserts sandboxes and
-- jobs with the defaults (its pods' logs are not shipped) and never reads
-- service_tokens.

ALTER TABLE sandboxes ADD COLUMN log_labels jsonb NOT NULL DEFAULT '{}';   -- fixed at the insert

ALTER TABLE jobs ADD COLUMN log_retention_days integer CHECK (log_retention_days BETWEEN 1 AND 400);

CREATE TABLE service_tokens (
  hash text PRIMARY KEY,                       -- sha256 hex of "feos-<kind>-<secret>"
  id uuid NOT NULL UNIQUE DEFAULT gen_random_uuid(),
  kind text NOT NULL CHECK (kind IN ('sink')),
  name text NOT NULL CHECK (name ~ '^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$'),   -- the holder's SPIFFE name: spiffe://<td>/<kind>/<name>
  created_by text NOT NULL DEFAULT '', created_at timestamptz NOT NULL DEFAULT now(),
  last_used_at timestamptz,
  cert_window timestamptz NOT NULL DEFAULT now(), cert_count integer NOT NULL DEFAULT 0);
