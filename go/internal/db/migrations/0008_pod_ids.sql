-- SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
-- SPDX-License-Identifier: Apache-2.0

-- Pod IDs are short (ADR 0019): 11 characters of lowercase Crockford
-- base32, the first a letter, which ferund mints. A pod made before keeps
-- its UUID until it is deleted, so every column that holds a pod's or a
-- sandbox's ID changes from uuid to text and holds either, in the
-- canonical form a check names: a short ID, or a UUID in lowercase, as
-- uuid::text renders it. Other IDs (tokens, environments, volumes,
-- incarnations, ops, watches) stay UUIDs.
--
-- The columns are text COLLATE "C": they compare and sort byte by byte, as
-- the uuids did and as feosd sorts its inventory, and their indexes do not
-- go through the locale.
--
-- The foreign keys to sandboxes(id) are dropped and added again around the
-- change of its type, which they would otherwise refuse. Each table is
-- rewritten once, its indexes with it; the checks are evaluated during the
-- rewrite. activity and traffic_records are the large ones: their
-- partitions are rewritten under the migration's lock, and need as much
-- free disk as they take for the time of it.
--
-- Not additive: an instance of the previous version, which a rolling
-- deploy runs beside this one for a while, fails the queries that cast a
-- sandbox ID to uuid and cannot read a short ID; the deploy replaces it.

-- The logs first: their rewrite is the long part (2 million rows of
-- activity took a minute in a test), and while it runs the previous
-- version's instances still read and write the sandboxes. Their writes
-- of activity and traffic records wait and fail after their lock_timeout:
-- the nodes send their records again, the recorder retries its batches
-- and then drops them, a transaction that records its write fails and is
-- retried by its caller or by the leader's loops. The sandboxes' tables
-- are locked from their change to the commit only, a short time. A
-- statement may take longer than the 5 minutes Migrate allows.
SET LOCAL statement_timeout = '30min';

ALTER TABLE activity
  ALTER COLUMN sandbox_id TYPE text COLLATE "C" USING sandbox_id::text,
  ALTER COLUMN pod_id TYPE text COLLATE "C" USING pod_id::text,
  ADD CONSTRAINT activity_sandbox_id_format CHECK (sandbox_id ~ '^([a-hjkmnp-tv-z][0-9a-hjkmnp-tv-z]{10}|[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})$'),
  ADD CONSTRAINT activity_pod_id_format CHECK (pod_id ~ '^([a-hjkmnp-tv-z][0-9a-hjkmnp-tv-z]{10}|[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})$');
ALTER TABLE traffic_records
  ALTER COLUMN pod_id TYPE text COLLATE "C" USING pod_id::text,
  ALTER COLUMN sandbox_id TYPE text COLLATE "C" USING sandbox_id::text,
  ADD CONSTRAINT traffic_records_pod_id_format CHECK (pod_id ~ '^([a-hjkmnp-tv-z][0-9a-hjkmnp-tv-z]{10}|[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})$'),
  ADD CONSTRAINT traffic_records_sandbox_id_format CHECK (sandbox_id ~ '^([a-hjkmnp-tv-z][0-9a-hjkmnp-tv-z]{10}|[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})$');

ALTER TABLE sandbox_tokens DROP CONSTRAINT sandbox_tokens_sandbox_id_fkey;
ALTER TABLE sandbox_holds DROP CONSTRAINT sandbox_holds_sandbox_id_fkey;
ALTER TABLE bookings DROP CONSTRAINT bookings_pod_id_fkey;
ALTER TABLE watches DROP CONSTRAINT watches_sandbox_id_fkey;
ALTER TABLE kube_bindings DROP CONSTRAINT kube_bindings_sandbox_id_fkey;
ALTER TABLE ssh_bindings DROP CONSTRAINT ssh_bindings_sandbox_id_fkey;
ALTER TABLE env_instances DROP CONSTRAINT env_instances_sandbox_id_fkey;

-- The TAP prefix gate: only UUIDs share a TAP device's name (pod- and the
-- first 8 hex digits); a short ID names its own. Made again below for the
-- UUIDs only, rather than rebuilt by the rewrite first.
DROP INDEX sandboxes_tap;

ALTER TABLE sandboxes
  ALTER COLUMN id TYPE text COLLATE "C" USING id::text,
  ADD CONSTRAINT sandboxes_id_format CHECK (id ~ '^([a-hjkmnp-tv-z][0-9a-hjkmnp-tv-z]{10}|[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})$');
