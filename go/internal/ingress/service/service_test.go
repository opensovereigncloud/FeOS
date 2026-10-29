// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"

	"github.com/gardenlinux/feos/go/internal/apierr"
	"github.com/gardenlinux/feos/go/internal/db"
	"github.com/gardenlinux/feos/go/internal/ingress/wire"
	"github.com/gardenlinux/feos/go/internal/mesh"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/nodelink"
	"github.com/gardenlinux/feos/go/internal/pki"
	"github.com/gardenlinux/feos/go/internal/store"
)

// Join: a pinned token joins its name whatever the proposal, an unpinned
// one creates the proposed name only; a node's token, an unknown or
// expired one, and a used one with another key are refused; a used one
// with the same key within the window answers again.
func TestJoin(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	jc := h.dial(nil)

	// A pinned token creates a new name.
	key := newKey(t)
	resp, err := h.join(jc, h.token("in-a"), "proposed", key)
	if err != nil {
		t.Fatal(err)
	}
	id := identityOf(t, resp.Name, resp.Certificate, key)
	if resp.Name != "in-a" || resp.Certificate.SpiffeID != pki.IngressID(testTD, "in-a") ||
		len(id.leaf.URIs) != 1 || id.leaf.URIs[0].String() != pki.IngressID(testTD, "in-a") {
		t.Fatalf("pinned join %+v", resp)
	}
	if !resp.Certificate.RenewAfter.After(resp.Certificate.NotBefore) || len(resp.Certificate.TrustBundle) == 0 {
		t.Errorf("certificate %+v", resp.Certificate)
	}
	if g := h.ingress("in-a"); g.Status != model.IngressJoined || g.MeshSubnet != 0 {
		t.Errorf("row %+v", g)
	}
	c := h.cert(id.serial)
	if c.Ingress != "in-a" || c.Kind != store.CertRejoin || c.Peer == "" || c.TokenID.String() == "" || !c.RevokedAt.IsZero() {
		t.Errorf("certificate row %+v", c)
	}
	toks, err := h.st.ListJoinTokens(bg)
	if err != nil || len(toks) != 1 || toks[0].UsedName != "in-a" || toks[0].UsedSerial != id.serial ||
		toks[0].ID != c.TokenID {
		t.Fatalf("tokens %+v %v", toks, err)
	}

	// An unpinned token takes the proposed name; a second one for the
	// same name exists already.
	resp, err = h.join(jc, h.token(""), "in-b", newKey(t))
	if err != nil || resp.Name != "in-b" {
		t.Fatalf("unpinned join %+v %v", resp, err)
	}
	if c := h.cert(identityOf(t, "in-b", resp.Certificate, newKey(t)).serial); c.Kind != store.CertJoin {
		t.Errorf("unpinned kind %s", c.Kind)
	}
	_, err = h.join(jc, h.token(""), "in-b", newKey(t))
	wantCode(t, "a second unpinned join", err, codes.AlreadyExists, "ingress in-b exists")

	// A node's token is refused by its form, before any lookup; a token
	// of kind node in the form of an ingress's after it.
	nodeToken, nodeSent, nodeHash, err := pki.NewJoinToken(h.iss.JoinPin())
	if err != nil || nodeToken == "" {
		t.Fatal(err)
	}
	if _, err := h.st.CreateJoinToken(bg, nodeHash, "test", model.JoinKindNode, "", time.Hour); err != nil {
		t.Fatal(err)
	}
	_, err = h.join(jc, nodeSent, "in-c", newKey(t))
	wantCode(t, "a node's token", err, codes.InvalidArgument, "this join token is for a node")
	_, kindNode, kindNodeHash, err := pki.NewIngressJoinToken(h.iss.JoinPin())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.st.CreateJoinToken(bg, kindNodeHash, "test", model.JoinKindNode, "", time.Hour); err != nil {
		t.Fatal(err)
	}
	_, err = h.join(jc, kindNode, "in-c", newKey(t))
	wantCode(t, "a token of kind node", err, codes.PermissionDenied, "this join token is for a node")
	if toks, err := h.st.ListJoinTokens(bg); err != nil || slices.ContainsFunc(toks, func(t store.JoinToken) bool {
		return t.Kind == model.JoinKindNode && t.Used()
	}) {
		t.Fatalf("a node's token was used: %+v %v", toks, err)
	}
	// An unknown token, an expired one.
	_, unknown, _, err := pki.NewIngressJoinToken(h.iss.JoinPin())
	if err != nil {
		t.Fatal(err)
	}
	_, err = h.join(jc, unknown, "in-c", newKey(t))
	wantCode(t, "an unknown token", err, codes.PermissionDenied, "unknown join token")
	expired := h.token("")
	h.exec("UPDATE join_tokens SET expires_at = now() - interval '1 second' WHERE hash = $1", pki.HashJoinSecret(expired))
	_, err = h.join(jc, expired, "in-c", newKey(t))
	wantCode(t, "an expired token", err, codes.PermissionDenied, "expired")
	if _, err := h.st.GetIngress(bg, "in-c"); !errors.Is(err, store.ErrNoRow) {
		t.Fatalf("a refused join made a row: %v", err)
	}

	// Used: another key is refused, the same key answers again with a
	// new certificate of the same name and kind.
	tok, key := h.token(""), newKey(t)
	first, err := h.join(jc, tok, "in-d", key)
	if err != nil {
		t.Fatal(err)
	}
	_, err = h.join(jc, tok, "in-d", newKey(t))
	wantCode(t, "a used token with another key", err, codes.PermissionDenied, "already used")
	again, err := h.join(jc, tok, "other-name", key)
	if err != nil || again.Name != "in-d" {
		t.Fatalf("the same key again: %+v %v", again, err)
	}
	a, b := identityOf(t, "in-d", first.Certificate, key), identityOf(t, "in-d", again.Certificate, key)
	if a.serial == b.serial || h.cert(b.serial).Kind != store.CertJoin || !h.cert(a.serial).RevokedAt.IsZero() {
		t.Fatalf("answer again: %s %s", a.serial, b.serial)
	}
	// Past the window it is refused.
	h.exec("UPDATE join_tokens SET used_at = now() - interval '11 minutes' WHERE hash = $1", pki.HashJoinSecret(tok))
	_, err = h.join(jc, tok, "in-d", key)
	wantCode(t, "the same key past the window", err, codes.PermissionDenied, "already used")

	// Malformed requests.
	_, err = h.join(jc, "feos-ingress-join-short", "in-e", newKey(t))
	wantCode(t, "a short token", err, codes.InvalidArgument, "feos-ingress-join-<secret>")
	_, err = h.join(jc, "feos-ingress-join-"+strings.Repeat("*", 43), "in-e", newKey(t))
	wantCode(t, "a token that is not base64url", err, codes.InvalidArgument, "feos-ingress-join-<secret>")
	_, err = h.join(jc, h.token(""), "Bad_Name", newKey(t))
	wantCode(t, "a bad name", err, codes.InvalidArgument, "ingress name")
	_, err = jc.api.Join(bg, &wire.JoinRequest{Token: h.token(""), Name: "in-e", CSR: []byte("junk")})
	wantCode(t, "a bad CSR", err, codes.InvalidArgument, "")
}

