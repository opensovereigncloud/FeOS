// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package fleet

import (
	"bytes"
	"context"
	"fmt"
	"net/netip"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	fleetv1 "github.com/gardenlinux/feos/go/gen/feos/fleet/v1"
	networkv1 "github.com/gardenlinux/feos/go/gen/feos/network/v1"
	"github.com/gardenlinux/feos/go/internal/store"
)

var testULA = netip.MustParsePrefix("fd5e:1b2c:93a0::/48")

// meshed makes the harness's cluster meshed on testULA.
func (h *harness) meshed() {
	h.t.Helper()
	if err := h.st.InsertPodNetwork(bg, testULA); err != nil {
		h.t.Fatal(err)
	}
	h.s.cfg.PodNetwork = testULA
}

func meshKey(b byte) []byte { return bytes.Repeat([]byte{b}, 32) }

func (h *harness) joinMesh(node, serial string, key []byte, endpoint string) (*fleetv1.JoinMeshResponse, error) {
	return h.s.UplinkServer().JoinMesh(uplinkCtx(node, serial), &fleetv1.JoinMeshRequest{PublicKey: key, Endpoint: endpoint})
}

func TestJoinMeshRouted(t *testing.T) {
	h := newHarness(t)
	serial := h.joined("n1")
	resp, err := h.joinMesh("n1", serial, meshKey(1), "10.0.0.1:51820")
	if err != nil || resp.GetPodNetwork() != "" || resp.GetPodPrefix() != "" {
		t.Fatalf("a routed cluster answered %v, %v", resp, err)
	}
	if n := h.node("n1"); n.MeshSubnet != 0 || n.MeshPeer() {
		t.Fatalf("a routed cluster put the node on a mesh: %+v", n)
	}
	// A routed cluster still checks the node.
	h.exec("UPDATE nodes SET status = 'removed', removed_at = now() WHERE name = 'n1'")
	_, err = h.joinMesh("n1", serial, nil, "")
	wantCode(t, err, codes.PermissionDenied, "")
}

func TestJoinMesh(t *testing.T) {
	h := newHarness(t)
	h.meshed()
	s1, s2 := h.joined("n1"), h.joined("n2")

	// A request without a key asks only whether the cluster is meshed.
	resp, err := h.joinMesh("n1", s1, nil, "")
	if err != nil || resp.GetPodNetwork() != testULA.String() || resp.GetPodPrefix() != "" {
		t.Fatalf("probe: %v, %v", resp, err)
	}
	if n := h.node("n1"); n.MeshSubnet != 0 {
		t.Fatalf("a probe got a subnet: %+v", n)
	}

	for _, bad := range []struct {
		key      []byte
		endpoint string
	}{
		{meshKey(1)[:31], "10.0.0.1:51820"},
		{append(meshKey(1), 0), "10.0.0.1:51820"},
		{meshKey(1), ""},
		{meshKey(1), "node1:51820"},
		{meshKey(1), "10.0.0.1"},
		{meshKey(1), "10.0.0.1:0"},
		{meshKey(1), "[fe80::1%eth0]:51820"},
	} {
		_, err := h.joinMesh("n1", s1, bad.key, bad.endpoint)
		wantCode(t, err, codes.InvalidArgument, "")
	}

	r1, err := h.joinMesh("n1", s1, meshKey(1), "10.0.0.1:51820")
	if err != nil || r1.GetPodNetwork() != "fd5e:1b2c:93a0::/48" || r1.GetPodPrefix() != "fd5e:1b2c:93a0:1::/64" {
		t.Fatalf("n1: %v, %v", r1, err)
	}
	r2, err := h.joinMesh("n2", s2, meshKey(2), "[2001:db8::2]:51820")
	if err != nil || r2.GetPodPrefix() != "fd5e:1b2c:93a0:2::/64" {
		t.Fatalf("n2: %v, %v", r2, err)
	}
	again, err := h.joinMesh("n1", s1, meshKey(3), "10.0.0.1:51820")
	if err != nil || again.GetPodPrefix() != r1.GetPodPrefix() {
		t.Fatalf("n1 again: %v, %v", again, err)
	}
	if n := h.node("n1"); !bytes.Equal(n.MeshPublicKey, meshKey(3)) || n.MeshEndpoint != "10.0.0.1:51820" || n.MeshSubnet != 1 {
		t.Fatalf("n1 %+v", n)
	}

	_, err = h.joinMesh("ghost", s1, meshKey(4), "10.0.0.4:51820")
	wantCode(t, err, codes.PermissionDenied, "")
	h.exec("UPDATE nodes SET status = 'removed', removed_at = now() WHERE name = 'n2'")
	_, err = h.joinMesh("n2", s2, meshKey(2), "[2001:db8::2]:51820")
	wantCode(t, err, codes.PermissionDenied, "")
}

