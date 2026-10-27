// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package fleet

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"

	fleetv1 "github.com/gardenlinux/feos/go/gen/feos/fleet/v1"
	"github.com/gardenlinux/feos/go/internal/db"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/store"
)

// definitive reports an error nodelink acts on.
func definitive(err error) bool {
	return errors.Is(err, model.ErrNodeRefused) || errors.Is(err, model.ErrCertSuperseded)
}

func TestCheckNode(t *testing.T) {
	h := newHarness(t)
	serial := h.joined("n1")
	if at, err := h.s.CheckNode(bg, "n1", serial, "x", true); err != nil || !at.IsZero() {
		t.Fatalf("a valid certificate: %v %v", at, err)
	}
	if at, err := h.s.CheckNode(bg, "n1", serial, "x", false); err != nil || !at.IsZero() {
		t.Fatalf("a valid certificate re-checked: %v %v", at, err)
	}
	refused := func(node, serial, text string) {
		t.Helper()
		for _, atAccept := range []bool{true, false} {
			_, err := h.s.CheckNode(bg, node, serial, "x", atAccept)
			if !errors.Is(err, model.ErrNodeRefused) || err.Error() != text {
				t.Fatalf("CheckNode(%s, %s, atAccept %v) = %v; want the refusal %q", node, serial, atAccept, err, text)
			}
		}
	}
	refused("n1", "ffff", "certificate of node n1 is unknown; join it again")
	// A serial of another node is unknown for this one.
	other := h.joined("n2")
	refused("n1", other, "certificate of node n1 is unknown; join it again")

	h.exec("UPDATE node_certs SET revoked_at = now(), revoked_reason = 'rejoined' WHERE serial = $1", serial)
	refused("n1", serial, "certificate of node n1 was revoked (rejoined); join it again")
	h.exec("UPDATE node_certs SET revoked_at = NULL, revoked_reason = '' WHERE serial = $1", serial)

	h.exec("UPDATE nodes SET status = 'removed', removed_at = now() WHERE name = 'n1'")
	refused("n1", serial, "node n1 was removed; join it again")
	h.exec("UPDATE nodes SET status = 'ready', removed_at = NULL, identity_forked_at = now() WHERE name = 'n1'")
	refused("n1", serial, "identity of node n1 forked; join it again with a token pinned to its name")
	h.exec("UPDATE nodes SET identity_forked_at = NULL WHERE name = 'n1'")

	// A database failure is transient: nodelink keeps the connection and
	// retries.
	ctx, cancel := context.WithCancel(bg)
	cancel()
	if _, err := h.s.CheckNode(ctx, "n1", serial, "x", false); err == nil || definitive(err) {
		t.Fatalf("a failed check: %v; want a transient error", err)
	}
	broken := New(Config{InstanceID: "i2", Log: h.s.cfg.Log}, Deps{DB: h.d, Issuer: h.iss, Links: h.links})
	defer broken.stop()
	h.exec("ALTER TABLE node_certs RENAME TO node_certs_moved")
	_, err := broken.CheckNode(bg, "n1", serial, "x", true)
	h.exec("ALTER TABLE node_certs_moved RENAME TO node_certs")
	if err == nil || definitive(err) {
		t.Fatalf("a check the database fails: %v; want a transient error", err)
	}
}