// A used token is not answered again once a removal overtook its first
// answer.
func TestJoinAgainAfterRemoval(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	jc := h.dial(nil)
	tok, key := h.token(""), newKey(t)
	if _, err := h.join(jc, tok, "in-0", key); err != nil {
		t.Fatal(err)
	}
	if found, err := h.s.RemoveIngress(bg, "in-0"); err != nil || !found {
		t.Fatalf("remove: %v %v", found, err)
	}
	_, err := h.join(jc, tok, "in-0", key)
	wantCode(t, "a repeat after the removal", err, codes.PermissionDenied, "already used")
}

// A join again keeps the ingress's /96, revokes its older certificates
// and closes their connections within 5 s of the notification; the new
// certificate may bring a new WireGuard key.
func TestRejoin(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	old, oc, first := h.connect("in-0", 1, "10.0.0.1:51820")
	subnet := h.ingress("in-0").MeshSubnet
	if subnet == 0 || h.links.IngressConnections("in-0") != 1 {
		t.Fatalf("subnet %d, connections %d", subnet, h.links.IngressConnections("in-0"))
	}

	re, _ := h.mustJoin("in-0", "in-0")
	if c := h.cert(re.serial); c.Kind != store.CertRejoin {
		t.Errorf("kind %s", c.Kind)
	}
	if c := h.cert(old.serial); c.RevokedReason != store.RevokedRejoined || c.RevokedAt.IsZero() {
		t.Errorf("old certificate %+v", c)
	}
	g := h.ingress("in-0")
	if g.MeshSubnet != subnet || g.MeshPublicKey != nil || g.MeshEndpoint != "" || g.CurrentSerial != "" {
		t.Errorf("row after the join again %+v", g)
	}
	closedWithin(t, oc.conn(t, 0), 5*time.Second, "the old certificate's connection")
	eventually(t, 5*time.Second, "unregistered", func() bool { return h.links.IngressConnections("in-0") == 0 })

	// The key of before the join again is retired: a copy of the state
	// directory does not get back on the mesh with it.
	nc := h.dial(re)
	peers := h.peersVersion()
	_, err := nc.hello(wgKey(1), "10.0.0.1:51820")
	wantCode(t, "the retired key", err, codes.FailedPrecondition,
		"the mesh key of ingress in-0 was retired by its join again; make a new one")
	if g := h.ingress("in-0"); g.MeshPublicKey != nil || !bytes.Equal(g.PreviousMeshPublicKey, wgKey(1)) || g.CurrentSerial != "" ||
		h.peersVersion() != peers {
		t.Fatalf("a refused Hello changed the row %+v or the peers", g)
	}

	// The same /96, a new key.
	resp, err := nc.hello(wgKey(2), "10.0.0.1:51820")
	if err != nil {
		t.Fatal(err)
	}
	if resp.Prefix != first.Prefix || resp.Address != first.Address {
		t.Errorf("after the join again %+v, before %+v", resp, first)
	}
	if g := h.ingress("in-0"); g.MeshSubnet != subnet || !bytes.Equal(g.MeshPublicKey, wgKey(2)) || g.CurrentSerial != re.serial {
		t.Errorf("row %+v", g)
	}

	// The old certificate's new connections are refused.
	_, err = h.dial(old).hello(wgKey(1), "10.0.0.1:51820")
	wantCode(t, "the old certificate", err, codes.PermissionDenied, "was revoked (rejoined); join it again")
}

