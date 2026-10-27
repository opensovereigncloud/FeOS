-- SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
-- SPDX-License-Identifier: Apache-2.0

-- The schema of ferund, the control plane of FeOS (ADR 0015). Migrate applies
-- it once, in one transaction, under an advisory lock.

CREATE TABLE schema_migrations (
  version integer PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now());

CREATE TABLE cluster_secrets (                 -- shared by all instances; 'mcp-session' keys MCP session ids
  name text PRIMARY KEY, value bytea NOT NULL CHECK (length(value) = 32),
  created_at timestamptz NOT NULL DEFAULT now());

CREATE TABLE instances (
  id text PRIMARY KEY,
  address text NOT NULL DEFAULT '',            -- its front-door listen address, informational
  node_address text NOT NULL DEFAULT '',       -- --node-advertise-address; '' = not listed to nodes
  version text NOT NULL DEFAULT '',
  started_at timestamptz NOT NULL DEFAULT now(),
  last_seen timestamptz NOT NULL DEFAULT now(),
  stopping timestamptz);                       -- set at SIGTERM

CREATE TABLE leases (
  name text PRIMARY KEY, holder text NOT NULL,
  epoch bigint NOT NULL,                       -- +1 on every acquisition, also by the same holder
  acquired_at timestamptz NOT NULL, renewed_at timestamptz NOT NULL, expires_at timestamptz NOT NULL);

CREATE TABLE tenants (
  id text PRIMARY KEY CHECK (id ~ '^[a-z0-9][a-z0-9.-]{1,62}$'),   -- the tenant's name
  created_at timestamptz NOT NULL DEFAULT now(),
  deleting_at timestamptz);                    -- set by DELETE; the tenant goes when its sandboxes have
CREATE TABLE tenant_tokens (                   -- hashed only; shown once
  hash text PRIMARY KEY,                       -- sha256 hex
  tenant_id text NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  label text NOT NULL DEFAULT '', created_at timestamptz NOT NULL DEFAULT now(),
  last_used_at timestamptz);
CREATE INDEX tenant_tokens_tenant ON tenant_tokens(tenant_id, created_at);

CREATE TABLE nodes (                           -- never deleted: removal is a status
  name text PRIMARY KEY CHECK (name ~ '^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$'),
  status text NOT NULL DEFAULT 'joined' CHECK (status IN ('joined','ready','down','removed')),
  status_description text NOT NULL DEFAULT '',
  down_since timestamptz, removed_at timestamptz,
  draining boolean NOT NULL DEFAULT false, drain_started_at timestamptz,
  slots integer NOT NULL DEFAULT 50 CHECK (slots >= 0),
  mem_headroom_bytes bigint NOT NULL DEFAULT 1073741824 CHECK (mem_headroom_bytes >= 0),
  cpu_overcommit real NOT NULL DEFAULT 16 CHECK (cpu_overcommit >= 0),   -- floor(cpu_cores × this) = 0: CPU not gated
  hostname text NOT NULL DEFAULT '', arch text NOT NULL DEFAULT '', cpu_model text NOT NULL DEFAULT '',
  cpu_cores integer NOT NULL DEFAULT 0, memory_total_bytes bigint NOT NULL DEFAULT 0,
  disk_total_bytes bigint NOT NULL DEFAULT 0, versions jsonb NOT NULL DEFAULT '{}',
  object_store boolean NOT NULL DEFAULT false,
  object_store_id text NOT NULL DEFAULT '',    -- Telemetry.object_store_id; '' = none
  pod_prefix text NOT NULL DEFAULT '',
  start_id uuid,                               -- feosd's current start, from telemetry
  stopping_until timestamptz,                  -- rollout grace after a NodeEvent "stopping"
  current_serial text NOT NULL DEFAULT '',     -- newest certificate presented in a handshake
  identity_forked_at timestamptz,              -- a superseded certificate was presented
  joined_at timestamptz NOT NULL DEFAULT now(),
  reconciled_at timestamptz,
  reconcile_requested_at timestamptz);         -- cross-instance rate limit of inventory reconciles
