// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package pki

import (
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

// InitCAOptions are the inputs of `ferund init-ca`.
type InitCAOptions struct {
	// RootDir receives root.crt and root.key; it must not be Dir.
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
}

// InitCA makes the platform CA: the root (ECDSA P-256, 10 years, path
// length 1) in RootDir and the issuer (ECDSA P-256, 5 years, path length 0)
// in Dir, both with critical name constraints that permit the DNS domains
// and the trust domain's URIs and exclude every IP address. It refuses to
// overwrite any file and refuses RootDir equal to Dir.
func InitCA(o InitCAOptions) error {
	if err := ValidateTrustDomain(o.TrustDomain); err != nil {
		return err
	}
	if len(o.DNSDomains) == 0 {
		return errors.New("at least one DNS domain is required")
	}
	for _, d := range o.DNSDomains {
		if err := validateDomain(d); err != nil {
			return fmt.Errorf("DNS domain: %w", err)
		}
	}
	cp := o.ControlPlaneDomain
	if cp == "" {
		cp = "ferun." + o.DNSDomains[0]
	}
	if err := validateDomain(cp); err != nil {
		return fmt.Errorf("control-plane domain: %w", err)
	}
	under := false
	for _, d := range o.DNSDomains {
		if cp != d && within(cp, d) {
			under = true
			break
		}
	}
	if !under {
		return fmt.Errorf("control-plane domain %s does not lie under a DNS domain %v", cp, o.DNSDomains)
	}
	if o.RootDir == "" || o.Dir == "" {
		return errors.New("both the root directory and the CA directory are required")
	}
	rootDir, err := filepath.Abs(o.RootDir)
	if err != nil {
		return err
	}
	dir, err := filepath.Abs(o.Dir)
	if err != nil {
		return err
	}
	if rootDir == dir {
		return fmt.Errorf("the root directory must not be the CA directory (%s): the root key stays offline", dir)
	}
	for _, d := range []string{rootDir, dir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
	}
	realRoot, err := filepath.EvalSymlinks(rootDir)
	if err != nil {
		return err
	}
	realDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return err
	}
	if realRoot == realDir {
		return fmt.Errorf("the root directory must not be the CA directory (%s): the root key stays offline", realDir)
	}
	files := []string{
		filepath.Join(rootDir, RootCertFile), filepath.Join(rootDir, RootKeyFile),
		filepath.Join(dir, IssuerCertFile), filepath.Join(dir, IssuerKeyFile),
		filepath.Join(dir, BundleFile), filepath.Join(dir, IssuerJSONFile),
	}
	for _, f := range files {
		if _, err := os.Lstat(f); err == nil {
			return fmt.Errorf("%s exists; init-ca never overwrites a file", f)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}

	constraints := func(t *x509.Certificate) {
		t.PermittedDNSDomainsCritical = true
		t.PermittedDNSDomains = append([]string(nil), o.DNSDomains...)
		t.ExcludedIPRanges = []*net.IPNet{
			{IP: net.IPv4zero.To4(), Mask: net.CIDRMask(0, 32)},
			{IP: net.IPv6zero, Mask: net.CIDRMask(0, 128)},
		}
		t.PermittedURIDomains = []string{o.TrustDomain}
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
	constraints(rootTmpl)
	rootDER, err := x509.CreateCertificate(rand.Reader, rootTmpl, rootTmpl, &rootKey.PublicKey, rootKey)
	if err != nil {
		return fmt.Errorf("make root: %w", err)
	}
	root, err := x509.ParseCertificate(rootDER)
	if err != nil {
		return err
	}

	issuerKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	issuerSerial, err := newSerial()
	if err != nil {
		return err
	}
	issuerTmpl := &x509.Certificate{
		SerialNumber:          issuerSerial,
		Subject:               pkix.Name{CommonName: "FeOS issuer " + o.TrustDomain},
		NotBefore:             now.Add(-backdate),
		NotAfter:              now.Add(issuerLifetime),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            0,
		MaxPathLenZero:        true,
	}
	constraints(issuerTmpl)
	issuerDER, err := x509.CreateCertificate(rand.Reader, issuerTmpl, root, &issuerKey.PublicKey, rootKey)
	if err != nil {
		return fmt.Errorf("make issuer: %w", err)
	}

	rootKeyDER, err := x509.MarshalPKCS8PrivateKey(rootKey)
	if err != nil {
		return err
	}
	issuerKeyDER, err := x509.MarshalPKCS8PrivateKey(issuerKey)
	if err != nil {
		return err
	}
	meta, err := json.Marshal(issuerJSON{ControlPlaneDomain: cp})
	if err != nil {
		return err
	}
	writes := []struct {
		path string
		mode os.FileMode
		data []byte
	}{
		{files[1], 0o600, pemBlock("PRIVATE KEY", rootKeyDER)},
		{files[0], 0o644, pemBlock("CERTIFICATE", rootDER)},
		{files[3], 0o600, pemBlock("PRIVATE KEY", issuerKeyDER)},
		{files[2], 0o644, pemBlock("CERTIFICATE", issuerDER)},
		{files[4], 0o644, pemBlock("CERTIFICATE", rootDER)},
		{files[5], 0o644, append(meta, '\n')},
	}
	// A failed write removes what this call wrote before it, so that a
	// rerun starts from nothing: no root key of a failed run stays on disk,
	// and no root or issuer is left without the files that belong to it.
	var written []string
	for _, w := range writes {
		if err := writeNew(w.path, w.mode, w.data); err != nil {
			for _, p := range written {
				_ = os.Remove(p)
			}
			return err
		}
		written = append(written, w.path)
	}
	return nil
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
