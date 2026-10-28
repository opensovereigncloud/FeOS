// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/gardenlinux/feos/go/internal/ingress/wg"
	"github.com/gardenlinux/feos/go/internal/ingress/wire"
)

// MeshDevice is the instance's WireGuard device on the pods' mesh. The
// agent serializes Configure and SetPeers; Status may run beside them.
type MeshDevice interface {
	// Configure creates the device if it is missing and sets it up for
	// c: its MTU, up, the address c.Address and no other, the route of
	// c.PodNetwork through it with that address as the source, its key
	// and its port. The peers the device has stay.
	Configure(MeshConfig) error
	// SetPeers makes peers the device's peers; a peer that stays as it
	// is keeps its session.
	SetPeers([]wg.Peer) error
	// Status returns the device and its peers as the kernel has them.
	Status() (wg.Device, error)
}

// MeshConfig is the configuration of the mesh device.
type MeshConfig struct {
	Device     string
	PrivateKey wg.Key
	ListenPort int
	// MTU is the device's MTU; 0 is ComputeMTU of the interface that
	// holds Endpoint's address.
	MTU int
	// Endpoint is where the nodes reach the device.
	Endpoint netip.AddrPort
	// Address is the instance's address on the mesh, a /128.
	Address netip.Prefix
	// PodNetwork is the cluster's ULA, routed through the device.
	PodNetwork netip.Prefix
}

// The device's MTU (D5) and the peers' keepalive.
const (
	minMTU            = 1280 // IPv6's minimum
	maxMTU            = 1420 // WireGuard's default
	wireGuardOverhead = 80   // WireGuard 32, UDP 8, outer IPv6 40
	peerKeepalive     = 25 * time.Second
)

// ComputeMTU is the mesh device's MTU over an interface with MTU ifMTU:
// the interface's MTU less WireGuard's overhead over IPv6, at most 1420
// and at least 1280. The smaller MTU also clamps the MSS of the
// connections the ingress opens through the device.
func ComputeMTU(ifMTU int) int {
	return min(maxMTU, max(minMTU, ifMTU-wireGuardOverhead))
}

// wantMesh makes c the device's configuration and configures it unless it
// has it already.
func (a *Agent) wantMesh(c MeshConfig) {
	a.mu.Lock()
	a.meshWant = c
	a.mu.Unlock()
	a.ensureMesh()
}

// ensureMesh configures the device as wanted unless it is so already, and
// then sets its peers again. A failure is logged; the mesh loop calls it
// again.
func (a *Agent) ensureMesh() {
	a.meshMu.Lock()
	a.mu.Lock()
	want := a.meshWant
	done := want.Device == "" || (a.meshOK && a.meshCfg == want)
	a.mu.Unlock()
	if done {
		a.meshMu.Unlock()
		return
	}
	err := a.deps.Mesh.Configure(want)
	a.mu.Lock()
	if err != nil {
		a.meshOK = false
	} else {
		a.meshCfg, a.meshOK, a.peersSet = want, true, false
	}
	a.mu.Unlock()
	a.meshMu.Unlock()
	if err != nil {
		a.rl.error("mesh", "ingress agent: configure the mesh device; retrying", "err", err, "device", want.Device, "address", want.Address)
		return
	}
	a.log.Info("ingress agent: configured the mesh device", "device", want.Device, "address", want.Address, "podNetwork", want.PodNetwork)
	a.syncPeers()
}

// syncPeers sets the device's peers from the table's mesh, unless the
// device already has that mesh version. A failure is retried by the mesh
// loop.
func (a *Agent) syncPeers() {
	a.meshMu.Lock()
	defer a.meshMu.Unlock()
	a.mu.Lock()
	if !a.meshOK || a.state == nil || (a.peersSet && a.peersVer == a.state.Mesh.Version) {
		a.mu.Unlock()
		return
	}
	m := a.state.Mesh
	a.mu.Unlock()
	peers, problems := wgPeers(m)
	if len(problems) > 0 {
		a.rl.warn("peers-skipped", "ingress agent: skipping mesh peers", "problems", problems, "meshVersion", m.Version)
	}
	err := a.deps.Mesh.SetPeers(peers)
	a.mu.Lock()
	a.peersSet = err == nil
	if err == nil {
		a.peersVer = m.Version
	}
	a.mu.Unlock()
	if err != nil {
		a.rl.error("peers", "ingress agent: set the mesh device's peers; retrying", "err", err, "meshVersion", m.Version)
		return
	}
	a.log.Info("ingress agent: set the mesh device's peers", "peers", len(peers), "meshVersion", m.Version)
	// Report the new peers' handshakes without waiting for the next poll.
	select {
	case a.meshKick <- struct{}{}:
	default:
	}
}