// Renew records the presenting certificate as the previous one; a Hello
// with the renewed certificate supersedes the older ones, revoked a grace
// ahead: their connections close then, and new ones are refused.
func TestRenewAndSupersede(t *testing.T) {
	h := newHarness(t, harnessOpts{grace: time.Second, renewAnytime: true})
	old, oc, _ := h.connect("in-0", 1, "10.0.0.1:51820")
	if g := h.ingress("in-0"); g.CurrentSerial != old.serial {
		t.Fatalf("current %s, want %s", g.CurrentSerial, old.serial)
	}

	key := old.key
	rr, err := oc.renew(t, key)
	if err != nil {
		t.Fatal(err)
	}
	renewed := identityOf(t, "in-0", rr.Certificate, key)
	c := h.cert(renewed.serial)
	if c.Kind != store.CertRenew || c.PreviousSerial != old.serial || c.Ingress != "in-0" || c.Peer == "" {
		t.Fatalf("renewed row %+v", c)
	}
	// No revocation at the renewal.
	if c := h.cert(old.serial); !c.RevokedAt.IsZero() {
		t.Fatalf("renewal revoked %+v", c)
	}
	// The old certificate still says Hello, and stays current.
	if _, err := oc.hello(wgKey(1), "10.0.0.1:51820"); err != nil {
		t.Fatal(err)
	}

	var before time.Time
	h.scalar(&before, "SELECT now()")
	nc := h.dial(renewed)
	if _, err := nc.hello(wgKey(1), "10.0.0.1:51820"); err != nil {
		t.Fatal(err)
	}
	if g := h.ingress("in-0"); g.CurrentSerial != renewed.serial {
		t.Fatalf("current %s, want %s", g.CurrentSerial, renewed.serial)
	}
	sup := h.cert(old.serial)
	if sup.RevokedReason != store.RevokedSuperseded || sup.RevokedAt.Before(before.Add(time.Second)) {
		t.Fatalf("superseded %+v (Hello after %v)", sup, before)
	}
	if !h.cert(renewed.serial).RevokedAt.IsZero() {
		t.Fatal("the renewed certificate is revoked")
	}
	// Its connection closes at the revocation; the new one stays.
	closedWithin(t, oc.conn(t, 0), 5*time.Second, "the superseded certificate's connection")
	eventually(t, 5*time.Second, "one connection left", func() bool { return h.links.IngressConnections("in-0") == 1 })
	// Past the grace its new connections are refused.
	time.Sleep(time.Until(sup.RevokedAt))
	_, err = h.dial(old).hello(wgKey(1), "10.0.0.1:51820")
	wantCode(t, "the superseded certificate", err, codes.PermissionDenied, "was superseded; reconnect with the newest certificate")
	if _, err := nc.hello(wgKey(1), "10.0.0.1:51820"); err != nil {
		t.Fatalf("the current certificate: %v", err)
	}
}

