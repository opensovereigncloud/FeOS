// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package wg

import (
	"net/netip"
	"reflect"
	"testing"
	"time"
)

func TestDiffPeers(t *testing.T) {
	t.Parallel()
	peer := func(b byte, endpoint string, keepalive time.Duration, allowed ...string) Peer {
		p := Peer{PublicKey: fillKey(b), Keepalive: keepalive}
		if endpoint != "" {
			p.Endpoint = netip.MustParseAddrPort(endpoint)
		}
		for _, s := range allowed {
			p.AllowedIPs = append(p.AllowedIPs, netip.MustParsePrefix(s))
		}
		return p
	}
	state := func(p Peer) PeerState {
		return PeerState{Peer: p, LastHandshake: time.Unix(1_700_000_000, 0), RxBytes: 1, TxBytes: 2}
	}
	a := peer(1, "10.0.0.1:51820", 25*time.Second, "fd00:1::/64")
	b := peer(2, "10.0.0.2:51820", 25*time.Second, "fd00:2::/64")
	c := peer(3, "[2001:db8::3]:51820", 25*time.Second, "fd00:3::/64")
	for _, tc := range []struct {
		name       string
		have       []Peer
		want       []Peer
		wantRemove []Key
		wantSet    []Peer
	}{
		{"empty device", nil, []Peer{a, b}, nil, []Peer{a, b}},
		{"nothing wanted", []Peer{a, b}, nil, []Key{a.PublicKey, b.PublicKey}, nil},
		{"the same", []Peer{a, b}, []Peer{b, a}, nil, nil},
		{"one goes, one comes", []Peer{a, b}, []Peer{b, c}, []Key{a.PublicKey}, []Peer{c}},
		{"endpoint moved", []Peer{a}, []Peer{peer(1, "10.0.0.9:51820", 25*time.Second, "fd00:1::/64")}, nil,
			[]Peer{peer(1, "10.0.0.9:51820", 25*time.Second, "fd00:1::/64")}},
		{"port moved", []Peer{a}, []Peer{peer(1, "10.0.0.1:51821", 25*time.Second, "fd00:1::/64")}, nil,
			[]Peer{peer(1, "10.0.0.1:51821", 25*time.Second, "fd00:1::/64")}},
		{"allowed IPs changed", []Peer{a}, []Peer{peer(1, "10.0.0.1:51820", 25*time.Second, "fd00:9::/64")}, nil,
			[]Peer{peer(1, "10.0.0.1:51820", 25*time.Second, "fd00:9::/64")}},
		{"allowed IP added", []Peer{a}, []Peer{peer(1, "10.0.0.1:51820", 25*time.Second, "fd00:1::/64", "10.1.0.0/16")},
			nil, []Peer{peer(1, "10.0.0.1:51820", 25*time.Second, "fd00:1::/64", "10.1.0.0/16")}},
		{"allowed IPs reordered, unmasked and repeated",
			[]Peer{peer(1, "10.0.0.1:51820", 0, "fd00:1::/64", "10.1.0.0/16")},
			[]Peer{peer(1, "10.0.0.1:51820", 0, "10.1.2.3/16", "fd00:1::1/64", "fd00:1::/64")}, nil, nil},
		{"keepalive changed", []Peer{a}, []Peer{peer(1, "10.0.0.1:51820", 0, "fd00:1::/64")}, nil,
			[]Peer{peer(1, "10.0.0.1:51820", 0, "fd00:1::/64")}},
		{"keepalive in whole seconds", []Peer{a},
			[]Peer{peer(1, "10.0.0.1:51820", 25*time.Second+400*time.Millisecond, "fd00:1::/64")}, nil, nil},
		{"no endpoint wanted keeps the learnt one", []Peer{a}, []Peer{peer(1, "", 25*time.Second, "fd00:1::/64")},
			nil, nil},
		{"an IPv4-mapped endpoint is the IPv4 one", []Peer{a},
			[]Peer{peer(1, "[::ffff:10.0.0.1]:51820", 25*time.Second, "fd00:1::/64")}, nil, nil},
		{"a peer without endpoint gets one", []Peer{peer(1, "", 25*time.Second, "fd00:1::/64")}, []Peer{a}, nil,
			[]Peer{a}},
	} {
		var have []PeerState
		for _, p := range tc.have {
			have = append(have, state(p))
		}
		remove, set := diffPeers(have, tc.want)
		if !reflect.DeepEqual(remove, tc.wantRemove) || !reflect.DeepEqual(set, tc.wantSet) {
			t.Errorf("%s: remove %v, set %+v; want %v, %+v", tc.name, remove, set, tc.wantRemove, tc.wantSet)
		}
	}
}