func TestCertificateLineageAndFork(t *testing.T) {
	h := newHarness(t)
	first := h.joined("n1")
	n := h.node("n1")
	if n.CurrentSerial != first {
		t.Fatalf("current serial %q, want %q", n.CurrentSerial, first)
	}
	if c, _ := h.st.GetNodeCert(bg, first); c.FirstSeenAt.IsZero() {
		t.Fatal("first_seen_at not set")
	}

	// The node renews and presents the new certificate: it becomes
	// current, the old one is revoked a grace ahead (the drain timeout of
	// 5 minutes + 10), and ferun_node tells every instance.
	ch, unsubscribe := h.d.Subscribe(db.ChanNode)
	defer unsubscribe()
	resp, err := h.s.UplinkServer().RenewCertificate(uplinkCtx("n1", first), &fleetv1.RenewCertificateRequest{Csr: newCSR(t)})
	if err != nil {
		t.Fatal(err)
	}
	second := serialOf(t, resp.GetCertificate())
	h.s.OnPresented("n1", second, "198.51.100.1:6000")
	expectPayload(t, ch, "n1")
	if n := h.node("n1"); n.CurrentSerial != second {
		t.Fatalf("current serial %q after the renewal", n.CurrentSerial)
	}
	c, _ := h.st.GetNodeCert(bg, first)
	var dbNow time.Time
	h.scalar(&dbNow, "SELECT now()")
	if grace := c.RevokedAt.Sub(dbNow); c.RevokedReason != store.RevokedSuperseded || grace < 14*time.Minute || grace > 15*time.Minute+5*time.Second {
		t.Fatalf("superseded certificate %+v: grace %v; want 15 minutes", c, c.RevokedAt.Sub(dbNow))
	}

	// Within the grace the old certificate is valid, with its revocation
	// time ahead, at accept and on a re-check: the node's own draining
	// connections.
	for _, atAccept := range []bool{false, true} {
		at, err := h.s.CheckNode(bg, "n1", first, "198.51.100.1:5000", atAccept)
		if err != nil || !at.Equal(c.RevokedAt) {
			t.Fatalf("superseded within its grace (atAccept %v): %v %v", atAccept, at, err)
		}
	}
	// Presenting the old one again changes nothing.
	h.s.OnPresented("n1", first, "198.51.100.1:5000")
	if n := h.node("n1"); n.CurrentSerial != second || n.Forked() {
		t.Fatalf("node after the old certificate's presentation %+v", n)
	}

	// Past the grace: a re-check of an old connection closes it, as at
	// the drain timeout, and forks nothing.
	h.exec("UPDATE node_certs SET revoked_at = now() - interval '1 second' WHERE serial = $1", first)
	if _, err := h.s.CheckNode(bg, "n1", first, "198.51.100.1:5000", false); !errors.Is(err, model.ErrCertSuperseded) {
		t.Fatalf("re-check past the grace: %v", err)
	}
	if n := h.node("n1"); n.Forked() {
		t.Fatal("a re-check forked the node")
	}
	if _, err := h.s.CheckNode(bg, "n1", second, "x", false); err != nil {
		t.Fatalf("the current certificate after the re-check: %v", err)
	}

	// A new handshake with it: two holders use the identity. The node is
	// forked, every certificate revoked, ferun_node sent.
	_, err = h.s.CheckNode(bg, "n1", first, "203.0.113.9:4444", true)
	if !errors.Is(err, model.ErrNodeRefused) || err.Error() != "identity of node n1 forked; join it again with a token pinned to its name" {
		t.Fatalf("superseded certificate presented past its grace: %v", err)
	}
	expectPayload(t, ch, "n1")
	n = h.node("n1")
	if !n.Forked() || !strings.HasPrefix(n.StatusDescription, "identity forked: a superseded certificate was presented at ") ||
		!strings.Contains(n.StatusDescription, "from 203.0.113.9:4444; join the node again with a token pinned to its name") {
		t.Fatalf("forked node %+v", n)
	}
	if c, _ := h.st.GetNodeCert(bg, first); c.RevokedReason != store.RevokedSuperseded {
		t.Fatalf("the superseded certificate after the fork %+v", c)
	}
	if c, _ := h.st.GetNodeCert(bg, second); c.RevokedReason != store.RevokedForked || c.RevokedAt.IsZero() {
		t.Fatalf("the current certificate after the fork %+v", c)
	}
	if _, err := h.s.CheckNode(bg, "n1", second, "x", false); !errors.Is(err, model.ErrNodeRefused) {
		t.Fatalf("the current certificate after the fork: %v", err)
	}
	// Renewing and pod credentials are refused for a forked node.
	_, err = h.s.UplinkServer().RenewCertificate(uplinkCtx("n1", second), &fleetv1.RenewCertificateRequest{Csr: newCSR(t)})
	wantCode(t, err, codes.PermissionDenied, "forked")

	// A pinned re-join ends the fork.
	resp2, err := h.join("n1", h.token("n1"), newCSR(t))
	if err != nil {
		t.Fatal(err)
	}
	if n := h.node("n1"); n.Forked() {
		t.Fatal("still forked after a pinned re-join")
	}
	if _, err := h.s.CheckNode(bg, "n1", serialOf(t, resp2.GetCertificate()), "x", true); err != nil {
		t.Fatalf("re-joined certificate: %v", err)
	}
}