// A copied certificate never yields one that outlives the instance's own
// (F5 of the security review): only the newest certificate is renewed, a
// superseded one not even in its grace, and for its own key alone.
func TestRenewRefusals(t *testing.T) {
	h := newHarness(t, harnessOpts{grace: time.Minute, renewAnytime: true})
	j0, c0, _ := h.connect("in-0", 1, "10.0.0.1:51820")

	// Another key needs a join.
	_, err := c0.renew(t, newKey(t))
	wantCode(t, "another key", err, codes.InvalidArgument, "is not for the key of certificate "+j0.serial)

	// A renewal that was never presented is not the newest: it is not
	// renewed again, so a copy cannot chain renewals beside the instance.
	rr, err := c0.renew(t, j0.key)
	if err != nil {
		t.Fatal(err)
	}
	c1 := identityOf(t, "in-0", rr.Certificate, j0.key)
	n1 := h.dial(c1)
	_, err = n1.renew(t, j0.key)
	wantCode(t, "a certificate not presented", err, codes.PermissionDenied,
		"certificate "+c1.serial+" is not the newest certificate of ingress in-0")
	// The current one renews again (an answer that was lost).
	if _, err := c0.renew(t, j0.key); err != nil {
		t.Fatalf("the current certificate again: %v", err)
	}

	// Presented, c1 is current and j0 superseded, within its grace still
	// connected, but never renewed.
	if _, err := n1.hello(wgKey(1), "10.0.0.1:51820"); err != nil {
		t.Fatal(err)
	}
	if g := h.ingress("in-0"); g.CurrentSerial != c1.serial {
		t.Fatalf("current %s, want %s", g.CurrentSerial, c1.serial)
	}
	if sup := h.cert(j0.serial); sup.RevokedReason != store.RevokedSuperseded || !sup.RevokedAt.After(time.Now()) {
		t.Fatalf("superseded %+v", sup)
	}
	_, err = c0.renew(t, j0.key)
	wantCode(t, "a superseded certificate in its grace", err, codes.PermissionDenied,
		"certificate "+j0.serial+" of ingress in-0 was superseded")
	_, err = h.dial(j0).renew(t, j0.key)
	wantCode(t, "a superseded certificate on a new connection", err, codes.PermissionDenied, "was superseded")
	rr, err = n1.renew(t, j0.key)
	if err != nil {
		t.Fatalf("the current certificate: %v", err)
	}
	if c := h.cert(identityOf(t, "in-0", rr.Certificate, j0.key).serial); c.PreviousSerial != c1.serial || c.Kind != store.CertRenew {
		t.Fatalf("renewed %+v", c)
	}

	// Without a current certificate (no Hello since the join), the newest
	// one not revoked is renewed, and no other.
	tok, key := h.token(""), newKey(t)
	first, err := h.join(h.dial(nil), tok, "in-1", key)
	if err != nil {
		t.Fatal(err)
	}
	again, err := h.join(h.dial(nil), tok, "in-1", key) // its first answer was lost
	if err != nil {
		t.Fatal(err)
	}
	a, b := identityOf(t, "in-1", first.Certificate, key), identityOf(t, "in-1", again.Certificate, key)
	_, err = h.dial(a).renew(t, key)
	wantCode(t, "an older join certificate", err, codes.PermissionDenied, "is not the newest certificate of ingress in-1")
	if _, err := h.dial(b).renew(t, key); err != nil {
		t.Fatalf("the newest join certificate: %v", err)
	}
}

// A certificate is renewed from 10 minutes before its renew time on, by
// the database's clock.
func TestRenewNotBeforeItsTime(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	id, c, _ := h.connect("in-0", 1, "10.0.0.1:51820")
	_, err := c.renew(t, id.key)
	wantCode(t, "a new certificate", err, codes.FailedPrecondition, "certificate "+id.serial+" of ingress in-0 is renewed from ")
	// Its renew time, half its lifetime, 15 minutes ahead: still early.
	h.exec(`UPDATE ingress_certs SET not_before = now() - interval '100 hours',
		not_after = now() + interval '100 hours 30 minutes' WHERE serial = $1`, id.serial)
	_, err = c.renew(t, id.key)
	wantCode(t, "15 minutes before its renew time", err, codes.FailedPrecondition, "is renewed from ")
	// 5 minutes ahead: within the margin for the ingress's clock.
	h.exec(`UPDATE ingress_certs SET not_after = now() + interval '100 hours 10 minutes' WHERE serial = $1`, id.serial)
	if _, err := c.renew(t, id.key); err != nil {
		t.Fatalf("5 minutes before its renew time: %v", err)
	}
}

