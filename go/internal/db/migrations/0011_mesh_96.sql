-- SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
-- SPDX-License-Identifier: Apache-2.0


-- ADR 0017's /96s: a meshed cluster's ULA is a /64, and every node and
-- ingress instance has a /96 of it, its ID from mesh_subnets in bits 64 to
-- 95, as GCP gives a VM a /96 of its subnet's /64. A cluster meshed on a
-- /48 keeps its Global ID: its ULA becomes the /64 of subnet 0 of the /48,
-- which was never given out, and every node and ingress keeps its number,
-- now the ID of a /96 of that /64. No new prefix shares an address with an
-- old /64. The peers' version and ferun-ingress's are bumped, so that the
-- leader sends every node the new prefixes and addresses; a node takes its
-- own new /96 from JoinMesh, at feosd's next start.
--
-- Not additive: an instance of the previous version reads the /64 as the
-- /48 it expects. Nodes keep their rows' prefixes from it, but its Hello
-- writes an ingress's /64 back; the next Hello to an instance of this
-- version writes the /96 again, and every ingress says Hello anew once the
-- previous version's instances are gone.

ALTER TABLE pod_network DROP CONSTRAINT pod_network_ula_check;
UPDATE pod_network SET ula = set_masklen(ula, 64) WHERE masklen(ula) = 48;
ALTER TABLE pod_network ADD CONSTRAINT pod_network_ula_check
  CHECK (family(ula) = 6 AND masklen(ula) = 64 AND ula << 'fd00::/8'::cidr);

ALTER SEQUENCE mesh_subnets AS bigint MAXVALUE 4294967295;

ALTER TABLE nodes DROP CONSTRAINT nodes_mesh_subnet_check;
ALTER TABLE nodes ALTER COLUMN mesh_subnet TYPE bigint;
ALTER TABLE nodes ADD CONSTRAINT nodes_mesh_subnet_check CHECK (mesh_subnet BETWEEN 1 AND 4294967295);
ALTER TABLE ingresses DROP CONSTRAINT ingresses_mesh_subnet_check;
ALTER TABLE ingresses ALTER COLUMN mesh_subnet TYPE bigint;
ALTER TABLE ingresses ADD CONSTRAINT ingresses_mesh_subnet_check CHECK (mesh_subnet BETWEEN 1 AND 4294967295);

-- A /96 of the ULA, as mesh.NodePrefix writes it: the ID in bits 64 to 95.
CREATE FUNCTION pg_temp.mesh_prefix(subnet bigint) RETURNS text LANGUAGE sql STABLE AS $$
  SELECT text(set_masklen(set_masklen(ula, 128)::inet + ($1 << 32), 96)) FROM pod_network
$$;

UPDATE nodes SET mesh_prefix = pg_temp.mesh_prefix(mesh_subnet)
  WHERE mesh_subnet IS NOT NULL AND EXISTS (SELECT 1 FROM pod_network);
UPDATE ingresses SET mesh_prefix = pg_temp.mesh_prefix(mesh_subnet),
    mesh_address = host(set_masklen(pg_temp.mesh_prefix(mesh_subnet)::inet, 128) + 1)
  WHERE mesh_subnet IS NOT NULL AND EXISTS (SELECT 1 FROM pod_network);

UPDATE pod_network SET peers_version = peers_version + 1,
  ingress_group_version = ingress_group_version + 1;
UPDATE ingresses SET group_version = (SELECT ingress_group_version FROM pod_network)
  WHERE mesh_subnet IS NOT NULL AND EXISTS (SELECT 1 FROM pod_network);
DROP FUNCTION pg_temp.mesh_prefix(bigint);
