// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package pki

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"time"
)

// The files of the CA. The root's key lives only in the root directory,
// which stays offline; an instance's CA directory holds the issuer, the
// bundle of roots and issuer.json.
const (
	RootCertFile   = "root.crt"
	RootKeyFile    = "root.key"
	IssuerCertFile = "issuer.crt"
	IssuerKeyFile  = "issuer.key"
	BundleFile     = "bundle.crt"
	IssuerJSONFile = "issuer.json"
)

const (
	rootLifetime   = 10 * 365 * 24 * time.Hour
	issuerLifetime = 5 * 365 * 24 * time.Hour
)

// InitCAOptions are the inputs of `ferund init-ca` and `ferund new-issuer`.
type InitCAOptions struct {
	// RootDir receives root.crt and root.key (InitCA), or holds them
	// (NewIssuer); it must not be Dir.
	RootDir string
	// Dir receives issuer.crt, issuer.key, bundle.crt and issuer.json: what
	// every instance holds in --ca-dir.
	Dir         string
	TrustDomain string
	// ControlPlaneDomain is the domain of the instances' names on the nodes'
	// port, recorded in issuer.json; it must lie under one of DNSDomains.
	// Empty: ferun.<DNSDomains[0]>.
	ControlPlaneDomain string
	// DNSDomains are the platform's DNS domains, the permitted DNS name
	// constraints of the root and the issuer; at least one.
	DNSDomains []string
}

// issuerJSON is the content of issuer.json.
type issuerJSON struct {
	ControlPlaneDomain string `json:"controlPlaneDomain"`
	// TrustDomain binds the issuer to its trust domain, which a URI name
	// constraint did before ADR 0024; an issuer.json of then has none.
	TrustDomain string `json:"trustDomain,omitempty"`
}

// InitCA makes the platform CA: the root (ECDSA P-256, 10 years, path
// length 1) in RootDir and the issuer (ECDSA P-256, 5 years, path length 0)
// in Dir, both with critical name constraints that permit the DNS domains
// and exclude every IP address. Neither constrains URIs (ADR 0024):
// rustls-webpki, which feosd checks the control plane with, refuses every
// URI name beneath a URI constraint. It refuses to overwrite any file and
// refuses RootDir equal to Dir.
func InitCA(o InitCAOptions) error {
	cp, err := o.check()
	if err != nil {
		return err
	}
	rootDir, dir, err := separateDirs(o.RootDir, o.Dir, true)
	if err != nil {
		return err
	}
	files := []string{
		filepath.Join(rootDir, RootCertFile), filepath.Join(rootDir, RootKeyFile),
		filepath.Join(dir, IssuerCertFile), filepath.Join(dir, IssuerKeyFile),
		filepath.Join(dir, BundleFile), filepath.Join(dir, IssuerJSONFile),
	}
	if err := refuseExisting(files, "init-ca"); err != nil {
		return err
	}
	now := time.Now()

	rootKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	rootSerial, err := newSerial()
	if err != nil {
		return err
	}
	rootTmpl := &x509.Certificate{
		SerialNumber:          rootSerial,
		Subject:               pkix.Name{CommonName: "FeOS platform root CA " + o.TrustDomain},
		NotBefore:             now.Add(-backdate),
		NotAfter:              now.Add(rootLifetime),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            1,
	}
	constrain(rootTmpl, o.DNSDomains)
	rootDER, err := x509.CreateCertificate(rand.Reader, rootTmpl, rootTmpl, &rootKey.PublicKey, rootKey)
	if err != nil {
		return fmt.Errorf("make root: %w", err)
	}
	root, err := x509.ParseCertificate(rootDER)
	if err != nil {
		return err
	}
	issuerDER, issuerKeyDER, err := makeIssuer(o, root, rootKey, now)
	if err != nil {
		return err
	}
	rootKeyDER, err := x509.MarshalPKCS8PrivateKey(rootKey)
	if err != nil {
		return err
	}
	meta, err := json.Marshal(issuerJSON{ControlPlaneDomain: cp, TrustDomain: o.TrustDomain})
	if err != nil {
		return err
	}
	_, err = writeAll([]newFile{
		{files[1], 0o600, pemBlock("PRIVATE KEY", rootKeyDER)},
		{files[0], 0o644, pemBlock("CERTIFICATE", rootDER)},
		{files[3], 0o600, pemBlock("PRIVATE KEY", issuerKeyDER)},
		{files[2], 0o644, pemBlock("CERTIFICATE", issuerDER)},
		{files[4], 0o644, pemBlock("CERTIFICATE", rootDER)},
		{files[5], 0o644, append(meta, '\n')},
	})
	return err
}