CREATE INDEX sandboxes_tap ON sandboxes(node, left(id, 8)) WHERE node IS NOT NULL AND length(id) = 36;

ALTER TABLE sandbox_tokens
  ALTER COLUMN sandbox_id TYPE text COLLATE "C" USING sandbox_id::text,
  ADD CONSTRAINT sandbox_tokens_sandbox_id_fkey FOREIGN KEY (sandbox_id) REFERENCES sandboxes(id) ON DELETE CASCADE;
ALTER TABLE sandbox_holds
  ALTER COLUMN sandbox_id TYPE text COLLATE "C" USING sandbox_id::text,
  ADD CONSTRAINT sandbox_holds_sandbox_id_fkey FOREIGN KEY (sandbox_id) REFERENCES sandboxes(id) ON DELETE CASCADE;
ALTER TABLE bookings
  ALTER COLUMN pod_id TYPE text COLLATE "C" USING pod_id::text,
  ADD CONSTRAINT bookings_pod_id_fkey FOREIGN KEY (pod_id) REFERENCES sandboxes(id) ON DELETE CASCADE;
ALTER TABLE watches
  ALTER COLUMN sandbox_id TYPE text COLLATE "C" USING sandbox_id::text,
  ADD CONSTRAINT watches_sandbox_id_fkey FOREIGN KEY (sandbox_id) REFERENCES sandboxes(id) ON DELETE CASCADE;
ALTER TABLE kube_bindings
  ALTER COLUMN sandbox_id TYPE text COLLATE "C" USING sandbox_id::text,
  ADD CONSTRAINT kube_bindings_sandbox_id_fkey FOREIGN KEY (sandbox_id) REFERENCES sandboxes(id) ON DELETE CASCADE;
ALTER TABLE ssh_bindings
  ALTER COLUMN sandbox_id TYPE text COLLATE "C" USING sandbox_id::text,
  ADD CONSTRAINT ssh_bindings_sandbox_id_fkey FOREIGN KEY (sandbox_id) REFERENCES sandboxes(id) ON DELETE CASCADE;
ALTER TABLE env_instances
  ALTER COLUMN sandbox_id TYPE text COLLATE "C" USING sandbox_id::text,
  ADD CONSTRAINT env_instances_sandbox_id_fkey FOREIGN KEY (sandbox_id) REFERENCES sandboxes(id) ON DELETE CASCADE;

-- The IDs without a foreign key: of pods that may be gone.
ALTER TABLE pod_tombstones
  ALTER COLUMN pod_id TYPE text COLLATE "C" USING pod_id::text,
  ADD CONSTRAINT pod_tombstones_pod_id_format CHECK (pod_id ~ '^([a-hjkmnp-tv-z][0-9a-hjkmnp-tv-z]{10}|[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})$');
ALTER TABLE issued_certs
  ALTER COLUMN pod_id TYPE text COLLATE "C" USING pod_id::text,
  ADD CONSTRAINT issued_certs_pod_id_format CHECK (pod_id ~ '^([a-hjkmnp-tv-z][0-9a-hjkmnp-tv-z]{10}|[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})$');
ALTER TABLE env_job_runs
  ALTER COLUMN sandbox_id TYPE text COLLATE "C" USING sandbox_id::text,
  ADD CONSTRAINT env_job_runs_sandbox_id_format CHECK (sandbox_id ~ '^([a-hjkmnp-tv-z][0-9a-hjkmnp-tv-z]{10}|[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})$');
ALTER TABLE env_attached
  ALTER COLUMN sandbox_id TYPE text COLLATE "C" USING sandbox_id::text,
  ADD CONSTRAINT env_attached_sandbox_id_format CHECK (sandbox_id ~ '^([a-hjkmnp-tv-z][0-9a-hjkmnp-tv-z]{10}|[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})$');
ALTER TABLE env_volumes
  ALTER COLUMN holder TYPE text COLLATE "C" USING holder::text,
  ADD CONSTRAINT env_volumes_holder_format CHECK (holder ~ '^([a-hjkmnp-tv-z][0-9a-hjkmnp-tv-z]{10}|[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})$');

-- The placement audit, emptied and no longer written since 0003, which
-- said a later migration would drop it; this one does, rather than
-- converting its column.
DROP TABLE placements;