// wgPeers are the device's peers for a mesh: each node with its /64 as
// its only allowed prefix, its endpoint and keepalive 25 s. Nodes that
// cannot be peers are left out and described in problems.
func wgPeers(m wire.Mesh) (peers []wg.Peer, problems []string) {
	seen := map[wg.Key]bool{}
	for _, p := range m.Peers {
		if len(p.PublicKey) != wg.KeyLen {
			problems = append(problems, fmt.Sprintf("node %s: a public key of %d bytes", p.Name, len(p.PublicKey)))
			continue
		}
		k := wg.Key(p.PublicKey)
		if seen[k] {
			problems = append(problems, fmt.Sprintf("node %s: public key %s listed twice", p.Name, k))
			continue
		}
		var ep netip.AddrPort
		if p.Endpoint != "" {
			var err error
			if ep, err = netip.ParseAddrPort(p.Endpoint); err != nil {
				problems = append(problems, fmt.Sprintf("node %s: endpoint %q is not address:port", p.Name, p.Endpoint))
				continue
			}
		}
		pfx, err := netip.ParsePrefix(p.Prefix)
		if err != nil {
			problems = append(problems, fmt.Sprintf("node %s: prefix %q", p.Name, p.Prefix))
			continue
		}
		seen[k] = true
		peers = append(peers, wg.Peer{
			PublicKey:  k,
			Endpoint:   ep,
			AllowedIPs: []netip.Prefix{pfx.Masked()},
			Keepalive:  peerKeepalive,
		})
	}
	return peers, problems
}

// meshLoop reads the device's peers every meshPoll, and after the peers
// were set, and reports each node's last handshake to OnMesh; it also
// retries setting the peers, warns of an old table and forgets old Wake
// calls.
func (a *Agent) meshLoop(ctx context.Context) {
	t := time.NewTicker(a.tune.meshPoll)
	defer t.Stop()
	for {
		now := time.Now()
		a.ensureMesh()
		a.syncPeers()
		a.pollMesh()
		a.checkTableAge(now)
		a.sweepWakes(now)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-a.meshKick:
		}
	}
}

// pollMesh reads the device's peers, maps their keys to node names
// through the table's mesh, and reports their handshakes. Without a
// configured device, or when it cannot be read, no node has one; a device
// that cannot be read is configured again.
func (a *Agent) pollMesh() {
	a.mu.Lock()
	ok := a.meshOK
	names := map[wg.Key]string{}
	if a.state != nil {
		for _, p := range a.state.Mesh.Peers {
			if len(p.PublicKey) == wg.KeyLen {
				names[wg.Key(p.PublicKey)] = p.Name
			}
		}
	}
	a.mu.Unlock()
	handshakes := map[string]time.Time{}
	var peers []MeshPeer
	if ok {
		dev, err := a.deps.Mesh.Status()
		if err != nil {
			a.rl.warn("mesh-status", "ingress agent: read the mesh device; configuring it again", "err", err)
			a.mu.Lock()
			a.meshOK = false
			a.mu.Unlock()
			a.ensureMesh()
		} else {
			for _, p := range dev.Peers {
				name, known := names[p.PublicKey]
				if !known {
					continue
				}
				handshakes[name] = p.LastHandshake
				mp := MeshPeer{Node: name, PublicKey: p.PublicKey.String(), LastHandshake: p.LastHandshake}
				if p.Endpoint.IsValid() {
					mp.Endpoint = p.Endpoint.String()
				}
				peers = append(peers, mp)
			}
			slices.SortFunc(peers, func(x, y MeshPeer) int { return strings.Compare(x.Node, y.Node) })
		}
	}
	a.mu.Lock()
	a.meshPeers = peers
	a.mu.Unlock()
	if a.deps.OnMesh != nil {
		a.deps.OnMesh(handshakes)
	}
}