CREATE TABLE node_telemetry (                  -- the hot row: ~100 updates/s at 512 nodes, never locked by placement
  node text PRIMARY KEY REFERENCES nodes(name),
  last_seen timestamptz NOT NULL,              -- DB clock at receipt: the heartbeat
  load1 real NOT NULL DEFAULT 0, memory_available_bytes bigint NOT NULL DEFAULT 0,
  disk_available_bytes bigint NOT NULL DEFAULT 0,
  pods_pending integer NOT NULL DEFAULT 0, pods_running integer NOT NULL DEFAULT 0,
  pods_suspended integer NOT NULL DEFAULT 0, pods_other integer NOT NULL DEFAULT 0,
  inventory_hash text NOT NULL DEFAULT '',
  inventory_mismatch_since timestamptz,        -- the node's hash differs from the registry's since
  reconciled_hash text NOT NULL DEFAULT '')    -- the node's hash the last reconcile saw
  WITH (fillfactor = 50);
CREATE INDEX node_telemetry_last_seen ON node_telemetry(last_seen);
CREATE TABLE node_certs (                      -- every node certificate issued: the node's renewal history
  serial text PRIMARY KEY,                     -- lowercase hex
  node text NOT NULL REFERENCES nodes(name),
  kind text NOT NULL CHECK (kind IN ('join','rejoin','renew')),
  key_hash text NOT NULL,                      -- sha256 hex of the key's SPKI DER
  previous_serial text NOT NULL DEFAULT '',    -- renew: the certificate that asked
  token_id uuid,                               -- join, rejoin: the token
  peer text NOT NULL DEFAULT '',               -- remote address of the issuing call
  not_before timestamptz NOT NULL, not_after timestamptz NOT NULL,
  issued_at timestamptz NOT NULL DEFAULT now(),
  first_seen_at timestamptz,                   -- first presented in a handshake
  revoked_at timestamptz,                      -- revoked from this time on (may lie ahead: superseded)
  revoked_reason text NOT NULL DEFAULT ''
    CHECK (revoked_reason IN ('','rejoined','superseded','removed','forked')));
CREATE INDEX node_certs_node ON node_certs(node, issued_at);
CREATE TABLE join_tokens (
  hash text PRIMARY KEY,                       -- sha256 hex of "feos-join-<secret>"
  id uuid NOT NULL UNIQUE DEFAULT gen_random_uuid(),
  created_by text NOT NULL DEFAULT '', created_at timestamptz NOT NULL DEFAULT now(),
  expires_at timestamptz NOT NULL,
  node_name text NOT NULL DEFAULT '',          -- pinned name; '' = creates a new name only
  used_at timestamptz, used_name text NOT NULL DEFAULT '',
  used_key_hash text NOT NULL DEFAULT '', used_serial text NOT NULL DEFAULT '',
  used_peer text NOT NULL DEFAULT '',
  CHECK ((used_at IS NULL) = (used_name = '')));

CREATE TABLE sandboxes (                       -- a row exists exactly while the sandbox exists (DELETE removes it)
  id uuid PRIMARY KEY,                         -- = the pod's ID on feosd
  incarnation uuid NOT NULL DEFAULT gen_random_uuid(),   -- the pod label ferun.incarnation
  owner text NOT NULL REFERENCES tenants(id),
  image text NOT NULL,                         -- as the client gave it
  image_ref text NOT NULL DEFAULT '',          -- resolved repo@sha256:..., fixed at the first materialisation
  vcpus integer NOT NULL CHECK (vcpus BETWEEN 1 AND 32),
  memory_mib integer NOT NULL CHECK (memory_mib BETWEEN 256 AND 65536),
  client_state text NOT NULL DEFAULT 'pending' CHECK (client_state IN ('pending','creating','running',
     'pausing','paused','resuming','hibernating','hibernated','stopped','failed','unknown','lost')),
  status_description text NOT NULL DEFAULT 'awaiting first tool call',
  node text REFERENCES nodes(name),            -- where its pod is or may be, or its HIBERNATED record; NULL: none
  materialised boolean NOT NULL DEFAULT false, -- a pod of this incarnation was seen on node
  lost_from text REFERENCES nodes(name),       -- lost grace: the node it was lost with
  avoid_nodes text[] NOT NULL DEFAULT '{}',    -- nodes where a create failed before a pod existed
  op text NOT NULL DEFAULT '' CHECK (op IN ('','create','start','resume','restore','pause','hibernate','repair')),
  op_id uuid, op_owner text NOT NULL DEFAULT '', op_deadline timestamptz,
  op_target text NOT NULL DEFAULT '',          -- the node the op calls
  op_from_state text NOT NULL DEFAULT '',      -- client_state before the op, for a rollback
  CHECK ((op = '') = (op_id IS NULL)),
  gen bigint NOT NULL DEFAULT 0,               -- +1 on every lifecycle write, never on activity writes
  checkpoint_epoch bigint NOT NULL DEFAULT 0, checkpoint_head text NOT NULL DEFAULT '',
  checkpoint_memory boolean NOT NULL DEFAULT false, checkpoint_at timestamptz,
  checkpoint_store text NOT NULL DEFAULT '',   -- object_store_id of the node that wrote the checkpoint
  pending_since timestamptz,                   -- observed PENDING without an op since
  unreachable_since timestamptz,               -- RUNNING but its guest unreachable since
  run_id text NOT NULL DEFAULT '',             -- the open run span, '' when none
  kube_version text NOT NULL DEFAULT '', ssh_version text NOT NULL DEFAULT '',
  created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now(),
  last_active_at timestamptz NOT NULL DEFAULT now(), paused_at timestamptz);