// renewRefusal case by case.
func TestRenewRefusal(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	ok := store.IngressCert{Serial: "ab", KeyHash: "k", NotBefore: now.Add(-100 * time.Hour), NotAfter: now.Add(68 * time.Hour)}
	for _, c := range []struct {
		cert   store.IngressCert
		newest string
		key    string
		want   string
	}{
		{ok, "ab", "k", ""},
		{func() store.IngressCert { c := ok; c.RevokedAt = now.Add(time.Minute); return c }(), "ab", "k", "was superseded"},
		{ok, "cd", "k", "is not the newest certificate of ingress g"},
		{ok, "", "k", "is not the newest certificate of ingress g"},
		{ok, "ab", "other", "is not for the key of certificate ab"},
		{func() store.IngressCert { c := ok; c.NotAfter = now.Add(100*time.Hour + 21*time.Minute); return c }(), "ab", "k", "is renewed from"},
		{func() store.IngressCert { c := ok; c.NotAfter = now.Add(100*time.Hour + 19*time.Minute); return c }(), "ab", "k", ""},
	} {
		err := renewRefusal("g", c.cert, c.newest, c.key, now, 10*time.Minute)
		if (c.want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), c.want)) {
			t.Errorf("%+v newest %q key %q: %v, want %q", c.cert, c.newest, c.key, err, c.want)
		}
	}
}

// One WireGuard key, one peer (F7 of the security review): an ingress
// never takes a node's key or another ingress's, and the refusal tells it
// to make a new one.
func TestHelloKeyTaken(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	h.meshNode("n1", 5)
	id, _ := h.mustJoin("", "in-0")
	c := h.dial(id)
	peers := h.peersVersion()
	_, err := c.hello(wgKey(5), "10.0.0.1:51820")
	wantCode(t, "a node's key", err, codes.FailedPrecondition, "the mesh key of ingress in-0 is another peer's; make a new one")
	if g := h.ingress("in-0"); g.MeshPublicKey != nil || g.MeshSubnet != 0 || h.peersVersion() != peers {
		t.Fatalf("a refused Hello changed the row %+v or the peers", g)
	}
	h.connect("in-1", 6, "10.0.0.2:51820")
	_, err = c.hello(wgKey(6), "10.0.0.1:51820")
	wantCode(t, "another ingress's key", err, codes.FailedPrecondition, "is another peer's; make a new one")
	if _, err := c.hello(wgKey(7), "10.0.0.1:51820"); err != nil {
		t.Fatalf("a key of its own: %v", err)
	}
}

// Hello allocates the ingress's /96 at its first call and changes nothing
// when repeated; a new endpoint bumps the mesh's peers; the key is fixed.
func TestHello(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	id, c, resp := h.connect("in-0", 1, "10.0.0.1:51820")
	g := h.ingress("in-0")
	prefix := mesh.NodePrefix(testULA, g.MeshSubnet)
	if g.MeshSubnet == 0 || resp.Name != "in-0" || resp.PodNetwork != testULA.String() || resp.Prefix != prefix.String() ||
		resp.Address != mesh.HostAddress(prefix).String() || resp.AppsZone != testZone || resp.Instance != "i1" {
		t.Fatalf("Hello %+v, row %+v", resp, g)
	}
	if g.MeshPrefix != resp.Prefix || g.MeshAddress != resp.Address || !bytes.Equal(g.MeshPublicKey, wgKey(1)) ||
		g.MeshEndpoint != "10.0.0.1:51820" || g.Version != "test" || g.LastSeen.IsZero() || g.GroupVersion == 0 ||
		g.CurrentSerial != id.serial {
		t.Fatalf("row %+v", g)
	}

	// Again: the same answer, nothing bumped.
	peers := h.peersVersion()
	again, err := c.hello(wgKey(1), "10.0.0.1:51820")
	if err != nil || *again != *resp {
		t.Fatalf("again %+v %v", again, err)
	}
	if h.peersVersion() != peers || h.ingress("in-0").GroupVersion != g.GroupVersion {
		t.Fatal("a repeated Hello bumped a version")
	}

	// A new endpoint: the same /96, the peers bumped.
	moved, err := c.hello(wgKey(1), "[2001:db8::7]:51820")
	if err != nil || moved.Prefix != resp.Prefix {
		t.Fatalf("moved %+v %v", moved, err)
	}
	if v := h.peersVersion(); v != peers+1 {
		t.Fatalf("peers version %d, want %d", v, peers+1)
	}
	// Its address did not move: the group's version stays.
	if moved := h.ingress("in-0"); moved.MeshEndpoint != "[2001:db8::7]:51820" || moved.GroupVersion != g.GroupVersion {
		t.Fatalf("row %+v", moved)
	}

	// The key is fixed.
	_, err = c.hello(wgKey(2), "10.0.0.1:51820")
	wantCode(t, "another key", err, codes.FailedPrecondition, "the mesh key of ingress in-0 is fixed; join it again to change it")
	_, err = c.hello(wgKey(1)[:31], "10.0.0.1:51820")
	wantCode(t, "a short key", err, codes.InvalidArgument, "publicKey")
	_, err = c.hello(wgKey(1), "10.0.0.1")
	wantCode(t, "an endpoint without a port", err, codes.InvalidArgument, "endpoint")
	_, err = c.hello(make([]byte, 32), "10.0.0.1:51820")
	wantCode(t, "a key of zeros", err, codes.InvalidArgument, "small order")
	if g := h.ingress("in-0"); !bytes.Equal(g.MeshPublicKey, wgKey(1)) {
		t.Fatal("a refused Hello changed the key")
	}

	// Each connection with a Hello is known until it closes.
	if !h.s.saidHello(nodelink.IngressPeer{Conn: knownConn(t, h.s)}) {
		t.Fatal("no Hello recorded")
	}
	c.cc.Close()
	eventually(t, 5*time.Second, "the Hello forgotten", func() bool {
		h.s.hellosMu.Lock()
		defer h.s.hellosMu.Unlock()
		return len(h.s.hellos) == 0
	})
}

