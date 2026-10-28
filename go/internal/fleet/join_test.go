// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package fleet

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"

	fleetv1 "github.com/gardenlinux/feos/go/gen/feos/fleet/v1"
	"github.com/gardenlinux/feos/go/internal/apierr"
	"github.com/gardenlinux/feos/go/internal/db"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/pki"
	"github.com/gardenlinux/feos/go/internal/store"
)

func TestIssueJoinToken(t *testing.T) {
	h := newHarness(t)
	id, token, expires, err := h.s.IssueJoinToken(bg, "alice", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	sent, pin, err := pki.SplitJoinToken(token)
	if err != nil || pin != h.iss.JoinPin() || len(sent) != pki.JoinSecretLen {
		t.Fatalf("token %q: %v", token, err)
	}
	if d := time.Until(expires); d < 59*time.Minute || d > 61*time.Minute {
		t.Fatalf("expires in %v; want the default hour", d)
	}
	tokens, err := h.st.ListJoinTokens(bg)
	if err != nil || len(tokens) != 1 {
		t.Fatalf("tokens %+v, %v", tokens, err)
	}
	if tk := tokens[0]; tk.ID != id || tk.CreatedBy != "alice" || tk.NodeName != "" || tk.Hash != pki.HashJoinSecret(sent) {
		t.Fatalf("stored token %+v", tk)
	}
	if strings.Contains(tokens[0].Hash, sent) {
		t.Fatal("the secret is stored")
	}
	_, _, _, err = h.s.IssueJoinToken(bg, "alice", "Not_A_Name", time.Hour)
	var ae *apierr.Error
	if !errors.As(err, &ae) || ae.Status != http.StatusBadRequest {
		t.Fatalf("invalid node name: %v", err)
	}
	if _, _, _, err := h.s.IssueJoinToken(bg, "alice", "n7", time.Minute); err != nil {
		t.Fatal(err)
	}
}

func TestJoinUnpinned(t *testing.T) {
	h := newHarness(t)
	if err := h.st.UpsertInstance(bg, store.InstanceInfo{ID: "i1", NodeAddress: "10.0.0.1:7443"}); err != nil {
		t.Fatal(err)
	}
	sent := h.token("")
	csr := newCSR(t)
	resp, err := h.join("n1", sent, csr)
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetName() != "n1" || resp.GetCertificate().GetSpiffeId() != pki.NodeID(testTD, "n1") ||
		len(resp.GetCertificate().GetChain()) != 2 || len(resp.GetCertificate().GetTrustBundle()) != 1 {
		t.Fatalf("response %v", resp)
	}
	if len(resp.GetInstances()) != 1 || resp.GetInstances()[0].GetId() != "i1" || resp.GetInstances()[0].GetNodeAddress() != "10.0.0.1:7443" {
		t.Fatalf("instances %v", resp.GetInstances())
	}
	leaf, err := x509.ParseCertificate(resp.GetCertificate().GetChain()[0])
	if err != nil {
		t.Fatal(err)
	}
	if got := time.Until(leaf.NotAfter); got < 167*time.Hour || got > 169*time.Hour {
		t.Fatalf("node certificate lifetime %v; want the default 168h", got)
	}
	if n := h.node("n1"); n.Status != model.NodeJoined {
		t.Fatalf("node %+v", n)
	}
	serial := pki.Serial(leaf)
	c, err := h.st.GetNodeCert(bg, serial)
	if err != nil {
		t.Fatal(err)
	}
	_, keyHash, _ := pki.CheckCSR(csr, true)
	tokens, _ := h.st.ListJoinTokens(bg)
	if c.Kind != store.CertJoin || c.KeyHash != keyHash || c.TokenID != tokens[0].ID || c.Peer != joinPeer || c.Node != "n1" {
		t.Fatalf("node cert %+v", c)
	}
	tk := tokens[0]
	if !tk.Used() || tk.UsedName != "n1" || tk.UsedSerial != serial || tk.UsedKeyHash != keyHash || tk.UsedPeer != joinPeer {
		t.Fatalf("token after the join %+v", tk)
	}
	var certRows int
	h.scalar(&certRows, "SELECT count(*) FROM activity WHERE node = 'n1' AND kind = 'node_cert' AND sandbox_id IS NULL")
	if certRows != 1 {
		t.Fatalf("%d node_cert activity rows", certRows)
	}

	// The same token and key again within ten minutes: the first answer
	// was lost, so it is answered again, for the same name, revoking
	// nothing.
	again, err := h.join("other-name", sent, csr)
	if err != nil {
		t.Fatalf("repeated join: %v", err)
	}
	if again.GetName() != "n1" || serialOf(t, again.GetCertificate()) == serial {
		t.Fatalf("repeated join answered %v", again)
	}
	c2, _ := h.st.GetNodeCert(bg, serialOf(t, again.GetCertificate()))
	if c2.Kind != store.CertJoin || c2.KeyHash != keyHash || c2.TokenID != tk.ID {
		t.Fatalf("repeated node cert %+v", c2)
	}
	if first, _ := h.st.GetNodeCert(bg, serial); !first.RevokedAt.IsZero() {
		t.Fatalf("the first certificate was revoked %+v", first)
	}
	if tokens, _ := h.st.ListJoinTokens(bg); tokens[0].UsedSerial != serial {
		t.Fatalf("the token's first use changed: %+v", tokens[0])
	}

	// Another key: the token is used.
	_, err = h.join("n1", sent, newCSR(t))
	wantCode(t, err, codes.PermissionDenied, "join token already used")
	// The same key, but after ten minutes: used.
	h.exec("UPDATE join_tokens SET used_at = now() - interval '11 minutes'")
	_, err = h.join("n1", sent, csr)
	wantCode(t, err, codes.PermissionDenied, "join token already used")

	// An unpinned token never takes over an existing name, whatever the
	// row's status; the token stays unused.
	sent2 := h.token("")
	h.exec("UPDATE nodes SET status = 'removed', removed_at = now() WHERE name = 'n1'")
	_, err = h.join("n1", sent2, newCSR(t))
	wantCode(t, err, codes.AlreadyExists, "node n1 exists; join it with a token pinned to its name")
	tokens, _ = h.st.ListJoinTokens(bg)
	for _, tk := range tokens {
		if tk.Hash == pki.HashJoinSecret(sent2) && tk.Used() {
			t.Fatal("a refused join used the token")
		}
	}
	// The same token joins a new name.
	if resp, err := h.join("n2", sent2, newCSR(t)); err != nil || resp.GetName() != "n2" {
		t.Fatalf("join n2: %v %v", resp, err)
	}
}

func TestJoinRefusals(t *testing.T) {
	h := newHarness(t)
	csr := newCSR(t)
	_, err := h.join("n1", pki.JoinTokenPrefix+strings.Repeat("A", 43), csr)
	wantCode(t, err, codes.PermissionDenied, "unknown join token")

	sent := h.token("")
	h.exec("UPDATE join_tokens SET expires_at = now() - interval '1 second'")
	_, err = h.join("n1", sent, csr)
	wantCode(t, err, codes.PermissionDenied, "join token expired")

	sent = h.token("")
	for _, tc := range []struct {
		token, name string
		csr         []byte
	}{
		{"feos-joi-" + strings.Repeat("A", 44), "n1", csr},
		{sent + ".pin", "n1", csr},
		{sent[:len(sent)-1], "n1", csr},
		{sent, "N1", csr},
		{sent, "-n1", csr},
		{sent, "n1-", csr},
		{sent, "", csr},
		{sent, strings.Repeat("a", 64), csr},
		{sent, "n1.example", csr},
		{sent, "n1", nil},
		{sent, "n1", make([]byte, pki.MaxCSRBytes+1)},
	} {
		_, err := h.join(tc.name, tc.token, tc.csr)
		wantCode(t, err, codes.InvalidArgument, "")
	}
	// A signing request that does not parse, with a valid token: refused,
	// and the token stays unused.
	_, err = h.join("n1", sent, []byte("not a csr"))
	wantCode(t, err, codes.InvalidArgument, "certificate signing request")
	// An RSA or P-384 key is refused; P-256 is a node's other key type.
	p384, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	bad, _ := pki.CreateCSR(p384, "n1")
	_, err = h.join("n1", sent, bad)
	wantCode(t, err, codes.InvalidArgument, "P-256")
	p256, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	good, _ := pki.CreateCSR(p256, "n1")
	if resp, err := h.join("n1", sent, good); err != nil || resp.GetName() != "n1" {
		t.Fatalf("join with a P-256 key: %v %v", resp, err)
	}
}

// A join token of kind ingress never joins a node, pinned or not, and
// stays unused for the ingress it is for.
func TestJoinIngressToken(t *testing.T) {
	h := newHarness(t)
	for _, pinned := range []string{"", "n1"} {
		_, sent, hash, err := pki.NewJoinToken(h.iss.JoinPin())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := h.st.CreateJoinToken(bg, hash, "test", model.JoinKindIngress, pinned, time.Hour); err != nil {
			t.Fatal(err)
		}
		_, err = h.join("n1", sent, newCSR(t))
		wantCode(t, err, codes.PermissionDenied, "this join token is for an ingress")
		tok, err := h.st.LockJoinToken(bg, hash)
		if err != nil || tok.Used() {
			t.Fatalf("the ingress token after a node's join: %+v %v", tok, err)
		}
	}
	// An ingress's token in its own form is refused by that form, before
	// any lookup, as the form check of a ferund that knows no kinds does
	// (prefix feos-join-, 53 characters).
	for _, pinned := range []string{"", "n1"} {
		_, sent, hash, err := pki.NewIngressJoinToken(h.iss.JoinPin())
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(sent, "feos-join-") || len(sent) == 53 {
			t.Fatalf("an ingress's token %q in a node's form", sent)
		}
		if _, err := h.st.CreateJoinToken(bg, hash, "test", model.JoinKindIngress, pinned, time.Hour); err != nil {
			t.Fatal(err)
		}
		_, err = h.join("n1", sent, newCSR(t))
		wantCode(t, err, codes.InvalidArgument, "this join token is for an ingress")
		tok, err := h.st.LockJoinToken(bg, hash)
		if err != nil || tok.Used() {
			t.Fatalf("the ingress token after a node's join: %+v %v", tok, err)
		}
	}
	if _, err := h.st.GetNode(bg, "n1"); !errors.Is(err, store.ErrNoRow) {
		t.Fatalf("an ingress token made a node: %v", err)
	}
}

func TestJoinPinned(t *testing.T) {
	h := newHarness(t)
	old := h.joined("n1")
	if n := h.node("n1"); n.CurrentSerial != old {
		t.Fatalf("current serial %q; want %q", n.CurrentSerial, old)
	}
	// The node is removed and forked meanwhile; a pinned token joins the
	// name again, whatever name the request proposes.
	h.exec(`UPDATE nodes SET status = 'removed', removed_at = now(), identity_forked_at = now(),
		status_description = 'gone', draining = true WHERE name = 'n1'`)
	ch, unsubscribe := h.d.Subscribe(db.ChanNode)
	defer unsubscribe()
	sent := h.token("n1")
	resp, err := h.join("proposed", sent, newCSR(t))
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetName() != "n1" {
		t.Fatalf("pinned join named %q", resp.GetName())
	}
	expectPayload(t, ch, "n1")
	n := h.node("n1")
	if n.Status != model.NodeJoined || !n.RemovedAt.IsZero() || n.Forked() || n.CurrentSerial != "" ||
		n.StatusDescription != "" || !n.Draining {
		t.Fatalf("node after the pinned join %+v", n)
	}
	if c, _ := h.st.GetNodeCert(bg, old); c.RevokedReason != store.RevokedRejoined || c.RevokedAt.IsZero() {
		t.Fatalf("old certificate %+v", c)
	}
	newSerial := serialOf(t, resp.GetCertificate())
	if c, _ := h.st.GetNodeCert(bg, newSerial); c.Kind != store.CertRejoin || !c.RevokedAt.IsZero() {
		t.Fatalf("new certificate %+v", c)
	}
	if _, err := h.s.CheckNode(bg, "n1", old, "x", true); !errors.Is(err, model.ErrNodeRefused) {
		t.Fatalf("old certificate after the re-join: %v", err)
	}
	if _, err := h.s.CheckNode(bg, "n1", newSerial, "x", true); err != nil {
		t.Fatalf("new certificate: %v", err)
	}

	// A pinned token for a name that does not exist yet makes it.
	resp, err = h.join("whatever", h.token("fresh"), newCSR(t))
	if err != nil || resp.GetName() != "fresh" {
		t.Fatalf("pinned join of a new name: %v %v", resp, err)
	}
	if c, _ := h.st.GetNodeCert(bg, serialOf(t, resp.GetCertificate())); c.Kind != store.CertRejoin {
		t.Fatalf("certificate kind %q", c.Kind)
	}
}

func TestJoinPinnedRepeat(t *testing.T) {
	h := newHarness(t)
	h.joined("n1")
	sent := h.token("n1")
	csr := newCSR(t)
	first, err := h.join("n1", sent, csr)
	if err != nil {
		t.Fatal(err)
	}
	again, err := h.join("n1", sent, csr)
	if err != nil {
		t.Fatalf("repeated pinned join: %v", err)
	}
	if c, _ := h.st.GetNodeCert(bg, serialOf(t, again.GetCertificate())); c.Kind != store.CertRejoin {
		t.Fatalf("repeat kind %q; want the first answer's", c.Kind)
	}
	if c, _ := h.st.GetNodeCert(bg, serialOf(t, first.GetCertificate())); !c.RevokedAt.IsZero() {
		t.Fatalf("the repeat revoked the first answer: %+v", c)
	}
}

// TestJoinRepeatOvertaken covers a used token's repeat within ten minutes
// after its first answer was overtaken: by a pinned re-join of the name
// (the operator evicted the key), a removal, a fork, or the presentation of
// a newer certificate. None is answered again.
func TestJoinRepeatOvertaken(t *testing.T) {
	h := newHarness(t)
	certCount := func() int { return h.count("SELECT count(*) FROM node_certs") }

	// A pinned re-join revokes the first answer's certificate.
	sent, csr := h.token(""), newCSR(t)
	if _, err := h.join("n1", sent, csr); err != nil {
		t.Fatal(err)
	}
	if _, err := h.join("n1", h.token("n1"), newCSR(t)); err != nil {
		t.Fatal(err)
	}
	before := certCount()
	_, err := h.join("n1", sent, csr)
	wantCode(t, err, codes.PermissionDenied, "join token already used")
	if after := certCount(); after != before {
		t.Fatalf("%d certificates issued by a refused repeat", after-before)
	}

	// A removed node.
	sent, csr = h.token(""), newCSR(t)
	if _, err := h.join("n2", sent, csr); err != nil {
		t.Fatal(err)
	}
	h.exec("UPDATE nodes SET status = 'removed', removed_at = now() WHERE name = 'n2'")
	_, err = h.join("n2", sent, csr)
	wantCode(t, err, codes.PermissionDenied, "join token already used")

	// A forked node.
	sent, csr = h.token(""), newCSR(t)
	if _, err := h.join("n3", sent, csr); err != nil {
		t.Fatal(err)
	}
	h.exec("UPDATE nodes SET identity_forked_at = now() WHERE name = 'n3'")
	_, err = h.join("n3", sent, csr)
	wantCode(t, err, codes.PermissionDenied, "join token already used")

	// The first answer arrived: the node renewed and presented the newer
	// certificate, which supersedes the first.
	sent, csr = h.token(""), newCSR(t)
	resp, err := h.join("n4", sent, csr)
	if err != nil {
		t.Fatal(err)
	}
	first := serialOf(t, resp.GetCertificate())
	h.s.OnPresented("n4", first, "x")
	renewed, err := h.s.UplinkServer().RenewCertificate(uplinkCtx("n4", first), &fleetv1.RenewCertificateRequest{Csr: newCSR(t)})
	if err != nil {
		t.Fatal(err)
	}
	h.s.OnPresented("n4", serialOf(t, renewed.GetCertificate()), "x")
	before = certCount()
	_, err = h.join("n4", sent, csr)
	wantCode(t, err, codes.PermissionDenied, "join token already used")
	if after := certCount(); after != before {
		t.Fatalf("%d certificates issued by refused repeats", after-before)
	}
}

func TestJoinLimits(t *testing.T) {
	h := newHarness(t)
	now := time.Now()
	h.s.joins.nowForTesting = func() time.Time { return now }
	csr := newCSR(t)
	unknown := pki.JoinTokenPrefix + strings.Repeat("B", 43)
	// The bucket holds a burst of 20: then the joins of this instance are
	// refused until it refills at 10 per second.
	for range 20 {
		_, err := h.join("n1", unknown, csr)
		wantCode(t, err, codes.PermissionDenied, "unknown join token")
	}
	for range 3 {
		_, err := h.join("n1", unknown, csr)
		wantCode(t, err, codes.ResourceExhausted, "too many joins")
	}
	now = now.Add(time.Second)
	for range 10 {
		_, err := h.join("n1", unknown, csr)
		wantCode(t, err, codes.PermissionDenied, "unknown join token")
	}
	_, err := h.join("n1", unknown, csr)
	wantCode(t, err, codes.ResourceExhausted, "too many joins")
	// Malformed requests are refused before the limits.
	_, err = h.join("N1", unknown, csr)
	wantCode(t, err, codes.InvalidArgument, "")

	// Four database slots, waited for at most the slot wait.
	now = now.Add(time.Hour)
	h.s.joinSlotWait = 200 * time.Millisecond
	for range 4 {
		h.s.joinSlots <- struct{}{}
	}
	began := time.Now()
	_, err = h.join("n1", unknown, csr)
	wantCode(t, err, codes.ResourceExhausted, "too many joins")
	if waited := time.Since(began); waited < 150*time.Millisecond {
		t.Fatalf("refused after %v; want the slot wait", waited)
	}
	<-h.s.joinSlots
	_, err = h.join("n1", unknown, csr)
	wantCode(t, err, codes.PermissionDenied, "unknown join token")
}

func TestJoinOnTheUplinkServer(t *testing.T) {
	h := newHarness(t)
	_, err := h.s.UplinkServer().Join(joinCtx(), nil)
	wantCode(t, err, codes.Unimplemented, "")
	_, err = h.s.JoinServer().ReportTelemetry(uplinkCtx("n1", "aa"), nil)
	wantCode(t, err, codes.Unimplemented, "")
	_, err = h.s.JoinServer().RenewCertificate(uplinkCtx("n1", "aa"), nil)
	wantCode(t, err, codes.Unimplemented, "")
	_, err = h.s.JoinServer().IssuePodCredentials(uplinkCtx("n1", "aa"), nil)
	wantCode(t, err, codes.Unimplemented, "")
}
