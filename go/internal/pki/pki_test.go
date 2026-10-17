// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package pki

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

var quiet = slog.New(slog.DiscardHandler)

const (
	testTD     = "feos.test"
	testDomain = "example.test"
)

// newCA makes a CA in a temporary directory and loads its issuer.
func newCA(t *testing.T) (*Issuer, string) {
	t.Helper()
	base := t.TempDir()
	dir := filepath.Join(base, "ca")
	if err := InitCA(InitCAOptions{
		RootDir:     filepath.Join(base, "root"),
		Dir:         dir,
		TrustDomain: testTD,
		DNSDomains:  []string{testDomain},
	}); err != nil {
		t.Fatalf("InitCA: %v", err)
	}
	iss, err := LoadIssuer(dir, testTD)
	if err != nil {
		t.Fatalf("LoadIssuer: %v", err)
	}
	return iss, base
}

func ed25519CSR(t *testing.T, tmpl *x509.CertificateRequest) ([]byte, ed25519.PrivateKey) {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if tmpl == nil {
		tmpl = &x509.CertificateRequest{}
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, tmpl, key)
	if err != nil {
		t.Fatal(err)
	}
	return der, key
}

func csrFor(t *testing.T, key crypto.Signer) []byte {
	t.Helper()
	der, err := CreateCSR(key, "whatever")
	if err != nil {
		t.Fatal(err)
	}
	return der
}