// knownConn returns the one connection with a Hello.
func knownConn(t *testing.T, s *Service) uint64 {
	t.Helper()
	s.hellosMu.Lock()
	defer s.hellosMu.Unlock()
	if len(s.hellos) != 1 {
		t.Fatalf("hellos %v", s.hellos)
	}
	for c := range s.hellos {
		return c
	}
	return 0
}

// A Hello on a cluster that is not meshed is refused.
func TestHelloNotMeshed(t *testing.T) {
	h := newHarness(t, harnessOpts{noMesh: true})
	id, _ := h.mustJoin("", "in-0")
	_, err := h.dial(id).hello(wgKey(1), "10.0.0.1:51820")
	wantCode(t, "Hello", err, codes.FailedPrecondition, notMeshedText)
	if g := h.ingress("in-0"); g.MeshSubnet != 0 || g.MeshPublicKey != nil {
		t.Fatalf("row %+v", g)
	}
}

// A removed ingress gets PERMISSION_DENIED on every call: its connections
// close at once, new ones are refused, and a call that comes before its
// connection's re-check is refused under the row lock.
func TestRemoval(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	id, c, _ := h.connect("in-0", 1, "10.0.0.1:51820")
	peers := h.peersVersion()
	members, version, err := h.st.IngressGroup(bg)
	if err != nil || len(members) != 1 {
		t.Fatalf("group %v %d %v", members, version, err)
	}

	found, err := h.s.RemoveIngress(bg, "in-0")
	if err != nil || !found {
		t.Fatalf("remove: %v %v", found, err)
	}
	if n := h.links.IngressConnections("in-0"); n != 0 {
		t.Fatalf("connections %d after the removal", n)
	}
	closedWithin(t, c.conn(t, 0), 5*time.Second, "the removed ingress's connection")
	if g := h.ingress("in-0"); g.Status != model.IngressRemoved || g.RemovedAt.IsZero() {
		t.Fatalf("row %+v", g)
	}
	if c := h.cert(id.serial); c.RevokedReason != store.RevokedRemoved {
		t.Fatalf("certificate %+v", c)
	}
	if h.peersVersion() != peers+1 {
		t.Error("the removal did not bump the peers")
	}
	if members, v, _ := h.st.IngressGroup(bg); len(members) != 0 || v != version+1 {
		t.Errorf("group after the removal %v %d", members, v)
	}

	nc := h.dial(id)
	for name, call := range map[string]func() error{
		"Hello": func() error { _, err := nc.hello(wgKey(1), "10.0.0.1:51820"); return err },
		"RenewCertificate": func() error {
			_, err := nc.renew(t, newKey(t))
			return err
		},
		"Wake": func() error { _, err := nc.api.Wake(bg, &wire.WakeRequest{Env: "e", Service: "s"}); return err },
		"Hold": func() error { _, err := nc.api.Hold(bg, &wire.HoldRequest{}); return err },
		"Sync": func() error {
			s, err := nc.api.Sync(bg)
			if err != nil {
				return err
			}
			_, err = s.Recv()
			return err
		},
	} {
		wantCode(t, name, call(), codes.PermissionDenied, "ingress in-0 was removed; join it again")
	}
	if n := h.links.IngressConnections("in-0"); n != 0 {
		t.Fatalf("a refused connection registered: %d", n)
	}

	// A call that comes before its connection's re-check.
	ctx := nodelink.ContextWithIngress(bg, nodelink.IngressPeer{Name: "in-0", Serial: id.serial, Addr: "192.0.2.1:1", Conn: 99})
	_, err = h.s.Hello(ctx, &wire.HelloRequest{PublicKey: wgKey(1), Endpoint: "10.0.0.1:51820"})
	wantCode(t, "Hello in the window", err, codes.PermissionDenied, "ingress in-0 was removed; join it again")
	_, err = h.s.RenewCertificate(ctx, &wire.RenewRequest{CSR: csrOf(t, newKey(t))})
	wantCode(t, "Renew in the window", err, codes.PermissionDenied, "ingress in-0 was removed; join it again")

	if found, err := h.s.RemoveIngress(bg, "nobody"); err != nil || found {
		t.Fatalf("remove an unknown ingress: %v %v", found, err)
	}
	// A pinned token joins it again.
	re, _ := h.mustJoin("in-0", "in-0")
	if _, err := h.dial(re).hello(wgKey(3), "10.0.0.1:51820"); err != nil {
		t.Fatalf("after a join again: %v", err)
	}
	if g := h.ingress("in-0"); g.Status != model.IngressJoined {
		t.Fatalf("row %+v", g)
	}
}