func TestRenewCertificate(t *testing.T) {
	h := newHarness(t)
	serial := h.joined("n1")
	csr := newCSR(t)
	resp, err := h.s.UplinkServer().RenewCertificate(uplinkCtx("n1", serial), &fleetv1.RenewCertificateRequest{Csr: csr})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetCertificate().GetSpiffeId() != "spiffe://feos.test/node/n1" {
		t.Fatalf("renewed %v", resp.GetCertificate())
	}
	renewed := serialOf(t, resp.GetCertificate())
	certs, err := h.st.ListNodeCerts(bg, "n1")
	if err != nil || len(certs) != 2 {
		t.Fatalf("history %+v, %v", certs, err)
	}
	c, _ := h.st.GetNodeCert(bg, renewed)
	if c.Kind != store.CertRenew || c.PreviousSerial != serial || c.Peer != "198.51.100.1:5000" || c.KeyHash == "" {
		t.Fatalf("renewal %+v", c)
	}
	var rows int
	h.scalar(&rows, "SELECT count(*) FROM activity WHERE node = 'n1' AND kind = 'node_cert' AND detail->>'kind' = 'renew'")
	if rows != 1 {
		t.Fatalf("%d renewal activity rows", rows)
	}

	// Not presented yet: the old certificate still renews (the node may
	// retry after a lost answer).
	if _, err := h.s.UplinkServer().RenewCertificate(uplinkCtx("n1", serial), &fleetv1.RenewCertificateRequest{Csr: csr}); err != nil {
		t.Fatalf("renew again: %v", err)
	}
	// Once the new one was presented, the old one is superseded. Within
	// its grace it still passes CheckNode's conditions, so a call that got
	// here before nodelink's re-check marked its uplink superseded is
	// served; past its grace it is answered as nodelink answers the calls
	// on a superseded uplink.
	h.s.OnPresented("n1", renewed, "x")
	if c, _ := h.st.GetNodeCert(bg, serial); c.RevokedReason != store.RevokedSuperseded || c.RevokedAt.IsZero() {
		t.Fatalf("the old certificate after the presentation %+v", c)
	}
	if _, err := h.s.UplinkServer().RenewCertificate(uplinkCtx("n1", serial), &fleetv1.RenewCertificateRequest{Csr: csr}); err != nil {
		t.Fatalf("renew with a superseded certificate in its grace: %v", err)
	}
	h.exec("UPDATE node_certs SET revoked_at = now() - interval '1 second' WHERE serial = $1", serial)
	_, err = h.s.UplinkServer().RenewCertificate(uplinkCtx("n1", serial), &fleetv1.RenewCertificateRequest{Csr: csr})
	wantCode(t, err, codes.Unavailable, "certificate superseded; reconnect with the node's newest certificate")

	_, err = h.s.UplinkServer().RenewCertificate(uplinkCtx("n1", renewed), &fleetv1.RenewCertificateRequest{Csr: []byte("junk")})
	wantCode(t, err, codes.InvalidArgument, "certificate signing request")
	_, err = h.s.UplinkServer().RenewCertificate(uplinkCtx("n1", "abcdef"), &fleetv1.RenewCertificateRequest{Csr: csr})
	wantCode(t, err, codes.PermissionDenied, "certificate of node n1 is unknown; join it again")
	_, err = h.s.UplinkServer().RenewCertificate(bg, &fleetv1.RenewCertificateRequest{Csr: csr})
	wantCode(t, err, codes.Unauthenticated, "")
	_, err = h.s.UplinkServer().RenewCertificate(uplinkCtx("ghost", "abcdef"), &fleetv1.RenewCertificateRequest{Csr: csr})
	wantCode(t, err, codes.PermissionDenied, "")

	h.exec("UPDATE node_certs SET revoked_at = now(), revoked_reason = 'rejoined' WHERE serial = $1", renewed)
	_, err = h.s.UplinkServer().RenewCertificate(uplinkCtx("n1", renewed), &fleetv1.RenewCertificateRequest{Csr: csr})
	wantCode(t, err, codes.PermissionDenied, "certificate of node n1 was revoked (rejoined); join it again")

	h.exec("UPDATE node_certs SET revoked_at = NULL, revoked_reason = '' WHERE serial = $1", renewed)
	h.exec("UPDATE nodes SET status = 'removed', removed_at = now() WHERE name = 'n1'")
	_, err = h.s.UplinkServer().RenewCertificate(uplinkCtx("n1", renewed), &fleetv1.RenewCertificateRequest{Csr: csr})
	wantCode(t, err, codes.PermissionDenied, "node n1 was removed; join it again")
}

