-- SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
-- SPDX-License-Identifier: Apache-2.0

-- The ingress (ADR 0021): ferun-ingress's instances, a second kind of peer
-- on the pods' mesh. An instance joins with a one-time join token of kind
-- 'ingress', holds certificates spiffe://<td>/ingress/<name>, and takes a
-- /64 of the cluster's ULA from the nodes' sequence mesh_subnets, which
-- never hands a subnet ID out twice, to nodes and ingresses alike. Its
-- address, <prefix>::1, is a member of the security group ferun-ingress,
-- whose version is pod_network.ingress_group_version: a counter bumped
-- under its row lock in the transaction that changes the members, so that
-- versions follow commit order. The pods of an environment that was ever
-- public name their groups ing-<sandbox id>, whose rules let the ingress in
-- while the environment is public. An environment's default host label is
-- given at its first publish and never handed to another tenant
-- (host_labels). One WireGuard key belongs to one peer: a writer of a
-- node's or an ingress's key checks under pod_network's row lock that no
-- other one that is not removed holds it.
--
-- Additive: an instance of the previous version, which a rolling deploy
-- runs beside this one, never reads the new tables and columns, inserts
-- join tokens of kind 'node' and environments that are not public, and
-- deletes environments, which releases their labels (ON DELETE SET NULL).

CREATE TABLE ingresses (                       -- never deleted: removal is a status
  name text PRIMARY KEY CHECK (name ~ '^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$'),
  status text NOT NULL DEFAULT 'joined' CHECK (status IN ('joined','removed')),
  joined_at timestamptz NOT NULL DEFAULT now(),
  removed_at timestamptz,
  mesh_subnet integer UNIQUE CHECK (mesh_subnet BETWEEN 1 AND 65535),   -- from mesh_subnets, as nodes'
  mesh_prefix text NOT NULL DEFAULT '',        -- the /64 of mesh_subnet; '' without one
  mesh_address text NOT NULL DEFAULT '',       -- <prefix>::1, its member of ferun-ingress; '' without one
  mesh_public_key bytea CHECK (octet_length(mesh_public_key) = 32),   -- fixed from the first Hello after a join; a re-join clears it; no node or other ingress that is not removed holds it
  previous_mesh_public_key bytea CHECK (octet_length(previous_mesh_public_key) = 32),   -- the key the last re-join cleared: the next Hello's must differ
  mesh_endpoint text NOT NULL DEFAULT '',      -- host:port of its WireGuard device
  group_version bigint NOT NULL DEFAULT 0 CHECK (group_version >= 0),   -- pod_network.ingress_group_version at its address's last entry into or exit from ferun-ingress
  current_serial text NOT NULL DEFAULT '',     -- the newest of its certificates presented in a Hello
  version text NOT NULL DEFAULT '',            -- ferun-ingress's version, from Hello
  last_seen timestamptz,                       -- DB clock of its last Hello, Sync heartbeat or Hold
  left_at timestamptz,                         -- it stopped cleanly (Hold leaving) and was not seen since
  CHECK ((status = 'removed') = (removed_at IS NOT NULL)),
  CHECK ((mesh_subnet IS NULL) = (mesh_prefix = '')),
  CHECK ((mesh_prefix = '') = (mesh_address = '')));

ALTER TABLE pod_network
  ADD COLUMN ingress_group_version bigint NOT NULL DEFAULT 0 CHECK (ingress_group_version >= 0);   -- the version of ferun-ingress: +1 per change of its members, under this row's lock

CREATE TABLE ingress_certs (                   -- every ingress certificate issued
  serial text PRIMARY KEY,                     -- lowercase hex
  ingress text NOT NULL REFERENCES ingresses(name),
  kind text NOT NULL CHECK (kind IN ('join','rejoin','renew')),
  key_hash text NOT NULL,                      -- sha256 hex of the key's SPKI DER
  previous_serial text NOT NULL DEFAULT '',    -- renew: the certificate that asked
  token_id uuid,                               -- join, rejoin: the token
  peer text NOT NULL DEFAULT '',               -- remote address of the issuing call
  not_before timestamptz NOT NULL,
  not_after timestamptz NOT NULL,
  issued_at timestamptz NOT NULL DEFAULT now(),
  revoked_at timestamptz,                      -- may lie ahead: superseded, in its grace
  revoked_reason text NOT NULL DEFAULT '' CHECK (revoked_reason IN ('','rejoined','removed','superseded')));
CREATE INDEX ingress_certs_ingress ON ingress_certs(ingress, issued_at);

ALTER TABLE join_tokens
  ADD COLUMN kind text NOT NULL DEFAULT 'node' CHECK (kind IN ('node','ingress'));   -- node_name: the pinned name of either kind

ALTER TABLE nodes
  ADD COLUMN ingress_group_version bigint NOT NULL DEFAULT 0;   -- the version of ferun-ingress the node holds, as last put or listed; 0: none

ALTER TABLE env_instances
  ADD COLUMN ingress_ports integer[] NOT NULL DEFAULT '{}';     -- TCP ports its group ing-<sandbox id> opens to the ingress, as last put on or listed by its node

ALTER TABLE environments
  ADD COLUMN public boolean NOT NULL DEFAULT false,             -- served by ferun-ingress at its default host, without a route token: its tenant's decision
  ADD COLUMN ingress_group boolean NOT NULL DEFAULT false,      -- its image services' pods name ing-<sandbox id>: set at the first publish, never cleared
  ADD COLUMN host_label text CHECK (host_label ~ '^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$' AND substr(host_label, 3, 2) <> '--'),   -- its default host's label under the apps zone; NULL: never published
  ADD CONSTRAINT environments_public_ready CHECK (NOT public OR (ingress_group AND host_label IS NOT NULL));
CREATE UNIQUE INDEX environments_host_label ON environments(host_label);   -- NULLs are distinct

CREATE TABLE host_labels (                     -- every default host label ever given, and whose it is
  label text PRIMARY KEY CHECK (label ~ '^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$' AND substr(label, 3, 2) <> '--'),
  owner text NOT NULL,                         -- the tenant that first claimed it; only it may claim it again
  env_id uuid UNIQUE REFERENCES environments(id) ON DELETE SET NULL,   -- the environment that has it; NULL: free for its owner
  assigned_at timestamptz NOT NULL DEFAULT now());
