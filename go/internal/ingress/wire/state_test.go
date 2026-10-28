// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package wire

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

func env(id, hash string) Env {
	return Env{
		ID: id, Tenant: "acme", Name: id, Hosts: []string{id + ".apps.test"}, Hash: hash,
		Routes: []Route{{Name: "web", Path: "/", Service: "web", Port: "http"}},
		Services: []Service{{Name: "web", Endpoints: []Endpoint{{
			Instance: "i-" + id, Address: "fd00::1", Node: "n1", Ports: map[string]int{"http": 8080},
		}}}},
	}
}

func mustApply(t *testing.T, s *State, tb *Table) {
	t.Helper()
	got, err := s.Apply(tb)
	if err != nil {
		t.Fatalf("Apply seq %d: %v", tb.Seq, err)
	}
	if got != tb {
		t.Fatalf("Apply seq %d applied %+v", tb.Seq, got)
	}
}

func envIDs(s *State) map[string]string {
	out := map[string]string{}
	for id, e := range s.Envs {
		out[id] = e.Hash
	}
	return out
}

func TestStateWhole(t *testing.T) {
	s := NewState()
	if s.Seq != 0 || s.Envs == nil || len(s.Envs) != 0 {
		t.Fatalf("NewState %+v", s)
	}
	mustApply(t, s, &Table{Seq: 1, Whole: true, Envs: []Env{env("a", "1"), env("b", "1")},
		Mesh: &Mesh{Version: 1, Peers: []Peer{{Name: "n1"}}}, Self: &Self{Name: "ingress-0", AdmittedVersion: 2}})
	mustApply(t, s, &Table{Seq: 2, Envs: []Env{env("c", "1")}})
	// A new stream starts whole again: it replaces the environments and
	// the sequence, whatever came before.
	mustApply(t, s, &Table{Seq: 1, Whole: true, Envs: []Env{env("d", "1")},
		Mesh: &Mesh{Version: 3}, Self: &Self{Name: "ingress-0", AdmittedVersion: 4}})
	if got := envIDs(s); !reflect.DeepEqual(got, map[string]string{"d": "1"}) {
		t.Errorf("envs %v", got)
	}
	if s.Seq != 1 || s.Mesh.Version != 3 || len(s.Mesh.Peers) != 0 || s.Self.AdmittedVersion != 4 {
		t.Errorf("state %+v", s)
	}
	if !reflect.DeepEqual(s.Envs["d"], env("d", "1")) {
		t.Errorf("env d %+v", s.Envs["d"])
	}
	// A whole table without environments empties the state.
	mustApply(t, s, &Table{Seq: 1, Whole: true})
	if len(s.Envs) != 0 || s.Envs == nil {
		t.Errorf("envs %v", s.Envs)
	}
}

func TestStateDelta(t *testing.T) {
	s := NewState()
	mustApply(t, s, &Table{Seq: 1, Whole: true, Envs: []Env{env("a", "1"), env("b", "1")}})
	// Puts add and replace by ID.
	mustApply(t, s, &Table{Seq: 2, Envs: []Env{env("b", "2"), env("c", "1")}})
	if got := envIDs(s); !reflect.DeepEqual(got, map[string]string{"a": "1", "b": "2", "c": "1"}) {
		t.Errorf("after puts %v", got)
	}
	// Removed deletes; an unknown ID is no error.
	mustApply(t, s, &Table{Seq: 3, Removed: []string{"a", "zz"}})
	if got := envIDs(s); !reflect.DeepEqual(got, map[string]string{"b": "2", "c": "1"}) {
		t.Errorf("after remove %v", got)
	}
	// A heartbeat only moves the sequence on.
	before := s.Clone()
	mustApply(t, s, &Table{Seq: 4})
	before.Seq = 4
	if !reflect.DeepEqual(s, before) {
		t.Errorf("heartbeat changed the state: %+v", s)
	}
	// Puts come before removes within one message.
	mustApply(t, s, &Table{Seq: 5, Envs: []Env{env("d", "1")}, Removed: []string{"c"}})
	if got := envIDs(s); !reflect.DeepEqual(got, map[string]string{"b": "2", "d": "1"}) {
		t.Errorf("put and remove %v", got)
	}

	// A zero State takes deltas as well.
	var z State
	mustApply(t, &z, &Table{Seq: 1, Envs: []Env{env("a", "1")}})
	if len(z.Envs) != 1 {
		t.Errorf("zero state %+v", z)
	}
}