// signRaw signs tmpl with the issuer, bypassing every check of the Issue
// functions: what a leaked issuer key could make.
func signRaw(t *testing.T, iss *Issuer, tmpl *x509.Certificate) *x509.Certificate {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	serial, _ := newSerial()
	tmpl.SerialNumber = serial
	tmpl.NotBefore = time.Now().Add(-time.Minute)
	tmpl.NotAfter = time.Now().Add(time.Hour)
	tmpl.KeyUsage = x509.KeyUsageDigitalSignature
	der, err := x509.CreateCertificate(rand.Reader, tmpl, iss.cert, &key.PublicKey, iss.key)
	if err != nil {
		t.Fatal(err)
	}
	c, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func verifyLeaf(iss *Issuer, leaf *x509.Certificate, usage x509.ExtKeyUsage) error {
	inter := x509.NewCertPool()
	inter.AddCert(iss.cert)
	_, err := leaf.Verify(x509.VerifyOptions{
		Roots:         iss.Bundle(),
		Intermediates: inter,
		KeyUsages:     []x509.ExtKeyUsage{usage},
	})
	return err
}

var oidNameConstraints = asn1.ObjectIdentifier{2, 5, 29, 30}

func nameConstraintsCritical(c *x509.Certificate) bool {
	for _, e := range c.Extensions {
		if e.Id.Equal(oidNameConstraints) {
			return e.Critical
		}
	}
	return false
}

func TestInitCAConstraints(t *testing.T) {
	iss, base := newCA(t)
	root := iss.Root()
	if !root.IsCA || root.MaxPathLen != 1 {
		t.Errorf("root: IsCA %v MaxPathLen %d, want true 1", root.IsCA, root.MaxPathLen)
	}
	if got := root.NotAfter.Sub(root.NotBefore); got < 10*365*24*time.Hour {
		t.Errorf("root lifetime %v, want 10 years", got)
	}
	is := iss.Certificate()
	if !is.IsCA || is.MaxPathLen != 0 || !is.MaxPathLenZero {
		t.Errorf("issuer: IsCA %v MaxPathLen %d MaxPathLenZero %v", is.IsCA, is.MaxPathLen, is.MaxPathLenZero)
	}
	if got := is.NotAfter.Sub(is.NotBefore); got < 5*365*24*time.Hour || got > 6*365*24*time.Hour {
		t.Errorf("issuer lifetime %v, want 5 years", got)
	}
	for _, c := range []*x509.Certificate{root, is} {
		if !nameConstraintsCritical(c) || !c.PermittedDNSDomainsCritical {
			t.Errorf("%s: name constraints not critical", c.Subject.CommonName)
		}
		if len(c.PermittedDNSDomains) != 1 || c.PermittedDNSDomains[0] != testDomain {
			t.Errorf("%s: permitted DNS %v", c.Subject.CommonName, c.PermittedDNSDomains)
		}
		if len(c.PermittedURIDomains) != 1 || c.PermittedURIDomains[0] != testTD {
			t.Errorf("%s: permitted URI %v", c.Subject.CommonName, c.PermittedURIDomains)
		}
		if len(c.ExcludedIPRanges) != 2 {
			t.Errorf("%s: excluded IP ranges %v", c.Subject.CommonName, c.ExcludedIPRanges)
		}
		if _, ok := c.PublicKey.(*ecdsa.PublicKey); !ok {
			t.Errorf("%s: key %T, want ECDSA", c.Subject.CommonName, c.PublicKey)
		}
	}
	if iss.ControlPlaneDomain() != "ferun."+testDomain {
		t.Errorf("control-plane domain %q", iss.ControlPlaneDomain())
	}
	raw, err := os.ReadFile(filepath.Join(base, "ca", IssuerJSONFile))
	if err != nil {
		t.Fatal(err)
	}
	var meta map[string]string
	if err := json.Unmarshal(raw, &meta); err != nil || meta["controlPlaneDomain"] != "ferun."+testDomain {
		t.Errorf("issuer.json = %s (%v)", raw, err)
	}
	for _, f := range []string{filepath.Join(base, "root", RootKeyFile), filepath.Join(base, "ca", IssuerKeyFile)} {
		st, err := os.Stat(f)
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm() != 0o600 {
			t.Errorf("%s mode %v, want 0600", f, st.Mode().Perm())
		}
	}
	if _, err := os.Stat(filepath.Join(base, "ca", RootKeyFile)); err == nil {
		t.Error("root.key in the CA directory")
	}

	// What the constraints refuse, whoever signs it.
	u := func(s string) []*url.URL { p, _ := url.Parse(s); return []*url.URL{p} }
	checks := []struct {
		name string
		tmpl *x509.Certificate
		ok   bool
	}{
		{"DNS inside", &x509.Certificate{DNSNames: []string{"svc." + testDomain}}, true},
		{"URI inside", &x509.Certificate{URIs: u("spiffe://" + testTD + "/x")}, true},
		{"DNS outside", &x509.Certificate{DNSNames: []string{"evil.example.com"}}, false},
		{"IPv4", &x509.Certificate{IPAddresses: []net.IP{net.ParseIP("10.0.0.1")}}, false},
		{"IPv6", &x509.Certificate{IPAddresses: []net.IP{net.ParseIP("2001:db8::1")}}, false},
		{"URI outside", &x509.Certificate{URIs: u("spiffe://other.test/x")}, false},
	}
	for _, c := range checks {
		c.tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
		leaf := signRaw(t, iss, c.tmpl)
		err := verifyLeaf(iss, leaf, x509.ExtKeyUsageServerAuth)
		if (err == nil) != c.ok {
			t.Errorf("%s: verify err %v, want ok %v", c.name, err, c.ok)
		}
	}
}

func TestInitCARefusals(t *testing.T) {
	base := t.TempDir()
	ok := InitCAOptions{RootDir: filepath.Join(base, "r"), Dir: filepath.Join(base, "d"), TrustDomain: testTD, DNSDomains: []string{testDomain}}

	same := ok
	same.Dir = same.RootDir
	if err := InitCA(same); err == nil || !strings.Contains(err.Error(), "must not be the CA directory") {
		t.Errorf("root dir = dir: %v", err)
	}
	// The same directory through a symlink.
	if err := os.MkdirAll(filepath.Join(base, "real"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(base, "real"), filepath.Join(base, "link")); err != nil {
		t.Fatal(err)
	}
	sym := ok
	sym.RootDir, sym.Dir = filepath.Join(base, "real"), filepath.Join(base, "link")
	if err := InitCA(sym); err == nil || !strings.Contains(err.Error(), "must not be the CA directory") {
		t.Errorf("root dir = dir through a symlink: %v", err)
	}

	noDNS := ok
	noDNS.DNSDomains = nil
	if err := InitCA(noDNS); err == nil {
		t.Error("no DNS domain accepted")
	}
	outside := ok
	outside.ControlPlaneDomain = "ferun.other.test"
	if err := InitCA(outside); err == nil {
		t.Error("control-plane domain outside the DNS domains accepted")
	}
	equal := ok
	equal.ControlPlaneDomain = testDomain
	if err := InitCA(equal); err == nil {
		t.Error("control-plane domain equal to the DNS domain accepted")
	}
	for _, td := range []string{"", "Feos.test", "spiffe://feos.test", "feos.test:443", "feos..test", "feos.test.", "-feos.test",
		"feos_test", strings.Repeat("a", 64) + ".test", "10.0.0.1", strings.Repeat("abcdefgh.", 29) + "test"} {
		bad := ok
		bad.TrustDomain = td
		if err := InitCA(bad); err == nil {
			t.Errorf("trust domain %q accepted", td)
		}
	}
	badDNS := ok
	badDNS.DNSDomains = []string{"Example.TEST"}
	if err := InitCA(badDNS); err == nil {
		t.Error("uppercase DNS domain accepted")
	}

	if err := InitCA(ok); err != nil {
		t.Fatalf("InitCA: %v", err)
	}
	before, _ := os.ReadFile(filepath.Join(base, "d", IssuerKeyFile))
	again := ok
	again.RootDir = filepath.Join(base, "r2")
	if err := InitCA(again); err == nil || !strings.Contains(err.Error(), "never overwrites") {
		t.Errorf("second InitCA into the same dir: %v", err)
	}
	after, _ := os.ReadFile(filepath.Join(base, "d", IssuerKeyFile))
	if string(before) != string(after) {
		t.Error("issuer.key was overwritten")
	}
	if _, err := os.Stat(filepath.Join(base, "r2", RootKeyFile)); err == nil {
		t.Error("a refused InitCA wrote a root key")
	}
}

func TestInitCAFailedWriteLeavesNothing(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes into a read-only directory")
	}
	base := t.TempDir()
	o := InitCAOptions{RootDir: filepath.Join(base, "r"), Dir: filepath.Join(base, "d"), TrustDomain: testTD, DNSDomains: []string{testDomain}}
	// The root's files are written, then the issuer's key fails: the CA
	// directory takes no new file.
	if err := os.Mkdir(o.Dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(o.Dir, 0o700) })
	if err := InitCA(o); err == nil {
		t.Fatal("InitCA into a read-only directory succeeded")
	}
	for _, dir := range []string{o.RootDir, o.Dir} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			t.Errorf("a failed InitCA left %s", filepath.Join(dir, e.Name()))
		}
	}
	// A rerun once the directory is writable makes the whole CA.
	if err := os.Chmod(o.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := InitCA(o); err != nil {
		t.Fatalf("InitCA after a failed run: %v", err)
	}
	if _, err := LoadIssuer(o.Dir, testTD); err != nil {
		t.Fatalf("LoadIssuer after a failed run: %v", err)
	}
}