CREATE INDEX sandboxes_owner ON sandboxes(owner, created_at);
CREATE INDEX sandboxes_node ON sandboxes(node) WHERE node IS NOT NULL;
CREATE INDEX sandboxes_tap ON sandboxes(node, (left(id::text, 8))) WHERE node IS NOT NULL;
CREATE INDEX sandboxes_lost_from ON sandboxes(lost_from) WHERE lost_from IS NOT NULL;
CREATE INDEX sandboxes_op ON sandboxes(op_owner, op_deadline) WHERE op <> '';
CREATE INDEX sandboxes_idle ON sandboxes(last_active_at) WHERE client_state = 'running' AND op = '';
CREATE INDEX sandboxes_paused ON sandboxes(paused_at) WHERE client_state = 'paused' AND op = '';
CREATE INDEX sandboxes_attention ON sandboxes(client_state)
  WHERE client_state IN ('unknown','creating','pending') OR unreachable_since IS NOT NULL;
CREATE TABLE sandbox_tokens (
  hash text PRIMARY KEY,                       -- sha256 hex; the REST API shows and revokes by it
  sandbox_id uuid NOT NULL REFERENCES sandboxes(id) ON DELETE CASCADE,
  label text NOT NULL DEFAULT '', created_at timestamptz NOT NULL DEFAULT now(), last_used_at timestamptz);
CREATE INDEX sandbox_tokens_sandbox ON sandbox_tokens(sandbox_id, created_at);
CREATE TABLE sandbox_holds (                   -- what keeps a sandbox awake, across instances
  sandbox_id uuid NOT NULL REFERENCES sandboxes(id) ON DELETE CASCADE,
  holder text NOT NULL,                        -- '<instance>:exec', '<instance>:forward', 'process:<id>'
  kind text NOT NULL CHECK (kind IN ('exec','forward','process')),
  instance_id text NOT NULL DEFAULT '',        -- exec, forward: the instance that renews it
  lease_until timestamptz NOT NULL,            -- 'infinity': a process without a time limit
  PRIMARY KEY (sandbox_id, holder));
CREATE INDEX sandbox_holds_lease ON sandbox_holds(lease_until);
CREATE TABLE pod_tombstones (                  -- pods to delete from a node; also a placement gate
  pod_id uuid NOT NULL,
  node text NOT NULL REFERENCES nodes(name),
  incarnation uuid NOT NULL,                   -- the pod it kills; the nil UUID: a pod without the label
  reason text NOT NULL DEFAULT '',
  created_at timestamptz NOT NULL DEFAULT now(),
  not_before timestamptz NOT NULL DEFAULT now(),   -- not processed before (a live op may still create it)
  attempts integer NOT NULL DEFAULT 0, last_error text NOT NULL DEFAULT '', last_attempt_at timestamptz,
  PRIMARY KEY (pod_id, node, incarnation));