func TestJoinMeshExhausted(t *testing.T) {
	h := newHarness(t)
	h.meshed()
	serial := h.joined("n1")
	h.exec("SELECT setval('mesh_subnets', 65535)")
	_, err := h.joinMesh("n1", serial, meshKey(1), "10.0.0.1:51820")
	wantCode(t, err, codes.ResourceExhausted, "")
}

// fakeMeshNode is a node's MeshService on a fake connection, as feosd
// answers it.
type fakeMeshNode struct {
	mu      sync.Mutex
	version uint64
	peers   map[string]*networkv1.MeshPeer
	calls   []string // "put 3" or "update 2-3"
	// refuseDelta answers every UpdateMeshPeers with FAILED_PRECONDITION;
	// older answers everything with UNIMPLEMENTED.
	refuseDelta, older bool
}

func (f *fakeMeshNode) Invoke(_ context.Context, method string, args, _ any, _ ...grpc.CallOption) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.older {
		f.calls = append(f.calls, "unimplemented")
		return status.Error(codes.Unimplemented, "unknown service feos.network.v1.MeshService")
	}
	switch req := args.(type) {
	case *networkv1.PutMeshPeersRequest:
		f.calls = append(f.calls, "put "+strconv.FormatUint(req.GetVersion(), 10))
		f.version, f.peers = req.GetVersion(), map[string]*networkv1.MeshPeer{}
		for _, p := range req.GetPeers() {
			f.peers[p.GetNode()] = p
		}
	case *networkv1.UpdateMeshPeersRequest:
		f.calls = append(f.calls, "update "+strconv.FormatUint(req.GetFromVersion(), 10)+"-"+strconv.FormatUint(req.GetVersion(), 10))
		if f.refuseDelta || req.GetFromVersion() != f.version {
			return status.Error(codes.FailedPrecondition, "not at from_version")
		}
		for _, p := range req.GetPutPeers() {
			f.peers[p.GetNode()] = p
		}
		for _, n := range req.GetRemovePeers() {
			delete(f.peers, n)
		}
		f.version = req.GetVersion()
	default:
		return status.Errorf(codes.Unimplemented, "%s", method)
	}
	return nil
}

func (f *fakeMeshNode) NewStream(context.Context, *grpc.StreamDesc, string, ...grpc.CallOption) (grpc.ClientStream, error) {
	return nil, status.Error(codes.Unimplemented, "no streams")
}

// state returns the node's version, its peers' names and endpoints, and
// its calls since the last state.
func (f *fakeMeshNode) state() (uint64, map[string]string, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	peers := map[string]string{}
	for n, p := range f.peers {
		peers[n] = p.GetEndpoint()
	}
	calls := f.calls
	f.calls = nil
	return f.version, peers, calls
}

func wantMeshNode(t *testing.T, name string, f *fakeMeshNode, version uint64, peers map[string]string, calls ...string) {
	t.Helper()
	v, got, made := f.state()
	if v != version || len(got) != len(peers) || !slices.Equal(made, calls) {
		t.Fatalf("%s: version %d, peers %v, calls %v; want %d, %v, %v", name, v, got, made, version, peers, calls)
	}
	for n, ep := range peers {
		if got[n] != ep {
			t.Fatalf("%s: peers %v, want %v", name, got, peers)
		}
	}
}