// ferun_ingress "*" re-checks every ingress connected here: a revocation
// written without a notification of its own closes the connection.
func TestRunRechecksAll(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	id, c, _ := h.connect("in-0", 1, "10.0.0.1:51820")
	// The Hello's own ferun_ingress "ingress:in-0" settles first.
	time.Sleep(500 * time.Millisecond)
	h.exec("UPDATE ingress_certs SET revoked_at = now(), revoked_reason = 'removed' WHERE serial = $1", id.serial)
	time.Sleep(200 * time.Millisecond)
	if h.links.IngressConnections("in-0") != 1 {
		t.Fatal("closed without a notification")
	}
	h.exec("SELECT pg_notify($1, $2)", db.ChanIngress, store.IngressPayloadAll)
	closedWithin(t, c.conn(t, 0), 5*time.Second, "after ferun_ingress *")
	eventually(t, 5*time.Second, "unregistered", func() bool { return h.links.IngressConnections("in-0") == 0 })
}

// CheckIngress refuses for good, with the texts the ingress reads, and
// returns a revocation ahead.
func TestCheckIngress(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	id, _ := h.mustJoin("", "in-0")
	revokeAt, err := h.s.CheckIngress(bg, "in-0", id.serial, "192.0.2.1:1")
	if err != nil || !revokeAt.IsZero() {
		t.Fatalf("a valid certificate: %v %v", revokeAt, err)
	}
	refusal := func(name, serial, text string) {
		t.Helper()
		_, err := h.s.CheckIngress(bg, name, serial, "192.0.2.1:1")
		if !errors.Is(err, nodelink.ErrIngressRefused) || err.Error() != text {
			t.Errorf("CheckIngress(%s, %s): %v, want %q", name, serial, err, text)
		}
	}
	refusal("in-1", id.serial, "certificate "+id.serial+" is not ingress in-1's")
	refusal("in-0", "abcd", "certificate abcd is not ingress in-0's")
	h.exec("UPDATE ingress_certs SET revoked_at = now() + interval '1 hour', revoked_reason = 'superseded' WHERE serial = $1", id.serial)
	if revokeAt, err := h.s.CheckIngress(bg, "in-0", id.serial, ""); err != nil || time.Until(revokeAt) < 50*time.Minute {
		t.Errorf("superseded ahead: %v %v", revokeAt, err)
	}
	h.exec("UPDATE ingress_certs SET revoked_at = now() - interval '1 second' WHERE serial = $1", id.serial)
	refusal("in-0", id.serial, "certificate "+id.serial+" of ingress in-0 was superseded; reconnect with the newest certificate")
	h.exec("UPDATE ingress_certs SET revoked_reason = 'rejoined' WHERE serial = $1", id.serial)
	refusal("in-0", id.serial, "certificate "+id.serial+" of ingress in-0 was revoked (rejoined); join it again")
	h.exec("UPDATE ingress_certs SET revoked_at = NULL, revoked_reason = '', not_after = now() - interval '1 second' WHERE serial = $1", id.serial)
	refusal("in-0", id.serial, "certificate "+id.serial+" of ingress in-0 expired; join it again")
	if _, err := h.s.RemoveIngress(bg, "in-0"); err != nil {
		t.Fatal(err)
	}
	refusal("in-0", id.serial, "ingress in-0 was removed; join it again")

	// A database error is transient.
	ctx, cancel := context.WithCancel(bg)
	cancel()
	if _, err := h.s.CheckIngress(ctx, "in-0", id.serial, ""); err == nil || errors.Is(err, nodelink.ErrIngressRefused) {
		t.Errorf("a failed read: %v", err)
	}
}