CREATE INDEX pod_tombstones_node ON pod_tombstones(node, not_before);
CREATE TABLE bookings (                        -- a pod's size on a node: what it uses there
  pod_id uuid PRIMARY KEY REFERENCES sandboxes(id) ON DELETE CASCADE,
  node text NOT NULL REFERENCES nodes(name),
  vcpus integer NOT NULL, memory_mib integer NOT NULL,
  gated boolean NOT NULL,                      -- false: booked because the pod uses the node, past the gates
  booked_at timestamptz NOT NULL DEFAULT now());
CREATE INDEX bookings_node ON bookings(node);
CREATE TABLE placements (                      -- audit of placement decisions
  id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY, pod_id uuid NOT NULL, chosen_node text NOT NULL,
  candidates jsonb NOT NULL,                   -- [{node, feasible, score, reason}]
  trigger text NOT NULL DEFAULT '', instance_id text NOT NULL,
  at timestamptz NOT NULL DEFAULT now());
CREATE INDEX placements_pod ON placements(pod_id, at);

CREATE TABLE node_starts (                     -- one feosd start; its row is the lock of its event ingest
  node text NOT NULL REFERENCES nodes(name), start_id uuid NOT NULL,
  first_seen timestamptz NOT NULL DEFAULT now(), last_seq bigint NOT NULL DEFAULT 0,
  last_event_at timestamptz, dropped bigint NOT NULL DEFAULT 0,   -- the node's count, as last reported
  PRIMARY KEY (node, start_id));
CREATE TABLE event_gaps (                      -- numbers neither stored nor counted as dropped by the node
  node text NOT NULL, start_id uuid NOT NULL, from_seq bigint NOT NULL, to_seq bigint NOT NULL,
  detected_at timestamptz NOT NULL DEFAULT now(), PRIMARY KEY (node, start_id, from_seq));
CREATE INDEX event_gaps_detected ON event_gaps(node, start_id, detected_at);

-- The activity log: the old system's event and span model. Partitioned by the day it was
-- recorded (DB clock, UTC); the leader keeps partitions for today and the next two days and
-- drops those past retention; every instance ensures today's and tomorrow's at start.
CREATE TABLE activity (
  day date NOT NULL DEFAULT (now() AT TIME ZONE 'utc')::date,
  id bigint GENERATED ALWAYS AS IDENTITY,
  ts_ns bigint NOT NULL,                       -- producer time; REST tsNs, ordering
  sandbox_id uuid,                             -- attributed sandbox; NULL: none
  pod_id uuid,                                 -- node records: the pod the record names, as sent
  foreign_pod boolean NOT NULL DEFAULT false,  -- a node record about a pod not placed on that node
  tenant_id text NOT NULL DEFAULT '',
  node text NOT NULL DEFAULT '', start_id uuid, seq bigint,   -- seq: node events only
  instance_id text NOT NULL DEFAULT '',        -- ferund's own rows
  source text NOT NULL CHECK (source IN ('ferund','feosd','authn')),
  kind text NOT NULL, category text NOT NULL DEFAULT '',
  request_id text NOT NULL DEFAULT '', span_id text NOT NULL DEFAULT '', parent_span_id text NOT NULL DEFAULT '',
  name text NOT NULL DEFAULT '', tool text NOT NULL DEFAULT '', params jsonb,
  exit_code bigint, duration_ns bigint, output text NOT NULL DEFAULT '',
  output_truncated boolean NOT NULL DEFAULT false, detail jsonb NOT NULL DEFAULT '{}',
  PRIMARY KEY (day, id)) PARTITION BY RANGE (day);