// The leader sends every connected node on the mesh the others: whole
// first, then deltas from the version it took, whole again when a node
// refuses a delta or its connection was lost; a node that does not serve
// MeshService is not asked again on its connection; a removed node leaves
// the others' peers.
func TestMeshPeersSent(t *testing.T) {
	h := newHarness(t)
	h.meshed()
	serials := map[string]string{}
	for i, n := range []string{"n1", "n2", "n3", "n4"} {
		serials[n] = h.joined(n)
		if _, err := h.joinMesh(n, serials[n], meshKey(byte(i+1)), fmt.Sprintf("10.0.0.%d:51820", i+1)); err != nil {
			t.Fatal(err)
		}
	}
	h.joined("off") // connected, but not on the mesh
	n1, n2, n4 := &fakeMeshNode{}, &fakeMeshNode{}, &fakeMeshNode{older: true}
	off := &fakeMeshNode{}
	h.links.setConn("n1", n1)
	h.links.setConn("n2", n2)
	h.links.setConn("n4", n4)
	h.links.setConn("off", off)
	run := func() {
		t.Helper()
		if err := h.s.sendMeshPeers(bg); err != nil {
			t.Fatal(err)
		}
	}

	run()
	all := map[string]string{"n1": "10.0.0.1:51820", "n2": "10.0.0.2:51820", "n3": "10.0.0.3:51820",
		"n4": "10.0.0.4:51820"}
	others := func(node string, m map[string]string) map[string]string {
		out := map[string]string{}
		for n, ep := range m {
			if n != node {
				out[n] = ep
			}
		}
		return out
	}
	wantMeshNode(t, "n1", n1, 4, others("n1", all), "put 4")
	wantMeshNode(t, "n2", n2, 4, others("n2", all), "put 4")
	wantMeshNode(t, "n4", n4, 0, nil, "unimplemented")
	wantMeshNode(t, "off", off, 0, nil)

	// Nothing changed: nothing is sent, and the older node is not asked
	// again.
	run()
	wantMeshNode(t, "n1", n1, 4, others("n1", all))
	wantMeshNode(t, "n4", n4, 0, nil)

	// n3 moves: a delta.
	if _, err := h.joinMesh("n3", serials["n3"], meshKey(3), "10.0.0.33:51820"); err != nil {
		t.Fatal(err)
	}
	all["n3"] = "10.0.0.33:51820"
	run()
	wantMeshNode(t, "n1", n1, 5, others("n1", all), "update 4-5")
	wantMeshNode(t, "n2", n2, 5, others("n2", all), "update 4-5")

	// A node that refuses the delta gets the peers whole.
	n2.mu.Lock()
	n2.refuseDelta = true
	n2.mu.Unlock()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(h.st.InTx(bg, func(tx *store.Store) error {
		if _, err := tx.LockNode(bg, "n4", store.ForUpdate); err != nil {
			return err
		}
		_, err := tx.RemoveNode(bg, "n4")
		return err
	}))
	delete(all, "n4")
	run()
	wantMeshNode(t, "n1", n1, 6, others("n1", all), "update 5-6")
	wantMeshNode(t, "n2", n2, 6, others("n2", all), "update 5-6", "put 6")

	// A lost connection: the node's next connection gets the peers whole,
	// and the older node is asked again.
	h.s.mesh.forget("n1")
	h.s.mesh.forget("n4")
	run()
	wantMeshNode(t, "n1", n1, 6, others("n1", all), "put 6")
	wantMeshNode(t, "n4", n4, 0, nil)

	// A new term begins with every node's peers whole.
	h.s.leadingSince = func() time.Time { return time.Now() }
	run()
	wantMeshNode(t, "n1", n1, 6, others("n1", all), "put 6")
	wantMeshNode(t, "n2", n2, 6, others("n2", all), "put 6")
}