// The operator's side: tokens of kind ingress, the list, the removal.
func TestAdmin(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	_, _, _, err := h.s.IssueJoinToken(bg, "test", "Bad_Name", 0)
	var ae *apierr.Error
	if !errors.As(err, &ae) || ae.Status != http.StatusBadRequest {
		t.Fatalf("a bad name: %v", err)
	}
	id, token, expires, err := h.s.IssueJoinToken(bg, "op", "in-9", 0)
	if err != nil || !strings.HasSuffix(token, "."+h.iss.JoinPin()) || time.Until(expires) < 59*time.Minute {
		t.Fatalf("token %s %s %v %v", id, token, expires, err)
	}
	toks, err := h.st.ListJoinTokens(bg)
	if err != nil || len(toks) != 1 || toks[0].Kind != model.JoinKindIngress || toks[0].NodeName != "in-9" ||
		toks[0].CreatedBy != "op" || toks[0].ID != id {
		t.Fatalf("tokens %+v %v", toks, err)
	}

	cid, _, resp := h.connect("in-0", 1, "10.0.0.1:51820")
	h.mustJoin("", "in-1")
	list, err := h.s.ListIngresses(bg)
	if err != nil || len(list) != 2 {
		t.Fatalf("list %+v %v", list, err)
	}
	g0, g1 := list[0], list[1]
	if g0.Name != "in-0" || g0.Status != model.IngressJoined || g0.JoinedAt.IsZero() || !g0.RemovedAt.IsZero() ||
		g0.MeshPrefix != resp.Prefix || g0.MeshAddress != resp.Address || g0.MeshEndpoint != "10.0.0.1:51820" ||
		!bytes.Equal(g0.PublicKey, wgKey(1)) || g0.Version != "test" || g0.LastSeen.IsZero() || !g0.Connected ||
		g0.AdmittedVersion == 0 || !g0.CertNotAfter.Equal(cid.leaf.NotAfter) {
		t.Errorf("in-0 %+v", g0)
	}
	if g1.Name != "in-1" || g1.Connected || g1.MeshPrefix != "" || g1.PublicKey != nil || g1.CertNotAfter.IsZero() {
		t.Errorf("in-1 %+v", g1)
	}
	h.exec("UPDATE ingresses SET last_seen = now() - interval '31 seconds' WHERE name = 'in-0'")
	if list, _ := h.s.ListIngresses(bg); list[0].Connected {
		t.Error("connected 31 s after it was seen")
	}
	if _, err := h.s.RemoveIngress(bg, "in-1"); err != nil {
		t.Fatal(err)
	}
	if list, _ := h.s.ListIngresses(bg); list[1].Status != model.IngressRemoved || list[1].RemovedAt.IsZero() ||
		!list[1].CertNotAfter.IsZero() {
		t.Errorf("removed %+v", list[1])
	}
}

// Join belongs to the join server; Hello and the later methods to the
// ingress server.
func TestUnimplemented(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	_, c, _ := h.connect("in-0", 1, "10.0.0.1:51820")
	_, err := c.api.Join(bg, &wire.JoinRequest{})
	wantCode(t, "Join on the ingress server", err, codes.Unimplemented, "")
	_, err = h.dial(nil).hello(wgKey(1), "10.0.0.1:51820")
	wantCode(t, "Hello on the join server", err, codes.Unimplemented, "")
}

// The refusal texts, case by case, on the database clock of the read.
func TestRefusalText(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	ok := store.IngressCertCheck{Found: true, IngressStatus: model.IngressJoined, NotAfter: now.Add(time.Hour), Now: now}
	for _, c := range []struct {
		check store.IngressCertCheck
		want  string
	}{
		{ok, ""},
		{store.IngressCertCheck{Now: now}, "certificate ab is not ingress g's"},
		{func() store.IngressCertCheck { c := ok; c.IngressStatus = model.IngressRemoved; return c }(), "ingress g was removed; join it again"},
		{func() store.IngressCertCheck {
			c := ok
			c.RevokedAt, c.RevokedReason = now.Add(time.Minute), store.RevokedSuperseded
			return c
		}(), ""},
		{func() store.IngressCertCheck {
			c := ok
			c.RevokedAt, c.RevokedReason = now, store.RevokedSuperseded
			return c
		}(), "certificate ab of ingress g was superseded; reconnect with the newest certificate"},
		{func() store.IngressCertCheck {
			c := ok
			c.RevokedAt, c.RevokedReason = now.Add(-time.Minute), store.RevokedRejoined
			return c
		}(), "certificate ab of ingress g was revoked (rejoined); join it again"},
		{func() store.IngressCertCheck { c := ok; c.NotAfter = now; return c }(), "certificate ab of ingress g expired; join it again"},
	} {
		if got := refusalText("g", "ab", c.check); got != c.want {
			t.Errorf("%+v: %q, want %q", c.check, got, c.want)
		}
	}
}