CREATE INDEX activity_sandbox ON activity(sandbox_id, ts_ns) WHERE sandbox_id IS NOT NULL;
CREATE INDEX activity_node ON activity(node, ts_ns) WHERE node <> '';
CREATE INDEX activity_span ON activity(span_id) WHERE span_id <> '';
CREATE INDEX activity_parent ON activity(parent_span_id) WHERE parent_span_id <> '';
CREATE INDEX activity_request ON activity(request_id) WHERE request_id <> '';
CREATE INDEX activity_authn ON activity(sandbox_id, kind, ts_ns) WHERE source = 'authn';
CREATE UNIQUE INDEX activity_seq ON activity(node, start_id, seq, day) WHERE seq IS NOT NULL;  -- backstop
CREATE TABLE traffic_records (                 -- ADR 0013, from the uplink; same partitioning, shorter retention
  day date NOT NULL DEFAULT (now() AT TIME ZONE 'utc')::date,
  id bigint GENERATED ALWAYS AS IDENTITY, ts_ns bigint NOT NULL,
  node text NOT NULL, start_id uuid NOT NULL, seq bigint NOT NULL,
  pod_id uuid,                                 -- as sent
  sandbox_id uuid,                             -- attributed; NULL for a foreign record
  foreign_pod boolean NOT NULL DEFAULT false,
  tenant_id text NOT NULL DEFAULT '',
  kind text NOT NULL CHECK (kind IN ('flow','drop','dns','server_name')), missed bigint NOT NULL DEFAULT 0,
  detail jsonb NOT NULL,                       -- protojson (UseProtoNames) of the TrafficRecord
  PRIMARY KEY (day, id)) PARTITION BY RANGE (day);
CREATE INDEX traffic_sandbox ON traffic_records(sandbox_id, ts_ns) WHERE sandbox_id IS NOT NULL;
CREATE UNIQUE INDEX traffic_seq ON traffic_records(node, start_id, seq, day);           -- backstop

CREATE TABLE issued_certs (                    -- pod certificates (ADR 0014)
  serial text PRIMARY KEY, pod_id uuid NOT NULL, incarnation uuid NOT NULL, node text NOT NULL,
  spiffe_id text NOT NULL,
  hpke_public_key bytea NOT NULL,              -- the run's key: what a later re-seal for a running pod needs
  bindings_version text NOT NULL DEFAULT '',
  not_after timestamptz NOT NULL, issued_at timestamptz NOT NULL DEFAULT now());
CREATE INDEX issued_certs_pod ON issued_certs(pod_id, issued_at);

CREATE TABLE watches (
  id uuid PRIMARY KEY, sandbox_id uuid NOT NULL REFERENCES sandboxes(id) ON DELETE CASCADE,
  container text NOT NULL DEFAULT 'main',
  paths text[] NOT NULL DEFAULT '{}', gen bigint NOT NULL DEFAULT 0,       -- path-set generation
  expires_at timestamptz NOT NULL,                                          -- the lease (2 min)
  owner text NOT NULL DEFAULT '', owner_until timestamptz, epoch bigint NOT NULL DEFAULT 0,
  result jsonb, result_gen bigint NOT NULL DEFAULT 0,                       -- the owner's last ack
  created_at timestamptz NOT NULL DEFAULT now());
CREATE INDEX watches_sandbox ON watches(sandbox_id);
CREATE INDEX watches_expires ON watches(expires_at);

CREATE TABLE kube_bindings (
  sandbox_id uuid NOT NULL REFERENCES sandboxes(id) ON DELETE CASCADE,
  name text NOT NULL, port integer NOT NULL CHECK (port BETWEEN 1024 AND 65535),
  server text NOT NULL, server_name text NOT NULL DEFAULT '', ca_pem text NOT NULL DEFAULT '',
  auth_type text NOT NULL CHECK (auth_type IN ('token','client_cert')),
  sealed bytea NOT NULL,                       -- 0x01 || nonce(12) || AES-256-GCM(credential JSON)
  fingerprint text NOT NULL,                   -- "sha256:<16 hex>"
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (sandbox_id, port), UNIQUE (sandbox_id, name));
CREATE TABLE ssh_bindings (
  sandbox_id uuid NOT NULL REFERENCES sandboxes(id) ON DELETE CASCADE,
  name text NOT NULL,
  server text NOT NULL,                        -- ssh://host[:port]; '' = any server
  ssh_user text NOT NULL,
  host_keys text[] NOT NULL DEFAULT '{}',      -- "<type> <base64>"
  auth_type text NOT NULL CHECK (auth_type IN ('private_key','generate')),
  key_type text NOT NULL, fingerprint text NOT NULL,   -- "SHA256:..."
  public_key text NOT NULL,
  sealed bytea NOT NULL,                       -- 0x01 || nonce(12) || AES-256-GCM({"private_key":...})
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (sandbox_id, name));
