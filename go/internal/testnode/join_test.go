// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package testnode

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestJoinConnectsAndReports(t *testing.T) {
	e := newEnv(t)
	in := e.instance("i1", caIss)
	n := e.start("n1", in)
	if n.Name() != "n1" || n.Serial() == "" {
		t.Fatalf("name %q, serial %q", n.Name(), n.Serial())
	}
	if e.f.joinCount() != 1 {
		t.Fatalf("%d joins", e.f.joinCount())
	}
	eventually(t, 5*time.Second, "a report", func() bool { return e.f.reportsFrom(n.Serial()) > 0 })
	r, _ := e.f.lastReport()
	if r.node != "n1" || r.t.StartId != n.StartID() || r.t.Hostname != "n1" || r.t.CpuCores != defaultCPUCores ||
		r.t.InventoryHash != InventoryHash(nil) || r.t.ObjectStore || r.t.PodPrefix == "" {
		t.Fatalf("report %+v", r.t)
	}
	if got := n.Connections(); len(got) != 1 || got[0] != in.addr() {
		t.Fatalf("connections %v", got)
	}
	if n.UplinkAddr() != in.addr() {
		t.Fatalf("uplink %q", n.UplinkAddr())
	}
}

// A server whose chain does not hold the root the token pins is refused
// before the token is sent.
func TestJoinRefusesAnotherCA(t *testing.T) {
	e := newEnv(t)
	in := e.instance("i1", otherIss)
	n := e.newNode(e.config("n1", in))
	err := e.run(n).wait(t, 10*time.Second)
	if !errors.Is(err, ErrUntrusted) {
		t.Fatalf("Run: %v", err)
	}
	if e.f.joinCount() != 0 {
		t.Fatalf("the token reached the other CA's server: %d joins", e.f.joinCount())
	}
}

// A JoinResponse whose trust bundle lacks the pinned root is refused, and
// nothing of it is kept.
func TestJoinRefusesBundleWithoutPin(t *testing.T) {
	e := newEnv(t)
	e.f.joinBundle = otherIss.BundleDER()
	in := e.instance("i1", caIss)
	cfg := e.config("n1", in)
	cfg.StateDir = t.TempDir()
	err := e.run(e.newNode(cfg)).wait(t, 10*time.Second)
	if !errors.Is(err, ErrUntrusted) {
		t.Fatalf("Run: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cfg.StateDir, identityFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("identity kept: %v", err)
	}
}

func TestJoinRefused(t *testing.T) {
	e := newEnv(t)
	e.f.joinErrs = []error{status.Error(codes.PermissionDenied, "join token already used")}
	in := e.instance("i1", caIss)
	err := e.run(e.newNode(e.config("n1", in))).wait(t, 10*time.Second)
	if !errors.Is(err, ErrJoinRefused) || err.Error() != ErrJoinRefused.Error()+": join token already used" {
		t.Fatalf("Run: %v", err)
	}
}

// A Join that fails on the way is repeated with the same key.
func TestJoinRetriesWithTheSameKey(t *testing.T) {
	e := newEnv(t)
	e.f.joinErrs = []error{status.Error(codes.Unavailable, "database unavailable"), status.Error(codes.ResourceExhausted, "too many joins")}
	in := e.instance("i1", caIss)
	e.start("n1", in)
	if e.f.joinCount() != 3 {
		t.Fatalf("%d joins", e.f.joinCount())
	}
	e.f.mu.Lock()
	defer e.f.mu.Unlock()
	for _, j := range e.f.joins[1:] {
		if string(j.Csr) != string(e.f.joins[0].Csr) || j.Token != e.f.joins[0].Token {
			t.Fatal("a repeated Join changed its key or token")
		}
	}
}

// A node keeps its identity in its state directory and takes it at its
// next start; another token makes it join again.
func TestIdentityKeptAndReused(t *testing.T) {
	e := newEnv(t)
	in := e.instance("i1", caIss)
	dir := t.TempDir()
	cfg := e.config("n1", in)
	cfg.StateDir = dir
	n1 := e.newNode(cfg)
	r := e.run(n1)
	eventually(t, 10*time.Second, "connected", func() bool { return in.srv.Connected("n1") })
	serial := n1.Serial()
	r.cancel()
	r.wait(t, 10*time.Second)
	eventually(t, 5*time.Second, "disconnected", func() bool { return !in.srv.Connected("n1") })

	n2 := e.newNode(cfg)
	r = e.run(n2)
	eventually(t, 10*time.Second, "connected again", func() bool { return in.srv.Connected("n1") })
	if n2.Serial() != serial || e.f.joinCount() != 1 {
		t.Fatalf("serial %s (was %s), %d joins", n2.Serial(), serial, e.f.joinCount())
	}

	// Another token: the node joins again.
	r.cancel()
	cfg.JoinToken = e.token()
	n4 := e.newNode(cfg)
	e.run(n4)
	eventually(t, 10*time.Second, "joined again", func() bool { return e.f.joinCount() == 2 && n4.Serial() != "" })
	if id, err := loadIdentity(dir, testTD); err != nil || id.serial() != n4.Serial() {
		t.Fatalf("kept identity: %v", err)
	}
	if n4.Serial() == serial {
		t.Fatal("the new token did not join again")
	}
}

// SaveIdentity gives a node an identity without a join.
func TestSaveIdentity(t *testing.T) {
	e := newEnv(t)
	in := e.instance("i1", caIss)
	dir := t.TempDir()
	key, csr := newKeyCSR(t, "n7")
	cert, _, err := caIss.IssueNode(csr, "n7", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveIdentity(dir, testTD, key, cert); err != nil {
		t.Fatal(err)
	}
	cfg := e.config("", in)
	cfg.JoinToken, cfg.StateDir = "", dir
	n := e.newNode(cfg)
	e.run(n)
	eventually(t, 10*time.Second, "connected", func() bool { return in.srv.Connected("n7") })
	if e.f.joinCount() != 0 || n.Name() != "n7" {
		t.Fatalf("%d joins, name %q", e.f.joinCount(), n.Name())
	}
}

// The refusal server's PERMISSION_DENIED stops the node from redialling:
// it must join again.
func TestRefusedNodeStopsRedialling(t *testing.T) {
	e := newEnv(t)
	in := e.instance("i1", caIss)
	r := e.startRun("n1", in)
	serial := r.n.Serial()
	e.f.refuse(serial, "node n1 was removed; join it again")
	in.srv.Recheck(t.Context(), "n1")
	eventually(t, 5*time.Second, "disconnected", func() bool { return !in.srv.Connected("n1") })
	// The uplink redials, lands on the refusal server and is told.
	runErr := r.wait(t, 10*time.Second)
	if !errors.Is(runErr, ErrRefused) || runErr.Error() != ErrRefused.Error()+": node n1 was removed; join it again" {
		t.Fatalf("Run: %v", runErr)
	}
	before := e.f.acceptChecks(serial)
	time.Sleep(1500 * time.Millisecond)
	if after := e.f.acceptChecks(serial); after > before+1 {
		t.Fatalf("the refused node kept dialling: %d accept checks after %d", after, before)
	}
}