func TestValidateTrustDomain(t *testing.T) {
	for _, td := range []string{"feos.local", "a", "x-1.y2.z", strings.Repeat("a", 63) + ".b"} {
		if err := ValidateTrustDomain(td); err != nil {
			t.Errorf("%q: %v", td, err)
		}
	}
	for _, td := range []string{"", "A.b", "a b", "a/b", "http://a", "a:1", ".a", "a.", "a-.b", "::1"} {
		if err := ValidateTrustDomain(td); err == nil {
			t.Errorf("%q accepted", td)
		}
	}
}

func TestLoadIssuerRefusals(t *testing.T) {
	_, base := newCA(t)
	dir := filepath.Join(base, "ca")
	copyDir := func(t *testing.T) string {
		t.Helper()
		d := t.TempDir()
		for _, f := range []string{IssuerCertFile, IssuerKeyFile, BundleFile, IssuerJSONFile} {
			b, err := os.ReadFile(filepath.Join(dir, f))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(d, f), b, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return d
	}

	t.Run("root key in dir", func(t *testing.T) {
		d := copyDir(t)
		b, _ := os.ReadFile(filepath.Join(base, "root", RootKeyFile))
		os.WriteFile(filepath.Join(d, RootKeyFile), b, 0o600)
		if _, err := LoadIssuer(d, testTD); err == nil || !strings.Contains(err.Error(), "root key") {
			t.Errorf("LoadIssuer: %v", err)
		}
	})
	t.Run("control-plane domain outside", func(t *testing.T) {
		d := copyDir(t)
		os.WriteFile(filepath.Join(d, IssuerJSONFile), []byte(`{"controlPlaneDomain":"ferun.other.test"}`), 0o644)
		if _, err := LoadIssuer(d, testTD); err == nil || !strings.Contains(err.Error(), "outside") {
			t.Errorf("LoadIssuer: %v", err)
		}
	})
	t.Run("control-plane domain equal to the platform domain", func(t *testing.T) {
		d := copyDir(t)
		os.WriteFile(filepath.Join(d, IssuerJSONFile), []byte(`{"controlPlaneDomain":"`+testDomain+`"}`), 0o644)
		if _, err := LoadIssuer(d, testTD); err != nil {
			t.Errorf("LoadIssuer: %v", err)
		}
	})
	t.Run("bundle without the anchoring root", func(t *testing.T) {
		d := copyDir(t)
		_, other := newCA(t)
		b, _ := os.ReadFile(filepath.Join(other, "ca", BundleFile))
		os.WriteFile(filepath.Join(d, BundleFile), b, 0o644)
		if _, err := LoadIssuer(d, testTD); err == nil || !strings.Contains(err.Error(), "does not verify") {
			t.Errorf("LoadIssuer: %v", err)
		}
	})
	t.Run("bundle with two roots", func(t *testing.T) {
		d := copyDir(t)
		_, other := newCA(t)
		b, _ := os.ReadFile(filepath.Join(other, "ca", BundleFile))
		own, _ := os.ReadFile(filepath.Join(d, BundleFile))
		os.WriteFile(filepath.Join(d, BundleFile), append(b, own...), 0o644)
		iss, err := LoadIssuer(d, testTD)
		if err != nil {
			t.Fatalf("LoadIssuer: %v", err)
		}
		if len(iss.BundleDER()) != 2 {
			t.Errorf("bundle has %d roots", len(iss.BundleDER()))
		}
		ownRoot, err := readCerts(filepath.Join(base, "root", RootCertFile))
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(ownRoot[0].RawSubjectPublicKeyInfo)
		if iss.JoinPin() != hex.EncodeToString(sum[:]) {
			t.Error("the pin is not the root anchoring the issuer")
		}
	})
	t.Run("trust domain not permitted", func(t *testing.T) {
		if _, err := LoadIssuer(copyDir(t), "other.test"); err == nil {
			t.Error("LoadIssuer accepted a trust domain outside the URI constraints")
		}
	})
	t.Run("key of another issuer", func(t *testing.T) {
		d := copyDir(t)
		_, other := newCA(t)
		b, _ := os.ReadFile(filepath.Join(other, "ca", IssuerKeyFile))
		os.WriteFile(filepath.Join(d, IssuerKeyFile), b, 0o600)
		if _, err := LoadIssuer(d, testTD); err == nil {
			t.Error("LoadIssuer accepted a key of another issuer")
		}
	})
	t.Run("missing issuer.json", func(t *testing.T) {
		d := copyDir(t)
		os.Remove(filepath.Join(d, IssuerJSONFile))
		if _, err := LoadIssuer(d, testTD); err == nil {
			t.Error("LoadIssuer without issuer.json")
		}
	})
}

func TestJoinPin(t *testing.T) {
	iss, base := newCA(t)
	roots, err := readCerts(filepath.Join(base, "root", RootCertFile))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(roots[0].RawSubjectPublicKeyInfo)
	if iss.JoinPin() != hex.EncodeToString(sum[:]) {
		t.Errorf("pin %s", iss.JoinPin())
	}
	token, sent, hash, err := NewJoinToken(iss.JoinPin())
	if err != nil {
		t.Fatal(err)
	}
	if len(sent) != 53 || !strings.HasPrefix(sent, "feos-join-") {
		t.Errorf("sent %q (%d)", sent, len(sent))
	}
	if token != sent+"."+iss.JoinPin() {
		t.Errorf("token %q", token)
	}
	if hash != HashJoinSecret(sent) {
		t.Error("hash")
	}
	s := sha256.Sum256([]byte(sent))
	if hash != hex.EncodeToString(s[:]) {
		t.Error("hash is not the SHA-256 of the sent part")
	}
	gotSent, gotPin, err := SplitJoinToken(token)
	if err != nil || gotSent != sent || gotPin != iss.JoinPin() {
		t.Errorf("SplitJoinToken = %q %q %v", gotSent, gotPin, err)
	}
	for _, bad := range []string{sent, sent + ".xyz", "feos-join-short." + iss.JoinPin(), "x" + token[1:]} {
		if _, _, err := SplitJoinToken(bad); err == nil {
			t.Errorf("SplitJoinToken(%q) accepted", bad)
		}
	}
	token2, _, _, _ := NewJoinToken(iss.JoinPin())
	if token2 == token {
		t.Error("two tokens are equal")
	}
	if _, _, _, err := NewJoinToken("abc"); err == nil {
		t.Error("NewJoinToken accepted a bad pin")
	}
}

func TestIssueNode(t *testing.T) {
	iss, _ := newCA(t)
	// A CSR with names of its own: all of them are ignored.
	evil, _ := url.Parse(ControlPlaneID(testTD))
	csr, key := ed25519CSR(t, &x509.CertificateRequest{
		Subject:     pkix.Name{CommonName: "ferun"},
		DNSNames:    []string{"ferun." + testDomain, "*." + testDomain},
		IPAddresses: []net.IP{net.ParseIP("10.0.0.1")},
		URIs:        []*url.URL{evil},
	})
	before := time.Now()
	pc, leaf, err := iss.IssueNode(csr, "node-1", 7*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if !leaf.PublicKey.(ed25519.PublicKey).Equal(key.Public()) {
		t.Error("the certificate is not for the CSR's key")
	}
	if len(leaf.DNSNames) != 0 || len(leaf.IPAddresses) != 0 || len(leaf.EmailAddresses) != 0 {
		t.Errorf("node certificate carries names %v %v", leaf.DNSNames, leaf.IPAddresses)
	}
	if len(leaf.URIs) != 1 || leaf.URIs[0].String() != "spiffe://"+testTD+"/node/node-1" {
		t.Errorf("URIs %v", leaf.URIs)
	}
	if leaf.Subject.CommonName != "node-1" {
		t.Errorf("CN %q", leaf.Subject.CommonName)
	}
	if len(leaf.ExtKeyUsage) != 1 || leaf.ExtKeyUsage[0] != x509.ExtKeyUsageClientAuth {
		t.Errorf("EKU %v", leaf.ExtKeyUsage)
	}
	if leaf.KeyUsage != x509.KeyUsageDigitalSignature || leaf.IsCA {
		t.Errorf("KeyUsage %v IsCA %v", leaf.KeyUsage, leaf.IsCA)
	}
	checkBackdated(t, leaf, before, 7*24*time.Hour)
	if leaf.SerialNumber.Sign() <= 0 || leaf.SerialNumber.BitLen() > 128 {
		t.Errorf("serial %v", leaf.SerialNumber)
	}
	if Serial(leaf) != strings.ToLower(leaf.SerialNumber.Text(16)) {
		t.Error("Serial is not lowercase hex")
	}
	if err := verifyLeaf(iss, leaf, x509.ExtKeyUsageClientAuth); err != nil {
		t.Errorf("verify: %v", err)
	}
	if name, err := ParseNodeID(testTD, leaf); err != nil || name != "node-1" {
		t.Errorf("ParseNodeID = %q %v", name, err)
	}
	if pc.SpiffeId != NodeID(testTD, "node-1") || len(pc.Chain) != 2 || len(pc.TrustBundle) != 1 {
		t.Errorf("proto %v", pc)
	}
	if string(pc.Chain[1]) != string(iss.Certificate().Raw) || string(pc.TrustBundle[0]) != string(iss.Root().Raw) {
		t.Error("chain or bundle")
	}
	// Renewed at half the lifetime: a node switched off right before its
	// renewal still has 3.5 days of the 7, a weekend and more.
	life := leaf.NotAfter.Sub(leaf.NotBefore)
	if !pc.RenewAfter.AsTime().Equal(leaf.NotBefore.Add(life / 2)) {
		t.Errorf("renew_after %v", pc.RenewAfter.AsTime())
	}
	if left := leaf.NotAfter.Sub(pc.RenewAfter.AsTime()); left < 84*time.Hour {
		t.Errorf("%v left at renew_after; a weekend off (62 h) must not outlive the certificate", left)
	}

	// ECDSA P-256 is accepted for nodes, other keys are not.
	p256, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if _, _, err := iss.IssueNode(csrFor(t, p256), "node-2", time.Hour); err != nil {
		t.Errorf("P-256: %v", err)
	}
	p384, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if _, _, err := iss.IssueNode(csrFor(t, p384), "node-2", time.Hour); err == nil {
		t.Error("P-384 accepted")
	}
	rsaKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	if _, _, err := iss.IssueNode(csrFor(t, rsaKey), "node-2", time.Hour); err == nil {
		t.Error("RSA accepted")
	}
	for _, name := range []string{"", "Node", "a.b", "-a", strings.Repeat("a", 64), "a_b"} {
		if _, _, err := iss.IssueNode(csr, name, time.Hour); err == nil {
			t.Errorf("node name %q accepted", name)
		}
	}
}

func checkBackdated(t *testing.T, leaf *x509.Certificate, before time.Time, ttl time.Duration) {
	t.Helper()
	nb := before.Add(-10 * time.Minute)
	if leaf.NotBefore.Before(nb.Add(-2*time.Second)) || leaf.NotBefore.After(time.Now().Add(-10*time.Minute+time.Second)) {
		t.Errorf("NotBefore %v, want about %v", leaf.NotBefore, nb)
	}
	if d := leaf.NotAfter.Sub(before.Add(ttl)); d < -2*time.Second || d > 2*time.Second {
		t.Errorf("NotAfter %v, want about %v", leaf.NotAfter, before.Add(ttl))
	}
}

func TestIssuePod(t *testing.T) {
	iss, _ := newCA(t)
	id := SandboxID(testTD, "acme", uuid.MustParse("3f2c0000-0000-4000-8000-000000000001"))
	if id != "spiffe://"+testTD+"/tenant/acme/sandbox/3f2c0000-0000-4000-8000-000000000001" {
		t.Errorf("SandboxID = %s", id)
	}
	csr, _ := ed25519CSR(t, &x509.CertificateRequest{DNSNames: []string{"ferun." + testDomain}})
	before := time.Now()
	pc, leaf, err := iss.IssuePod(csr, id, []string{"svc.acme." + testDomain}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(leaf.DNSNames) != 1 || leaf.DNSNames[0] != "svc.acme."+testDomain {
		t.Errorf("DNS %v", leaf.DNSNames)
	}
	if len(leaf.URIs) != 1 || leaf.URIs[0].String() != id || pc.SpiffeId != id {
		t.Errorf("URIs %v", leaf.URIs)
	}
	if len(leaf.ExtKeyUsage) != 2 {
		t.Errorf("EKU %v", leaf.ExtKeyUsage)
	}
	// 66 bytes: truncated to 47, "-" and 16 hex digits of the ID's hash.
	sum := sha256.Sum256([]byte(id))
	if want := (testTD + "/tenant/acme/sandbox/3f2c0000-0000-4000-8000-000000000001")[:47] + "-" + hex.EncodeToString(sum[:8]); leaf.Subject.CommonName != want {
		t.Errorf("CN %q, want %q", leaf.Subject.CommonName, want)
	}
	short := SandboxID(testTD, "a", uuid.MustParse("3f2c0000-0000-4000-8000-000000000001"))
	if cn := podCommonName(short); cn != testTD+"/tenant/a/sandbox/3f2c0000-0000-4000-8000-000000000001" {
		t.Errorf("short CN %q", cn)
	}
	checkBackdated(t, leaf, before, time.Hour)
	if life := leaf.NotAfter.Sub(leaf.NotBefore); !pc.RenewAfter.AsTime().Equal(leaf.NotBefore.Add(life / 4 * 3)) {
		t.Errorf("renew_after %v, want three quarters of the lifetime", pc.RenewAfter.AsTime())
	}
	for _, u := range []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth} {
		if err := verifyLeaf(iss, leaf, u); err != nil {
			t.Errorf("verify %v: %v", u, err)
		}
	}
	// A pod's certificate never passes as a node's.
	if _, err := ParseNodeID(testTD, leaf); err == nil {
		t.Error("ParseNodeID accepted a pod certificate")
	}

	refused := []struct {
		name  string
		id    string
		names []string
	}{
		{"control-plane ID", ControlPlaneID(testTD), nil},
		{"under the control-plane ID", ControlPlaneID(testTD) + "/x", nil},
		{"node ID", NodeID(testTD, "node-1"), nil},
		{"other trust domain", "spiffe://other.test/tenant/a/sandbox/b", nil},
		{"no path", "spiffe://" + testTD, nil},
		{"dot segment", "spiffe://" + testTD + "/a/../ferun", nil},
		{"query", "spiffe://" + testTD + "/a?b", nil},
		{"not spiffe", "https://" + testTD + "/a", nil},
		{"control-plane domain", id, []string{"ferun." + testDomain}},
		{"under the control-plane domain", id, []string{"x.ferun." + testDomain}},
		{"wildcard", id, []string{"*." + testDomain}},
		{"wildcard elsewhere", id, []string{"*.acme." + testDomain}},
		{"IP", id, []string{"10.0.0.1"}},
		{"outside the platform", id, []string{"svc.example.com"}},
	}
	for _, c := range refused {
		if _, _, err := iss.IssuePod(csr, c.id, c.names, time.Hour); err == nil {
			t.Errorf("%s: issued", c.name)
		}
	}
	// Pods get Ed25519 keys only.
	p256, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if _, _, err := iss.IssuePod(csrFor(t, p256), id, nil, time.Hour); err == nil {
		t.Error("ECDSA pod key accepted")
	}

	long := SandboxID(testTD, strings.Repeat("t", 60), uuid.New())
	_, leaf, err = iss.IssuePod(csr, long, nil, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if cn := leaf.Subject.CommonName; len(cn) != 64 || !strings.HasPrefix(cn, testTD+"/tenant/") {
		t.Errorf("long CN %q (%d)", cn, len(cn))
	}
	if podCommonName(long) == podCommonName(long+"x") {
		t.Error("truncated CNs collide")
	}
}

func TestIssueServer(t *testing.T) {
	iss, _ := newCA(t)
	name := "ferun." + testDomain
	before := time.Now()
	c, err := iss.IssueServer([]string{name, "n1.ferun." + testDomain}, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	leaf := c.Leaf
	if len(c.Certificate) != 3 || string(c.Certificate[1]) != string(iss.Certificate().Raw) || string(c.Certificate[2]) != string(iss.Root().Raw) {
		t.Error("chain is not leaf, issuer, root")
	}
	if len(leaf.URIs) != 1 || leaf.URIs[0].String() != ControlPlaneID(testTD) {
		t.Errorf("URIs %v", leaf.URIs)
	}
	if len(leaf.IPAddresses) != 0 || len(leaf.DNSNames) != 2 {
		t.Errorf("names %v %v", leaf.DNSNames, leaf.IPAddresses)
	}
	if len(leaf.ExtKeyUsage) != 1 || leaf.ExtKeyUsage[0] != x509.ExtKeyUsageServerAuth {
		t.Errorf("EKU %v", leaf.ExtKeyUsage)
	}
	if _, ok := leaf.PublicKey.(*ecdsa.PublicKey); !ok {
		t.Errorf("key %T", leaf.PublicKey)
	}
	checkBackdated(t, leaf, before, 24*time.Hour)
	if err := VerifyControlPlane(iss.Bundle(), name, testTD, c.Certificate); err != nil {
		t.Errorf("VerifyControlPlane: %v", err)
	}
	for _, bad := range [][]string{nil, {"x." + testDomain}, {"*.ferun." + testDomain}, {"10.0.0.1"}, {testDomain}, {"ferun.other.test"}} {
		if _, err := iss.IssueServer(bad, time.Hour); err == nil {
			t.Errorf("IssueServer(%v) accepted", bad)
		}
	}

	// Only IssueServer writes the control plane's URI.
	csr, _ := ed25519CSR(t, nil)
	_, n, _ := iss.IssueNode(csr, "ferun", time.Hour)
	_, p, _ := iss.IssuePod(csr, SandboxID(testTD, "ferun", uuid.New()), nil, time.Hour)
	for _, c := range []*x509.Certificate{n, p} {
		for _, u := range c.URIs {
			if u.String() == ControlPlaneID(testTD) {
				t.Errorf("%s carries the control plane's URI", c.Subject.CommonName)
			}
		}
	}
}

func TestCheckCSR(t *testing.T) {
	csr, key := ed25519CSR(t, nil)
	_, hash, err := CheckCSR(csr, false)
	if err != nil {
		t.Fatal(err)
	}
	spki, _ := x509.MarshalPKIXPublicKey(key.Public())
	if hash != spkiHash(spki) {
		t.Error("key hash")
	}
	if _, _, err := CheckCSR(nil, true); err == nil {
		t.Error("empty CSR accepted")
	}
	if _, _, err := CheckCSR(make([]byte, MaxCSRBytes+1), true); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("oversized CSR: %v", err)
	}
	if _, _, err := CheckCSR([]byte("garbage"), true); err == nil {
		t.Error("garbage accepted")
	}
	// A broken signature: flip a byte of the signature at the end.
	bad := append([]byte(nil), csr...)
	bad[len(bad)-1] ^= 0xff
	if _, _, err := CheckCSR(bad, true); err == nil {
		t.Error("CSR with a bad signature accepted")
	}
	p256, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if _, _, err := CheckCSR(csrFor(t, p256), true); err != nil {
		t.Errorf("P-256 with ECDSA allowed: %v", err)
	}
	if _, _, err := CheckCSR(csrFor(t, p256), false); err == nil {
		t.Error("P-256 accepted for Ed25519 only")
	}
}

func TestVerifyServer(t *testing.T) {
	iss, _ := newCA(t)
	other, _ := newCA(t)
	name := "ferun." + testDomain
	good, err := iss.IssueServer([]string{name}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := other.IssueServer([]string{name}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	pin := iss.JoinPin()

	if err := VerifyJoinServer(pin, name, testTD, good.Certificate); err != nil {
		t.Errorf("good chain: %v", err)
	}
	if err := VerifyJoinServer(strings.ToUpper(pin), name, testTD, good.Certificate); err != nil {
		t.Errorf("uppercase pin: %v", err)
	}
	if err := VerifyJoinServer(pin, name, testTD, foreign.Certificate); err == nil || !strings.Contains(err.Error(), "pin") {
		t.Errorf("another CA's chain: %v", err)
	}
	if err := VerifyJoinServer(pin, name, testTD, good.Certificate[:2]); err == nil {
		t.Error("chain without the root accepted")
	}
	if err := VerifyJoinServer(pin, "other."+testDomain, testTD, good.Certificate); err == nil {
		t.Error("wrong server name accepted")
	}
	if err := VerifyJoinServer(pin, name, "other.test", good.Certificate); err == nil {
		t.Error("wrong trust domain accepted")
	}
	// The pin of the issuer, not the root, matches no CA root here: the
	// issuer is a CA, so pinning it would verify; a node pins the root, and
	// a leaf's own key never stands in for a root.
	leafPin := spkiHash(good.Leaf.RawSubjectPublicKeyInfo)
	if err := VerifyJoinServer(leafPin, name, testTD, good.Certificate); err == nil {
		t.Error("the leaf's own key accepted as the pin")
	}

	// A pod certificate of the same issuer, with a DNS name under the
	// platform domain and ServerAuth, verifies as a chain but is not the
	// control plane.
	csr, _ := ed25519CSR(t, nil)
	podName := "svc." + testDomain
	_, podLeaf, err := iss.IssuePod(csr, SandboxID(testTD, "acme", uuid.New()), []string{podName}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	podChain := [][]byte{podLeaf.Raw, iss.Certificate().Raw, iss.Root().Raw}
	if err := VerifyJoinServer(pin, podName, testTD, podChain); err == nil || !strings.Contains(err.Error(), "not the control plane") {
		t.Errorf("pod certificate at join: %v", err)
	}
	if err := VerifyControlPlane(iss.Bundle(), podName, testTD, podChain); err == nil || !strings.Contains(err.Error(), "not the control plane") {
		t.Errorf("pod certificate after join: %v", err)
	}

	if err := CheckBundlePin(iss.BundleDER(), pin); err != nil {
		t.Errorf("CheckBundlePin: %v", err)
	}
	if err := CheckBundlePin(other.BundleDER(), pin); err == nil {
		t.Error("CheckBundlePin accepted a bundle without the pinned root")
	}
	if err := VerifyControlPlane(iss.Bundle(), name, testTD, good.Certificate); err != nil {
		t.Errorf("VerifyControlPlane: %v", err)
	}
	if err := VerifyControlPlane(iss.Bundle(), name, testTD, good.Certificate[:2]); err != nil {
		t.Errorf("VerifyControlPlane without the root in the chain: %v", err)
	}
	if err := VerifyControlPlane(iss.Bundle(), name, testTD, foreign.Certificate); err == nil {
		t.Error("VerifyControlPlane accepted another CA")
	}
	if err := VerifyControlPlane(iss.Bundle(), name, testTD, nil); err == nil {
		t.Error("VerifyControlPlane accepted no certificate")
	}
}

func TestServerCertSourceRotation(t *testing.T) {
	iss, _ := newCA(t)
	src, err := NewServerCertSource(iss, []string{"ferun." + testDomain}, 2*time.Second, quiet)
	if err != nil {
		t.Fatal(err)
	}
	first, _ := src.GetCertificate(nil)
	ctx := t.Context()
	go src.Run(ctx)
	deadline := time.Now().Add(5 * time.Second)
	for {
		c, _ := src.GetCertificate(nil)
		if c.Leaf.SerialNumber.Cmp(first.Leaf.SerialNumber) != 0 {
			if time.Since(time.Unix(0, src.issued.Load())) > time.Second {
				t.Error("issued time not updated")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the certificate was not renewed")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, err := NewServerCertSource(iss, []string{"x.other.test"}, time.Hour, nil); err == nil {
		t.Error("a name outside the control-plane domain accepted")
	}
}

func TestServerCertSourceRetry(t *testing.T) {
	iss, _ := newCA(t)
	src, err := NewServerCertSource(iss, []string{"ferun." + testDomain}, 400*time.Millisecond, quiet)
	if err != nil {
		t.Fatal(err)
	}
	first, _ := src.GetCertificate(nil)
	failures := make(chan struct{}, 100)
	fail := true
	src.retry = 100 * time.Millisecond
	real := src.issue
	src.issue = func(names []string, ttl time.Duration) (*tls.Certificate, error) {
		if fail {
			failures <- struct{}{}
			return nil, os.ErrPermission
		}
		return real(names, ttl)
	}
	go src.Run(t.Context())
	for range 3 {
		select {
		case <-failures:
		case <-time.After(3 * time.Second):
			t.Fatal("no retry")
		}
	}
	c, _ := src.GetCertificate(nil)
	if c != first {
		t.Error("a failed renewal replaced the certificate")
	}
}
