// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/gardenlinux/feos/go/internal/config"
	"github.com/gardenlinux/feos/go/internal/mesh"
	"github.com/gardenlinux/feos/go/internal/server"
	"github.com/gardenlinux/feos/go/internal/testnode"
)

// peersOf is a node's peers on the mesh, by name, with their endpoints and
// /96s.
func peersOf(n *node) map[string]string {
	out := map[string]string{}
	for _, p := range n.Mesh().GetPeers() {
		out[p.GetNode()] = p.GetEndpoint() + " " + p.GetPodPrefix()
	}
	return out
}

// Scenario: the pods' mesh (ADR 0017). The first instance draws the
// cluster's ULA, which every later start keeps; nodes on the mesh get
// distinct /96s of it, on whichever instance they ask, and end up with the
// others as their peers. A node that moves reaches the others with its new
// endpoint, a node that joins later is added, a removed one leaves their
// peers. An instance whose --pod-network says otherwise does not start.
func TestMesh(t *testing.T) {
	t.Parallel()
	c := newCluster(t, func(cfg *config.Config) { cfg.PodNetwork = config.PodNetworkMesh })
	c.start("a")
	c.start("b")
	ula, meshed, err := c.st.PodNetwork(bg)
	if err != nil || !meshed || ula.Bits() != mesh.NetworkBits || ula.Addr().As16()[0] != 0xfd {
		t.Fatalf("the cluster's ULA %v, meshed %v: %v", ula, meshed, err)
	}

	onMesh := func(endpoint string) func(*testnode.Config) {
		return func(cfg *testnode.Config) { cfg.Mesh, cfg.MeshEndpoint = true, endpoint }
	}
	nodes := map[string]*node{}
	endpoints := map[string]string{}
	for i, name := range []string{"n1", "n2", "n3"} {
		endpoints[name] = fmt.Sprintf("127.0.0.%d:51820", i+1)
		nodes[name] = c.join(name, onMesh(endpoints[name]))
	}
	prefixes := map[string]string{}
	// want waits until every node in names has the others of names as its
	// peers, with their endpoints and /64s.
	want := func(what string, names ...string) {
		t.Helper()
		eventually(t, 60*time.Second, what, func() bool {
			for _, name := range names {
				m := nodes[name].Mesh()
				if m.GetPodNetwork() != ula.String() || m.GetPodPrefix() == "" {
					return false
				}
				prefixes[name] = m.GetPodPrefix()
			}
			for _, name := range names {
				got := peersOf(nodes[name])
				if len(got) != len(names)-1 {
					return false
				}
				for _, other := range names {
					if other != name && got[other] != endpoints[other]+" "+prefixes[other] {
						return false
					}
				}
			}
			return true
		})
	}
	want("three nodes with each other as peers", "n1", "n2", "n3")
	seen := map[string]bool{}
	for name, p := range prefixes {
		prefix, err := netip.ParsePrefix(p)
		if err != nil || prefix.Bits() != mesh.PrefixBits || !ula.Contains(prefix.Addr()) || seen[p] {
			t.Fatalf("%s got %s of %s: %v (seen %v)", name, p, ula, err, seen)
		}
		seen[p] = true
	}

	// n3 moves.
	endpoints["n3"] = "127.0.0.33:51821"
	nodes["n3"].SetMeshEndpoint(endpoints["n3"])
	want("n3's new endpoint", "n1", "n2", "n3")

	// n4 joins later.
	endpoints["n4"] = "[::1]:51820"
	nodes["n4"] = c.join("n4", onMesh(endpoints["n4"]))
	want("n4 as a peer", "n1", "n2", "n3", "n4")

	// n2 is removed.
	if r := c.call(nil, "DELETE", "/api/v1/admin/nodes/n2", adminAuth, ""); r.code != 200 {
		t.Fatalf("remove n2: %d %s", r.code, r.body)
	}
	if err := nodes["n2"].ended(t, 60*time.Second); !errors.Is(err, testnode.ErrRefused) {
		t.Fatalf("the removed node: %v", err)
	}
	want("n2 gone from the peers", "n1", "n3", "n4")

	// A node off the mesh gets no peers, and its feosd's FAILED_PRECONDITION
	// does not hold the others up.
	off := c.join("off")
	if m := off.Mesh(); m.GetPodPrefix() != "" || len(m.GetPeers()) != 0 {
		t.Fatalf("a node off the mesh: %v", m)
	}

	// The cluster stays on its ULA: another one, or none, refuses to start.
	for _, other := range []string{config.PodNetworkRouted, "fd00:1:2::/64"} {
		cfg := c.cfg
		cfg.InstanceID, cfg.PodNetwork = "e2e-other", other
		cfg.NodeListen = "127.0.0.1:0"
		if in, err := server.New(bg, cfg, c.log); err == nil || !strings.Contains(err.Error(), "meshed on "+ula.String()) {
			if in != nil {
				in.Close()
			}
			t.Fatalf("--pod-network %q on a cluster meshed on %s: %v", other, ula, err)
		}
	}
	cfg := c.cfg
	cfg.InstanceID, cfg.PodNetwork, cfg.NodeListen = "e2e-same", ula.String(), "127.0.0.1:0"
	in, err := server.New(bg, cfg, c.log)
	if err != nil {
		t.Fatalf("--pod-network %s on a cluster meshed on it: %v", ula, err)
	}
	in.Close()
}