// NewIssuer makes a new issuer under the root in RootDir, whose root.crt
// and root.key it reads, into Dir: issuer.crt, issuer.key, issuer.json and
// bundle.crt (that root), as InitCA makes them, without a URI constraint
// (ADR 0024). It replaces an issuer, of an old CA that constrained URIs or
// near its end; the root, the bundle and the join tokens' pin stay. The
// DNS domains must lie within the root's, and the trust domain within its
// URI constraint, if it has one. It refuses to overwrite any file and
// refuses RootDir equal to Dir, and it loads the new issuer as an instance
// would before it returns; what it wrote is removed when that fails.
func NewIssuer(o InitCAOptions) error {
	cp, err := o.check()
	if err != nil {
		return err
	}
	rootDir, dir, err := separateDirs(o.RootDir, o.Dir, false)
	if err != nil {
		return err
	}
	roots, err := readCerts(filepath.Join(rootDir, RootCertFile))
	if err != nil {
		return err
	}
	root := roots[0]
	if !root.IsCA || !root.BasicConstraintsValid {
		return fmt.Errorf("%s is not a CA certificate", RootCertFile)
	}
	if err := root.CheckSignatureFrom(root); err != nil {
		return fmt.Errorf("%s is not a self-signed root: %w", RootCertFile, err)
	}
	rootKey, err := readKey(filepath.Join(rootDir, RootKeyFile))
	if err != nil {
		return err
	}
	if !publicKeysEqual(rootKey.Public(), root.PublicKey) {
		return fmt.Errorf("%s does not belong to %s", RootKeyFile, RootCertFile)
	}
	for _, d := range o.DNSDomains {
		if !dnsPermitted(root, d) {
			return fmt.Errorf("DNS domain %s lies outside the root's permitted DNS domains %v", d, root.PermittedDNSDomains)
		}
	}
	if !uriDomainPermitted(root, o.TrustDomain) {
		return fmt.Errorf("the root's name constraints do not permit trust domain %s", o.TrustDomain)
	}
	now := time.Now()
	if !now.Before(root.NotAfter) {
		return fmt.Errorf("the root expired on %s", root.NotAfter.UTC().Format(time.RFC3339))
	}
	files := []string{
		filepath.Join(dir, IssuerCertFile), filepath.Join(dir, IssuerKeyFile),
		filepath.Join(dir, BundleFile), filepath.Join(dir, IssuerJSONFile),
	}
	if err := refuseExisting(files, "new-issuer"); err != nil {
		return err
	}
	issuerDER, issuerKeyDER, err := makeIssuer(o, root, rootKey, now)
	if err != nil {
		return err
	}
	meta, err := json.Marshal(issuerJSON{ControlPlaneDomain: cp, TrustDomain: o.TrustDomain})
	if err != nil {
		return err
	}
	written, err := writeAll([]newFile{
		{files[1], 0o600, pemBlock("PRIVATE KEY", issuerKeyDER)},
		{files[0], 0o644, pemBlock("CERTIFICATE", issuerDER)},
		{files[2], 0o644, pemBlock("CERTIFICATE", root.Raw)},
		{files[3], 0o644, append(meta, '\n')},
	})
	if err != nil {
		return err
	}
	if _, err := LoadIssuer(dir, o.TrustDomain); err != nil {
		for _, p := range written {
			_ = os.Remove(p)
		}
		return fmt.Errorf("the new issuer does not load: %w", err)
	}
	return nil
}

// check validates the options and returns the control-plane domain.
func (o InitCAOptions) check() (string, error) {
	if err := ValidateTrustDomain(o.TrustDomain); err != nil {
		return "", err
	}
	if len(o.DNSDomains) == 0 {
		return "", errors.New("at least one DNS domain is required")
	}
	for _, d := range o.DNSDomains {
		if err := validateDomain(d); err != nil {
			return "", fmt.Errorf("DNS domain: %w", err)
		}
	}
	cp := o.ControlPlaneDomain
	if cp == "" {
		cp = "ferun." + o.DNSDomains[0]
	}
	if err := validateDomain(cp); err != nil {
		return "", fmt.Errorf("control-plane domain: %w", err)
	}
	under := false
	for _, d := range o.DNSDomains {
		if cp != d && within(cp, d) {
			under = true
			break
		}
	}
	if !under {
		return "", fmt.Errorf("control-plane domain %s does not lie under a DNS domain %v", cp, o.DNSDomains)
	}
	return cp, nil
}

