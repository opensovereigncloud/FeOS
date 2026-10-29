// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package fleet

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	fleetv1 "github.com/gardenlinux/feos/go/gen/feos/fleet/v1"
	networkv1 "github.com/gardenlinux/feos/go/gen/feos/network/v1"
	"github.com/gardenlinux/feos/go/internal/activity"
	"github.com/gardenlinux/feos/go/internal/db"
	"github.com/gardenlinux/feos/go/internal/mesh"
	"github.com/gardenlinux/feos/go/internal/store"
)

// LoopMeshPeers is the leader's loop that sends every node its peers on
// the pods' mesh; store.HintMeshPeers wakes it.
const LoopMeshPeers = store.HintMeshPeers

// The timings of the peers' distribution.
const (
	// meshPeersInterval is the loop's backstop: a change wakes it at once.
	meshPeersInterval = 15 * time.Second
	// meshCallTimeout bounds one PutMeshPeers or UpdateMeshPeers.
	meshCallTimeout = 10 * time.Second
	// meshParallel is how many nodes the loop sends to at once.
	meshParallel = 16
)

// joinMesh is JoinMesh (ADR 0017): the node, named by its certificate,
// which must pass CheckNode's conditions, gets its /96 of the cluster's
// ULA, the next subnet ID at its first call and the same one after it, and
// its key and endpoint make it a peer of the others. A key of small order
// is INVALID_ARGUMENT, a new key that another node or an ingress holds
// FAILED_PRECONDITION (store.ErrMeshKeyTaken). In a routed cluster
// the answer is empty; a request without a key, from a node with a prefix
// of its network's, gets the ULA alone.
func (s *Service) joinMesh(ctx context.Context, req *fleetv1.JoinMeshRequest) (*fleetv1.JoinMeshResponse, error) {
	p, err := nodePeer(ctx)
	if err != nil {
		return nil, err
	}
	key := req.GetPublicKey()
	var endpoint string
	if len(key) > 0 {
		if len(key) != mesh.KeyLen {
			return nil, status.Errorf(codes.InvalidArgument, "public_key has %d bytes, not %d", len(key), mesh.KeyLen)
		}
		if err := mesh.CheckKey(key); err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "public_key: %v", err)
		}
		if endpoint, err = mesh.CheckEndpoint(req.GetEndpoint()); err != nil {
			return nil, status.Error(codes.InvalidArgument, activity.Clean(err.Error()))
		}
	}
	ula := s.cfg.PodNetwork
	joining := ula.IsValid() && len(key) > 0
	lock := store.NoLock
	if joining {
		lock = store.ForUpdate
	}
	ctx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()
	var j store.MeshJoin
	err = s.st.InTx(ctx, func(tx *store.Store) error {
		j = store.MeshJoin{}
		n, err := tx.LockNode(ctx, p.Node, lock)
		if errors.Is(err, store.ErrNoRow) {
			return status.Errorf(codes.PermissionDenied, "node %s is unknown; join it again", p.Node)
		}
		if err != nil {
			return err
		}
		if err := checkInTx(ctx, tx, p.Node, p.Serial); err != nil {
			return err
		}
		if !joining {
			return nil
		}
		j, err = tx.JoinMesh(ctx, n, ula, key, endpoint)
		return err
	})
	if errors.Is(err, store.ErrMeshExhausted) {
		s.log.Error("a node cannot join the pods' mesh: every /96 of it has been handed out", "node", p.Node,
			"pod_network", ula.String())
		return nil, status.Errorf(codes.ResourceExhausted, "every /96 of the pod network %s has been handed out", ula)
	}
	if errors.Is(err, store.ErrMeshKeyTaken) {
		s.rl.warn("mesh-key", "JoinMesh refused: the node's key is another peer's", "node", p.Node)
		return nil, status.Errorf(codes.FailedPrecondition, "the mesh key of node %s is another peer's; make a new one", p.Node)
	}
	if err != nil {
		if st, ok := status.FromError(err); ok && st.Code() != codes.Unavailable {
			s.rl.warn("mesh", "JoinMesh refused", "node", p.Node, "code", st.Code().String(), "err", st.Message())
			return nil, st.Err()
		}
		return nil, rpcError(err, "join the mesh")
	}
	if j.Changed {
		s.log.Info("node is on the pods' mesh", "node", p.Node, "pod_prefix", j.Prefix, "endpoint", endpoint)
	}
	if !ula.IsValid() {
		return &fleetv1.JoinMeshResponse{}, nil
	}
	return &fleetv1.JoinMeshResponse{PodNetwork: ula.String(), PodPrefix: j.Prefix}, nil
}

// meshSender is the leader's memory of the peers it sent: per node, the
// version that node took in this term since its connection came up; the
// peer sets of those versions, for the deltas; and the nodes whose feosd
// answered UNIMPLEMENTED on this connection. A node's lost connection
// forgets it, so that its next connection gets its peers whole.
type meshSender struct {
	mu    sync.Mutex
	term  time.Time
	sent  map[string]uint64
	older map[string]bool
	gen   map[string]uint64 // per node, counts its forgets: a send begun before one does not count
	sets  map[uint64]mesh.Peers
}

// meshJob is a send to one node: from the version it has, whole when nil.
type meshJob struct {
	node string
	from *mesh.Peers
	gen  uint64
}

