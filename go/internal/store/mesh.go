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

	"github.com/gardenlinux/feos/go/internal/db"
	"github.com/gardenlinux/feos/go/internal/mesh"
)

// HintMeshPeers is the ferun_leader hint of a change of the mesh's peers:
// it wakes fleet's loop of that name, which sends the nodes their peers.
const HintMeshPeers = "mesh-peers"

// ErrMeshExhausted is JoinMesh's and JoinIngressMesh's error when every
// subnet ID of the pod network has been handed out.
var ErrMeshExhausted = errors.New("every /96 of the pod network has been handed out")

// ErrMeshKeyTaken is JoinMesh's and JoinIngressMesh's error for a
// WireGuard key that another node or ingress holds: a key is one peer's,
// since the nodes' devices keep one peer per key, and two peers with one
// key would take each other's prefix and endpoint there.
var ErrMeshKeyTaken = errors.New("the WireGuard key is another peer's")

// MeshPeer reports whether the node is a peer of the mesh: it has its /96,
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

// InsertPodNetwork makes the cluster meshed with ula, a /64 of fd00::/8.
func (s *Store) InsertPodNetwork(ctx context.Context, ula netip.Prefix) error {
	_, err := s.q.Exec(ctx, "INSERT INTO pod_network (ula) VALUES ($1)", ula)
	return err
}

// MeshJoin is what JoinMesh made of a node: its subnet ID and /96, and
// whether its subnet, key or endpoint changed, which bumped the peers'
// version.
type MeshJoin struct {
	Subnet  int
	Prefix  string
	Changed bool
}

// JoinMesh puts node n on the mesh of ula with its WireGuard key and
// endpoint: a node without a subnet ID gets the next one of the sequence,
// which never hands one out twice (ErrMeshExhausted when none is left). A
// new key that another node or an ingress holds is ErrMeshKeyTaken
// (checkMeshKey). When the subnet, key or endpoint changed, the peers'
// version goes up and the leader is woken (HintMeshPeers). The caller
// holds n's row FOR UPDATE, read in the same transaction.
func (s *Store) JoinMesh(ctx context.Context, n Node, ula netip.Prefix, key []byte, endpoint string) (MeshJoin, error) {
	if !bytes.Equal(key, n.MeshPublicKey) {
		if err := s.checkMeshKey(ctx, key, n.Name, ""); err != nil {
			return MeshJoin{}, err
		}
	}
	j := MeshJoin{Subnet: n.MeshSubnet}
	if j.Subnet == 0 {
		next, err := s.nextMeshSubnet(ctx)
		if err != nil {
			return MeshJoin{}, err
		}
		j.Subnet = next
	}
	// The prefix follows from ula and the ID; it differs from the row's
	// only for a new ID, or after the ULA changed, as from a /48 to its
	// /64 (ADR 0017).
	j.Prefix = mesh.NodePrefix(ula, j.Subnet).String()
	j.Changed = j.Subnet != n.MeshSubnet || j.Prefix != n.MeshPrefix || !bytes.Equal(key, n.MeshPublicKey) ||
		endpoint != n.MeshEndpoint
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

// checkMeshKey is ErrMeshKeyTaken when a node other than node or an
// ingress other than ingress ("" for none) holds key; removed ones hold
// none. It takes pod_network's row lock first, which every writer of a
// new key takes before it checks and holds until it commits, so that two
// peers never get one key, whatever the order of their transactions. The
// caller holds its own row's lock, which comes before pod_network's.
func (s *Store) checkMeshKey(ctx context.Context, key []byte, node, ingress string) error {
	if _, err := s.q.Exec(ctx, "SELECT 1 FROM pod_network FOR UPDATE"); err != nil {
		return err
	}
	var taken bool
	err := s.q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM nodes WHERE mesh_public_key = $1 AND name <> $2 AND status <> 'removed')
		OR EXISTS (SELECT 1 FROM ingresses WHERE mesh_public_key = $1 AND name <> $3 AND status <> 'removed')`,
		key, node, ingress).Scan(&taken)
	if err != nil {
		return err
	}
	if taken {
		return ErrMeshKeyTaken
	}
	return nil
}

// nextMeshSubnet takes the next subnet ID of the sequence mesh_subnets,
// which nodes and ingresses share and which never hands one out twice;
// ErrMeshExhausted when none is left.
func (s *Store) nextMeshSubnet(ctx context.Context) (int, error) {
	var next int64
	if err := s.q.QueryRow(ctx, "SELECT nextval('mesh_subnets')").Scan(&next); err != nil {
		var pe *pgconn.PgError
		if errors.As(err, &pe) && pe.Code == "2200H" { // sequence_generator_limit_exceeded
			return 0, ErrMeshExhausted
		}
		return 0, err
	}
	return int(next), nil
}

// bumpMeshPeers counts a change of the mesh's peers, wakes the leader and
// tells the ingress's table builders (ferun_ingress IngressPayloadMesh);
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
	if err := s.Notify(ctx, db.ChanLeader, HintMeshPeers); err != nil {
		return 0, err
	}
	return uint64(v), s.Notify(ctx, db.ChanIngress, IngressPayloadMesh)
}

// MeshPeers returns the mesh's peers at their version, in one snapshot:
// every node that is a peer (MeshPeer) and not removed, down ones too, by
// its name, and every ingress that is a peer (Ingress.MeshPeer) and not
// removed, by mesh.IngressPeerName, with its /96 as the pod prefix. A
// cluster that is not meshed has none, at version 0.
func (s *Store) MeshPeers(ctx context.Context) (mesh.Peers, error) {
	rows, err := s.q.Query(ctx, `SELECT p.peers_version, m.ingress, m.name, m.mesh_public_key, m.mesh_endpoint, m.mesh_prefix
		FROM pod_network p LEFT JOIN (
			SELECT false AS ingress, n.name, n.mesh_public_key, n.mesh_endpoint, n.mesh_prefix FROM nodes n
				WHERE n.mesh_subnet IS NOT NULL AND n.mesh_public_key IS NOT NULL AND n.mesh_endpoint <> ''
					AND n.status <> 'removed'
			UNION ALL
			SELECT true, g.name, g.mesh_public_key, g.mesh_endpoint, g.mesh_prefix FROM ingresses g
				WHERE g.mesh_subnet IS NOT NULL AND g.mesh_public_key IS NOT NULL AND g.mesh_endpoint <> ''
					AND g.status <> 'removed') m ON true`)
	if err != nil {
		return mesh.Peers{}, err
	}
	defer rows.Close()
	peers := mesh.Peers{Nodes: map[string]mesh.Peer{}}
	for rows.Next() {
		var version int64
		var ingress *bool
		var name, endpoint, prefix *string
		var key []byte
		if err := rows.Scan(&version, &ingress, &name, &key, &endpoint, &prefix); err != nil {
			return mesh.Peers{}, err
		}
		peers.Version = uint64(version)
		if ingress != nil && name != nil && endpoint != nil && prefix != nil {
			k := *name
			if *ingress {
				k = mesh.IngressPeerName(k)
			}
			peers.Nodes[k] = mesh.Peer{Node: k, PublicKey: key, Endpoint: *endpoint, PodPrefix: *prefix}
		}
	}
	return peers, rows.Err()
}
