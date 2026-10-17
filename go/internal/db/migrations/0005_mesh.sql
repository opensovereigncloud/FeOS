-- SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
-- SPDX-License-Identifier: Apache-2.0

-- The pods' mesh (ADR 0017): a meshed cluster's ULA, a /48, and every
-- node's /64 of it, whose subnet ID comes from a sequence and is never
-- handed out twice, not after its node was removed or joined anew either.
-- A node's WireGuard key and endpoint make it a peer of the others; the
-- peers' version counts every change of who they are.

CREATE TABLE pod_network (                     -- one row, while the cluster is meshed
  id boolean PRIMARY KEY DEFAULT true CHECK (id),
  ula cidr NOT NULL CHECK (family(ula) = 6 AND masklen(ula) = 48 AND ula << 'fd00::/8'::cidr),
  peers_version bigint NOT NULL DEFAULT 0 CHECK (peers_version >= 0),
  created_at timestamptz NOT NULL DEFAULT now());

CREATE SEQUENCE mesh_subnets AS integer MINVALUE 1 MAXVALUE 65535 NO CYCLE;

ALTER TABLE nodes
  ADD COLUMN mesh_subnet integer UNIQUE CHECK (mesh_subnet BETWEEN 1 AND 65535),
  ADD COLUMN mesh_prefix text NOT NULL DEFAULT '',     -- the /64 of mesh_subnet; '' without one
  ADD COLUMN mesh_public_key bytea CHECK (octet_length(mesh_public_key) = 32),
  ADD COLUMN mesh_endpoint text NOT NULL DEFAULT '';   -- host:port of its WireGuard device
