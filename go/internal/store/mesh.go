// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"bytes"
	"context"
	"errors"
	"net/netip"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ironcore-dev/feos/go/internal/db"
	"github.com/ironcore-dev/feos/go/internal/mesh"
)

// HintMeshPeers is the ferun_leader hint of a change of the mesh's peers:
// it wakes fleet's loop of that name, which sends the nodes their peers.
const HintMeshPeers = "mesh-peers"

// ErrMeshExhausted is JoinMesh's error when every subnet ID of the pod
// network has been handed out.
var ErrMeshExhausted = errors.New("every /64 of the pod network has been handed out")

// MeshPeer reports whether the node is a peer of the mesh: it has its /64,
// a key and an endpoint.
func (n *Node) MeshPeer() bool {
	return n.MeshSubnet != 0 && len(n.MeshPublicKey) == mesh.KeyLen && n.MeshEndpoint != ""
}

// PodNetwork returns the cluster's ULA, and false when the cluster is not
// meshed.
func (s *Store) PodNetwork(ctx context.Context) (netip.Prefix, bool, error) {
	var ula netip.Prefix
	err := s.q.QueryRow(ctx, "SELECT ula FROM pod_network").Scan(&ula)
	if errors.Is(err, pgx.ErrNoRows) {
		return netip.Prefix{}, false, nil
	}
	if err != nil {
		return netip.Prefix{}, false, err
	}
	return ula, true, nil
}

// InsertPodNetwork makes the cluster meshed with ula, a /48 of fd00::/8.
func (s *Store) InsertPodNetwork(ctx context.Context, ula netip.Prefix) error {
	_, err := s.q.Exec(ctx, "INSERT INTO pod_network (ula) VALUES ($1)", ula)
	return err
}

// MeshJoin is what JoinMesh made of a node: its subnet ID and /64, and
// whether its subnet, key or endpoint changed, which bumped the peers'
// version.
type MeshJoin struct {
	Subnet  int
	Prefix  string
	Changed bool
}

// JoinMesh puts node n on the mesh of ula with its WireGuard key and
// endpoint: a node without a subnet ID gets the next one of the sequence,
// which never hands one out twice (ErrMeshExhausted when none is left).
// When the subnet, key or endpoint changed, the peers' version goes up and
// the leader is woken (HintMeshPeers). The caller holds n's row FOR
// UPDATE, read in the same transaction.
func (s *Store) JoinMesh(ctx context.Context, n Node, ula netip.Prefix, key []byte, endpoint string) (MeshJoin, error) {
	j := MeshJoin{Subnet: n.MeshSubnet, Prefix: n.MeshPrefix}
	if j.Subnet == 0 {
		var next int64
		if err := s.q.QueryRow(ctx, "SELECT nextval('mesh_subnets')").Scan(&next); err != nil {
			var pe *pgconn.PgError
			if errors.As(err, &pe) && pe.Code == "2200H" { // sequence_generator_limit_exceeded
				return MeshJoin{}, ErrMeshExhausted
			}
			return MeshJoin{}, err
		}
		j.Subnet, j.Prefix = int(next), mesh.NodePrefix(ula, int(next)).String()
	}
	j.Changed = j.Subnet != n.MeshSubnet || !bytes.Equal(key, n.MeshPublicKey) || endpoint != n.MeshEndpoint
	if !j.Changed {
		return j, nil
	}
	if _, err := s.q.Exec(ctx, `UPDATE nodes SET mesh_subnet = $2, mesh_prefix = $3, mesh_public_key = $4,
		mesh_endpoint = $5 WHERE name = $1`, n.Name, j.Subnet, j.Prefix, key, endpoint); err != nil {
		return MeshJoin{}, err
	}
	if _, err := s.bumpMeshPeers(ctx); err != nil {
		return MeshJoin{}, err
	}
	return j, nil
}

// bumpMeshPeers counts a change of the mesh's peers and wakes the leader;
// it returns the new version, 0 when the cluster is not meshed.
func (s *Store) bumpMeshPeers(ctx context.Context) (uint64, error) {
	var v int64
	err := s.q.QueryRow(ctx, "UPDATE pod_network SET peers_version = peers_version + 1 RETURNING peers_version").Scan(&v)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return uint64(v), s.Notify(ctx, db.ChanLeader, HintMeshPeers)
}

// MeshPeers returns the mesh's peers at their version, in one snapshot:
// every node that is a peer (MeshPeer) and not removed, down ones too. A
// cluster that is not meshed has none, at version 0.
func (s *Store) MeshPeers(ctx context.Context) (mesh.Peers, error) {
	rows, err := s.q.Query(ctx, `SELECT p.peers_version, n.name, n.mesh_public_key, n.mesh_endpoint, n.mesh_prefix
		FROM pod_network p LEFT JOIN nodes n ON n.mesh_subnet IS NOT NULL AND n.mesh_public_key IS NOT NULL
			AND n.mesh_endpoint <> '' AND n.status <> 'removed'`)
	if err != nil {
		return mesh.Peers{}, err
	}
	defer rows.Close()
	peers := mesh.Peers{Nodes: map[string]mesh.Peer{}}
	for rows.Next() {
		var version int64
		var name, endpoint, prefix *string
		var key []byte
		if err := rows.Scan(&version, &name, &key, &endpoint, &prefix); err != nil {
			return mesh.Peers{}, err
		}
		peers.Version = uint64(version)
		if name != nil && endpoint != nil && prefix != nil {
			peers.Nodes[*name] = mesh.Peer{Node: *name, PublicKey: key, Endpoint: *endpoint, PodPrefix: *prefix}
		}
	}
	return peers, rows.Err()
}
