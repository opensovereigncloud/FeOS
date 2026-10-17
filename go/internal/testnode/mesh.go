// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package testnode

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"sort"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	fleetv1 "github.com/ironcore-dev/feos/go/gen/feos/fleet/v1"
	networkv1 "github.com/ironcore-dev/feos/go/gen/feos/network/v1"
)

// joinMeshTimeout bounds a JoinMesh on the uplink.
const joinMeshTimeout = 3 * time.Second

// meshService is the node on the pods' mesh (ADR 0017), as feosd is with
// Config.Mesh: its key, made at its start, and its endpoint, which it asks
// JoinMesh for on its uplink; the ULA and the /64 the answer gives it; and
// its peers, which the control plane sends over the node connection, whole
// or as a delta from the version the node has. Without Config.Mesh the
// node is not on a mesh: MeshService answers FAILED_PRECONDITION, and the
// node asks JoinMesh nothing.
type meshService struct {
	networkv1.UnimplementedMeshServiceServer
	n *Node

	mu         sync.Mutex
	key        []byte
	endpoint   string
	asked      string // the endpoint of the last JoinMesh answered on this uplink; "" asks again
	podNetwork string
	podPrefix  string
	version    uint64
	peers      map[string]*networkv1.MeshPeer
	changed    chan struct{} // closed and replaced on every change
}

func newMeshService(n *Node) (*meshService, error) {
	m := &meshService{n: n, peers: map[string]*networkv1.MeshPeer{}, changed: make(chan struct{})}
	if !n.cfg.Mesh {
		return m, nil
	}
	m.key = make([]byte, 32)
	if _, err := rand.Read(m.key); err != nil {
		return nil, fmt.Errorf("testnode: the mesh's key: %w", err)
	}
	m.endpoint = n.cfg.MeshEndpoint
	if m.endpoint == "" {
		h := sha256.Sum256([]byte(n.cfg.Name))
		m.endpoint = fmt.Sprintf("127.0.0.1:%d", 20000+(int(h[0])<<8|int(h[1]))%40000)
	}
	return m, nil
}

// changedLocked wakes those that wait for a change.
func (m *meshService) changedLocked() {
	close(m.changed)
	m.changed = make(chan struct{})
}

func (m *meshService) PutMeshPeers(_ context.Context, req *networkv1.PutMeshPeersRequest) (*networkv1.PutMeshPeersResponse, error) {
	if !m.n.cfg.Mesh {
		return nil, status.Error(codes.FailedPrecondition, "the node is not on a mesh")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.version = req.GetVersion()
	m.peers = map[string]*networkv1.MeshPeer{}
	for _, p := range req.GetPeers() {
		m.peers[p.GetNode()] = proto.Clone(p).(*networkv1.MeshPeer)
	}
	m.changedLocked()
	return &networkv1.PutMeshPeersResponse{}, nil
}

func (m *meshService) UpdateMeshPeers(_ context.Context, req *networkv1.UpdateMeshPeersRequest) (*networkv1.UpdateMeshPeersResponse, error) {
	if !m.n.cfg.Mesh {
		return nil, status.Error(codes.FailedPrecondition, "the node is not on a mesh")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if req.GetFromVersion() != m.version {
		return nil, status.Errorf(codes.FailedPrecondition, "the node has the peers at version %d, not %d",
			m.version, req.GetFromVersion())
	}
	for _, p := range req.GetPutPeers() {
		m.peers[p.GetNode()] = proto.Clone(p).(*networkv1.MeshPeer)
	}
	for _, name := range req.GetRemovePeers() {
		delete(m.peers, name)
	}
	m.version = req.GetVersion()
	m.changedLocked()
	return &networkv1.UpdateMeshPeersResponse{}, nil
}

func (m *meshService) GetMesh(context.Context, *networkv1.GetMeshRequest) (*networkv1.Mesh, error) {
	return m.snapshot(), nil
}

// snapshot is the node's mesh, its peers sorted by name.
func (m *meshService) snapshot() *networkv1.Mesh {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := &networkv1.Mesh{PodNetwork: m.podNetwork, PodPrefix: m.podPrefix, PublicKey: append([]byte(nil), m.key...),
		Version: m.version}
	if m.n.cfg.Mesh {
		out.Port = 51820
	}
	for _, p := range m.peers {
		out.Peers = append(out.Peers, proto.Clone(p).(*networkv1.MeshPeer))
	}
	sort.Slice(out.Peers, func(i, j int) bool { return out.Peers[i].GetNode() < out.Peers[j].GetNode() })
	return out
}

// newUplink makes the node ask JoinMesh again on its next report: its
// uplink is new.
func (m *meshService) newUplink() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.asked = ""
}

// join asks JoinMesh on the uplink of client when the node is on a mesh
// and has not asked it there with its endpoint yet.
func (m *meshService) join(ctx context.Context, client fleetv1.FleetServiceClient) error {
	if !m.n.cfg.Mesh {
		return nil
	}
	m.mu.Lock()
	key, endpoint, asked := m.key, m.endpoint, m.asked
	m.mu.Unlock()
	if asked == endpoint {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, joinMeshTimeout)
	defer cancel()
	resp, err := client.JoinMesh(ctx, &fleetv1.JoinMeshRequest{PublicKey: key, Endpoint: endpoint})
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.podNetwork, m.podPrefix = resp.GetPodNetwork(), resp.GetPodPrefix()
	if m.endpoint == endpoint {
		m.asked = endpoint
	}
	m.changedLocked()
	return nil
}

// prefix is the node's /64 of the mesh, "" before JoinMesh answered.
func (m *meshService) prefix() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.podPrefix
}

// Mesh returns the node's mesh: the ULA and the /64 JoinMesh gave it, its
// key, and the peers at their version.
func (n *Node) Mesh() *networkv1.Mesh { return n.mesh.snapshot() }

// MeshChanged returns a channel that is closed at the next change of the
// node's mesh.
func (n *Node) MeshChanged() <-chan struct{} {
	n.mesh.mu.Lock()
	defer n.mesh.mu.Unlock()
	return n.mesh.changed
}

// SetMeshEndpoint changes the node's endpoint on the mesh, as a node that
// moves does: it asks JoinMesh again on its next report.
func (n *Node) SetMeshEndpoint(endpoint string) {
	n.mesh.mu.Lock()
	defer n.mesh.mu.Unlock()
	n.mesh.endpoint = endpoint
}