func TestStateSequence(t *testing.T) {
	s := NewState()
	mustApply(t, s, &Table{Seq: 1, Whole: true, Envs: []Env{env("a", "1")}, Self: &Self{Name: "x"}})
	mustApply(t, s, &Table{Seq: 2})
	before := s.Clone()
	for _, seq := range []uint64{4, 2, 1, 0} {
		got, err := s.Apply(&Table{Seq: seq, Envs: []Env{env("b", "1")}, Removed: []string{"a"}, Mesh: &Mesh{Version: 9}, Self: &Self{Name: "y"}})
		if !errors.Is(err, ErrSequence) || got != nil {
			t.Errorf("seq %d after 2: %v, want ErrSequence", seq, err)
		}
	}
	if !reflect.DeepEqual(s, before) {
		t.Errorf("a refused message changed the state: %+v", s)
	}
	mustApply(t, s, &Table{Seq: 3})
	if _, err := s.Apply(nil); err == nil || errors.Is(err, ErrSequence) {
		t.Errorf("nil table: %v", err)
	}
}

func TestStateMeshAndSelf(t *testing.T) {
	s := NewState()
	mesh := &Mesh{Version: 2, Peers: []Peer{{Name: "n1", PublicKey: []byte{1}}}}
	self := &Self{Name: "ingress-0", Address: "fd00::1", AppsZone: "apps.test", AdmittedVersion: 5}
	mustApply(t, s, &Table{Seq: 1, Whole: true, Mesh: mesh, Self: self})
	// Nil keeps the value, in a delta and in a whole table.
	mustApply(t, s, &Table{Seq: 2, Envs: []Env{env("a", "1")}})
	mustApply(t, s, &Table{Seq: 1, Whole: true})
	if !reflect.DeepEqual(s.Mesh, *mesh) || s.Self != *self {
		t.Errorf("nil Mesh or Self changed the state: %+v %+v", s.Mesh, s.Self)
	}
	// Non-nil replaces, also with an empty value.
	mustApply(t, s, &Table{Seq: 2, Mesh: &Mesh{Version: 3}})
	if s.Mesh.Version != 3 || s.Mesh.Peers != nil || s.Self != *self {
		t.Errorf("mesh %+v self %+v", s.Mesh, s.Self)
	}
	mustApply(t, s, &Table{Seq: 3, Self: &Self{Name: "ingress-0", AdmittedVersion: 6}})
	if s.Self.AdmittedVersion != 6 || s.Self.Address != "" || s.Mesh.Version != 3 {
		t.Errorf("mesh %+v self %+v", s.Mesh, s.Self)
	}
}

func TestStateClone(t *testing.T) {
	if (*State)(nil).Clone() != nil {
		t.Error("Clone of nil is not nil")
	}
	s := NewState()
	mustApply(t, s, &Table{Seq: 1, Whole: true, Envs: []Env{env("a", "1"), env("b", "1")},
		Mesh: &Mesh{Version: 2, Peers: []Peer{{Name: "n1", PublicKey: []byte{1, 2}}}},
		Self: &Self{Name: "ingress-0"}})
	orig := s.Clone()
	c := s.Clone()
	if !reflect.DeepEqual(c, s) {
		t.Fatalf("clone differs: %+v", c)
	}

	// Change everything the clone holds, down to the maps in endpoints.
	c.Seq = 9
	c.Envs["z"] = env("z", "1")
	delete(c.Envs, "b")
	a := c.Envs["a"]
	a.Hosts[0] = "changed"
	a.Routes[0].Path = "/changed"
	a.Services[0].Name = "changed"
	a.Services[0].Endpoints[0].Address = "changed"
	a.Services[0].Endpoints[0].Ports["http"] = 1
	a.Services[0].Endpoints[0].Ports["new"] = 2
	c.Mesh.Peers[0].PublicKey[0] = 9
	c.Mesh.Peers[0].Name = "changed"
	c.Self.Name = "changed"
	if !reflect.DeepEqual(s, orig) {
		t.Errorf("changing the clone changed the state:\n got %+v\nwant %+v", s, orig)
	}

	// And the other way round: applying to the state leaves the clone.
	c = s.Clone()
	mustApply(t, s, &Table{Seq: 2, Removed: []string{"a"}, Mesh: &Mesh{Version: 5}})
	if _, ok := c.Envs["a"]; !ok || c.Mesh.Version != 2 || c.Seq != 1 {
		t.Errorf("applying to the state changed the clone: %+v", c)
	}
}

func TestStateJSON(t *testing.T) {
	s := NewState()
	mustApply(t, s, &Table{Seq: 1, Whole: true, Envs: []Env{env("a", "1")},
		Mesh: &Mesh{Version: 2, Peers: []Peer{{Name: "n1", PublicKey: []byte{1, 2}}}}, Self: &Self{Name: "ingress-0"}})
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	var back State
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(&back, s) {
		t.Errorf("cache round trip\n got %+v\nwant %+v", back, s)
	}
}