// plan begins a run with the peers at their version, in the leader's term:
// it returns the sends to the nodes, of those connected, that are on the
// mesh and have not taken this version.
func (m *meshSender) plan(term time.Time, peers mesh.Peers, connected []string) []meshJob {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sent == nil || !m.term.Equal(term) {
		m.term = term
		m.sent, m.older, m.sets = map[string]uint64{}, map[string]bool{}, map[uint64]mesh.Peers{}
		if m.gen == nil {
			m.gen = map[string]uint64{}
		}
	}
	m.sets[peers.Version] = peers
	var jobs []meshJob
	for _, node := range connected {
		if _, ok := peers.Nodes[node]; !ok || m.older[node] {
			continue
		}
		v, ok := m.sent[node]
		if ok && v == peers.Version {
			continue
		}
		job := meshJob{node: node, gen: m.gen[node]}
		if from, have := m.sets[v]; ok && have {
			job.from = &from
		}
		jobs = append(jobs, job)
	}
	return jobs
}

// took records that the node took version, unless it was forgotten since
// the send began.
func (m *meshSender) took(job meshJob, version uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sent != nil && m.gen[job.node] == job.gen {
		m.sent[job.node] = version
	}
}

// isOlder records that the node's feosd does not serve MeshService; it
// reports whether this is news.
func (m *meshSender) isOlder(job meshJob) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.older == nil || m.gen[job.node] != job.gen || m.older[job.node] {
		return false
	}
	m.older[job.node] = true
	return true
}

// forget drops what the leader knows of the node's peers: its connection
// is gone.
func (m *meshSender) forget(node string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.gen == nil {
		m.gen = map[string]uint64{}
	}
	m.gen[node]++
	delete(m.sent, node)
	delete(m.older, node)
}

// prune drops the peer sets that no node has taken, but current.
func (m *meshSender) prune(current uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	used := map[uint64]bool{current: true}
	for _, v := range m.sent {
		used[v] = true
	}
	for v := range m.sets {
		if !used[v] {
			delete(m.sets, v)
		}
	}
}

// sendMeshPeers is LoopMeshPeers: every node on the mesh that the leader
// is connected to and that has not taken the peers' current version gets
// them, as a delta from the version it took in this term, or whole: after
// its connection came up, in a new term, and when it refuses the delta.
// The calls run outside any transaction, at most meshParallel at once.
func (s *Service) sendMeshPeers(ctx context.Context) error {
	if !s.cfg.PodNetwork.IsValid() || s.links == nil {
		return nil
	}
	peers, err := s.st.MeshPeers(ctx)
	if err != nil {
		return fmt.Errorf("read the mesh's peers: %w", err)
	}
	jobs := s.mesh.plan(s.leadingSince(), peers, s.links.Nodes())
	sem := make(chan struct{}, meshParallel)
	var wg sync.WaitGroup
	for _, job := range jobs {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			wg.Wait()
			return nil
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			s.sendPeers(ctx, job, peers)
		}()
	}
	wg.Wait()
	s.mesh.prune(peers.Version)
	return nil
}

// sendPeers sends one node its peers of to.
func (s *Service) sendPeers(ctx context.Context, job meshJob, to mesh.Peers) {
	ctx, cancel := context.WithTimeout(ctx, meshCallTimeout)
	defer cancel()
	conn, err := s.links.Conn(ctx, job.node)
	if err != nil {
		s.rl.warn("mesh-peers", "cannot send a node its peers", "node", job.node, "err", err)
		return
	}
	client := networkv1.NewMeshServiceClient(conn)
	if job.from != nil {
		put, remove := mesh.Delta(*job.from, to, job.node)
		_, err = client.UpdateMeshPeers(ctx, &networkv1.UpdateMeshPeersRequest{
			FromVersion: job.from.Version, Version: to.Version, PutPeers: wirePeers(put), RemovePeers: remove,
		})
		if status.Code(err) != codes.FailedPrecondition {
			s.sentPeers(job, to.Version, err)
			return
		}
	}
	_, err = client.PutMeshPeers(ctx, &networkv1.PutMeshPeersRequest{Version: to.Version, Peers: wirePeers(to.For(job.node))})
	s.sentPeers(job, to.Version, err)
}

// sentPeers records how a send to a node ended.
func (s *Service) sentPeers(job meshJob, version uint64, err error) {
	switch {
	case err == nil:
		s.mesh.took(job, version)
	case status.Code(err) == codes.Unimplemented:
		if s.mesh.isOlder(job) {
			s.log.Info("node's feosd does not serve the mesh's peers; not sent again on this connection", "node", job.node)
		}
	default:
		s.rl.warn("mesh-peers", "a node did not take its peers", "node", job.node, "version", version, "err", err)
	}
}

func wirePeers(peers []mesh.Peer) []*networkv1.MeshPeer {
	out := make([]*networkv1.MeshPeer, len(peers))
	for i, p := range peers {
		out[i] = &networkv1.MeshPeer{Node: p.Node, PublicKey: p.PublicKey, Endpoint: p.Endpoint, PodPrefix: p.PodPrefix}
	}
	return out
}

// wakeMeshPeers wakes the leader's LoopMeshPeers, when this instance leads
// a meshed cluster: a node got its first connection here.
func (s *Service) wakeMeshPeers() {
	if !s.cfg.PodNetwork.IsValid() || s.el == nil || !s.el.IsLeader() {
		return
	}
	s.goBackground(func(ctx context.Context) {
		ctx, cancel := context.WithTimeout(ctx, rpcTimeout)
		defer cancel()
		if err := s.st.Notify(ctx, db.ChanLeader, store.HintMeshPeers); err != nil {
			s.rl.warn("mesh-peers", "cannot wake the mesh's peers loop", "err", err)
		}
	})
}
