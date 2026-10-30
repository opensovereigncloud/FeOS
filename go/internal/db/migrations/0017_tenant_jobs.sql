-- SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
-- SPDX-License-Identifier: Apache-2.0

-- A tenant's jobs (ADR 0018, "A tenant's job"): a container run to its
-- end, its input in the call, run exactly once; the job carries its one
-- run's status, exit code, result and log. A job's pod is a row of
-- sandboxes under the job's ID with tenant_job set, which every query that
-- tells a tenant's sandbox from an instance counts as an instance; the row
-- goes when the job ends, the job's row stays until the sweep (7 days
-- after its end) or a DELETE. sealed holds the job's secrets and its image
-- credentials under --secret-key-file's key with the AAD "jobs|<id>", NULL
-- once the job has ended or when it has none. The database holds no
-- secret value: the pod's template names them, and the engine unseals
-- them when it builds the pod's spec.
--
-- Additive: an instance of the previous version never reads the table,
-- and inserts sandboxes with tenant_job false.

CREATE TABLE jobs (
  id text COLLATE "C" PRIMARY KEY CHECK (id ~ '^[a-hjkmnp-tv-z][0-9a-hjkmnp-tv-z]{10}$'),   -- its pod's sandbox ID
  owner text NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  idempotency_key text,                        -- the create's Idempotency-Key; NULL: none
  request_hash text NOT NULL,                  -- sha256 hex of the create's body: a repeat with another is refused
  image text NOT NULL, image_ref text NOT NULL,   -- as asked, and pinned to its digest at the create
  vcpus integer NOT NULL, memory_mib integer NOT NULL,
  result_path text NOT NULL DEFAULT '',
  labels jsonb NOT NULL DEFAULT '{}',
  sealed bytea,
  phase text NOT NULL DEFAULT 'queued'
    CHECK (phase IN ('queued','running','succeeded','failed','deadline_exceeded','cancelled')),
  reason text NOT NULL DEFAULT '',             -- of failed: exit, image, node_lost, no_exit_status
  detail text NOT NULL DEFAULT '',
  exit_code integer,
  result text NOT NULL DEFAULT '',             -- the result file, at most 4 KiB
  log_tail jsonb NOT NULL DEFAULT '[]',        -- the last 64 KiB and 1000 lines, redacted
  created_at timestamptz NOT NULL DEFAULT now(), started_at timestamptz,
  finished_at timestamptz,                     -- NULL while it has not ended
  deadline_at timestamptz NOT NULL,
  CHECK ((finished_at IS NULL) = (phase IN ('queued','running'))),
  UNIQUE (owner, idempotency_key));
CREATE INDEX jobs_owner ON jobs(owner, created_at);
CREATE INDEX jobs_open ON jobs(created_at) WHERE finished_at IS NULL;
CREATE INDEX jobs_finished ON jobs(finished_at) WHERE finished_at IS NOT NULL;

ALTER TABLE sandboxes ADD COLUMN tenant_job boolean NOT NULL DEFAULT false;   -- a tenant's job's pod; fixed
CREATE INDEX sandboxes_tenant_job ON sandboxes(id) WHERE tenant_job;