// A presentation that changes nothing, the steady state of every accept,
// neither takes the node's row lock nor writes: it is not held up by a
// writer of the row. A presentation that changes the lineage takes the
// lock, at most 4 at once per instance, however many arrive.
func TestPresentationLocks(t *testing.T) {
	h := newHarness(t)
	first := h.joined("n1")
	if news, err := h.st.PresentationNews(bg, "n1", first); err != nil || news {
		t.Fatalf("the current certificate, presented: news %v %v", news, err)
	}
	if news, err := h.st.PresentationNews(bg, "n1", "abcdef"); err != nil || !news {
		t.Fatalf("an unknown certificate: news %v %v", news, err)
	}
	resp, err := h.s.UplinkServer().RenewCertificate(uplinkCtx("n1", first), &fleetv1.RenewCertificateRequest{Csr: newCSR(t)})
	if err != nil {
		t.Fatal(err)
	}
	second := serialOf(t, resp.GetCertificate())
	if news, err := h.st.PresentationNews(bg, "n1", second); err != nil || !news {
		t.Fatalf("a renewed certificate, not presented: news %v %v", news, err)
	}

	// Another transaction holds the node's row.
	tx, err := h.d.Pool.Begin(bg)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(bg)
	if _, err := tx.Exec(bg, "SELECT 1 FROM nodes WHERE name = 'n1' FOR UPDATE"); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	for range 20 {
		h.s.OnPresented("n1", first, "198.51.100.1:5000")
	}
	if took := time.Since(start); took > time.Second {
		t.Fatalf("20 presentations of the current certificate took %v behind the row lock", took)
	}

	// The renewed certificate's first presentations queue on the lock,
	// four at a time.
	var done sync.WaitGroup
	for range 10 {
		done.Add(1)
		go func() {
			defer done.Done()
			h.s.OnPresented("n1", second, "198.51.100.1:6000")
		}()
	}
	waiting := func() int {
		var n int
		h.scalar(&n, `SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock'`)
		return n
	}
	waitFor(t, "four presentations waiting on the lock", 5*time.Second, func() bool { return waiting() == maxPresentations })
	never := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(never) {
		if n := waiting(); n > maxPresentations {
			t.Fatalf("%d presentations wait on the node's row lock", n)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := tx.Rollback(bg); err != nil {
		t.Fatal(err)
	}
	done.Wait()
	if n := h.node("n1"); n.CurrentSerial != second {
		t.Fatalf("current serial %q after the presentations, want %q", n.CurrentSerial, second)
	}
	if news, err := h.st.PresentationNews(bg, "n1", first); err != nil || news {
		t.Fatalf("the superseded certificate: news %v %v", news, err)
	}
}