// separateDirs returns the absolute root and CA directories, which must
// differ, also through symlinks, since the root's key stays offline. It
// makes the CA directory, and the root directory when makeRoot is set.
func separateDirs(rootArg, dirArg string, makeRoot bool) (string, string, error) {
	if rootArg == "" || dirArg == "" {
		return "", "", errors.New("both the root directory and the CA directory are required")
	}
	rootDir, err := filepath.Abs(rootArg)
	if err != nil {
		return "", "", err
	}
	dir, err := filepath.Abs(dirArg)
	if err != nil {
		return "", "", err
	}
	if rootDir == dir {
		return "", "", fmt.Errorf("the root directory must not be the CA directory (%s): the root key stays offline", dir)
	}
	mk := []string{dir}
	if makeRoot {
		mk = append(mk, rootDir)
	}
	for _, d := range mk {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return "", "", err
		}
	}
	realRoot, err := filepath.EvalSymlinks(rootDir)
	if err != nil {
		return "", "", err
	}
	realDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", "", err
	}
	if realRoot == realDir {
		return "", "", fmt.Errorf("the root directory must not be the CA directory (%s): the root key stays offline", realDir)
	}
	return rootDir, dir, nil
}

// refuseExisting fails when any of files exists: cmd never overwrites one.
func refuseExisting(files []string, cmd string) error {
	for _, f := range files {
		if _, err := os.Lstat(f); err == nil {
			return fmt.Errorf("%s exists; %s never overwrites a file", f, cmd)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}

// constrain gives a CA certificate the platform's critical name
// constraints: DNS names under domains, no IP address. No URI constraint:
// rustls-webpki fails every URI name beneath one (ADR 0024).
func constrain(t *x509.Certificate, domains []string) {
	t.PermittedDNSDomainsCritical = true
	t.PermittedDNSDomains = append([]string(nil), domains...)
	t.ExcludedIPRanges = []*net.IPNet{
		{IP: net.IPv4zero.To4(), Mask: net.CIDRMask(0, 32)},
		{IP: net.IPv6zero, Mask: net.CIDRMask(0, 128)},
	}
}

// makeIssuer makes an issuer (ECDSA P-256, 5 years but not past the root,
// path length 0) signed by the root, and returns its certificate and its
// PKCS#8 key in DER.
func makeIssuer(o InitCAOptions, root *x509.Certificate, rootKey crypto.Signer, now time.Time) ([]byte, []byte, error) {
	issuerKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	issuerSerial, err := newSerial()
	if err != nil {
		return nil, nil, err
	}
	notAfter := now.Add(issuerLifetime)
	if notAfter.After(root.NotAfter) {
		notAfter = root.NotAfter
	}
	issuerTmpl := &x509.Certificate{
		SerialNumber:          issuerSerial,
		Subject:               pkix.Name{CommonName: "FeOS issuer " + o.TrustDomain},
		NotBefore:             now.Add(-backdate),
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            0,
		MaxPathLenZero:        true,
	}
	constrain(issuerTmpl, o.DNSDomains)
	issuerDER, err := x509.CreateCertificate(rand.Reader, issuerTmpl, root, &issuerKey.PublicKey, rootKey)
	if err != nil {
		return nil, nil, fmt.Errorf("make issuer: %w", err)
	}
	issuerKeyDER, err := x509.MarshalPKCS8PrivateKey(issuerKey)
	if err != nil {
		return nil, nil, err
	}
	return issuerDER, issuerKeyDER, nil
}

// newFile is a file the CA commands write: path, mode and content.
type newFile struct {
	path string
	mode os.FileMode
	data []byte
}

// writeAll writes files in order and returns their paths. A failed write
// removes what this call wrote before it, so that a rerun starts from
// nothing: no key of a failed run stays on disk, and no root or issuer is
// left without the files that belong to it.
func writeAll(files []newFile) ([]string, error) {
	var written []string
	for _, w := range files {
		if err := writeNew(w.path, w.mode, w.data); err != nil {
			for _, p := range written {
				_ = os.Remove(p)
			}
			return nil, err
		}
		written = append(written, w.path)
	}
	return written, nil
}

func pemBlock(typ string, der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der})
}

// writeNew writes a file that must not exist yet (O_EXCL), so that a race
// with another init-ca never overwrites a key. A file it created but could
// not write completely is removed again.
func writeNew(path string, mode os.FileMode, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(path)
	}
	return err
}
