// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"crypto"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gardenlinux/feos/go/internal/ingress/wire"
	"github.com/gardenlinux/feos/go/internal/pki"
)

func TestStateDirAtomicWritesAndModes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state")
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	// A temporary file a crash left behind.
	stale := filepath.Join(path, tmpPrefix+"table.json-123")
	if err := os.WriteFile(stale, []byte("half"), 0o600); err != nil {
		t.Fatal(err)
	}
	d, err := openStateDir(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != dirMode {
		t.Fatalf("directory mode %v, %v; want %v", fi.Mode().Perm(), err, os.FileMode(dirMode))
	}
	if fileExists(stale) {
		t.Error("the stale temporary file is still there")
	}
	for i, content := range []string{"first", "second, longer than the first"} {
		if err := d.write("x", []byte(content)); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(filepath.Join(path, "x"))
		if err != nil || string(got) != content {
			t.Fatalf("write %d: %q, %v", i, got, err)
		}
		fi, err := os.Stat(filepath.Join(path, "x"))
		if err != nil || fi.Mode().Perm() != fileMode {
			t.Fatalf("file mode %v, %v", fi.Mode().Perm(), err)
		}
	}
	// A file made with a wider mode is narrowed when it is replaced.
	if err := os.WriteFile(filepath.Join(path, "y"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := d.write("y", []byte("new")); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(filepath.Join(path, "y")); fi.Mode().Perm() != fileMode {
		t.Errorf("replaced file mode %v", fi.Mode().Perm())
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), tmpPrefix) {
			t.Errorf("temporary file %s left behind", e.Name())
		}
	}
}

func TestStateDirFiles(t *testing.T) {
	d, err := openStateDir(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}

	// Missing files read as nothing.
	if k, err := d.loadKey(); k != nil || err != nil {
		t.Errorf("loadKey on nothing: %v %v", k, err)
	}
	if c, err := d.loadCert(); c != nil || err != nil {
		t.Errorf("loadCert on nothing: %v %v", c, err)
	}
	if _, ok, err := d.loadWGKey(); ok || err != nil {
		t.Errorf("loadWGKey on nothing: %v %v", ok, err)
	}
	if m, err := d.loadMesh(); m != nil || err != nil {
		t.Errorf("loadMesh on nothing: %v %v", m, err)
	}
	if s, _, err := d.loadTable(); s != nil || err != nil {
		t.Errorf("loadTable on nothing: %v %v", s, err)
	}
	if lost, err := d.loseCert(time.Now()); lost != "" || err != nil {
		t.Errorf("loseCert on nothing: %q %v", lost, err)
	}
	if text, ok, err := d.loadRemoved(); ok || text != "" || err != nil {
		t.Errorf("loadRemoved on nothing: %q %v %v", text, ok, err)
	}
	if err := d.clearRemoved(); err != nil {
		t.Errorf("clearRemoved on nothing: %v", err)
	}

	// Keys round-trip.
	k, err := d.newKey()
	if err != nil {
		t.Fatal(err)
	}
	k2, err := d.loadKey()
	if err != nil || !k2.Public().(interface{ Equal(crypto.PublicKey) bool }).Equal(k.Public()) {
		t.Errorf("tls.key round trip: %v", err)
	}
	if data, _ := os.ReadFile(d.file(keyFile)); !strings.HasPrefix(string(data), "-----BEGIN PRIVATE KEY-----") {
		t.Errorf("tls.key is not PKCS #8 PEM: %q", data)
	}
	wk, err := d.newWGKey()
	if err != nil {
		t.Fatal(err)
	}
	if got, ok, err := d.loadWGKey(); !ok || err != nil || got != wk {
		t.Errorf("wg.key round trip: %v %v", ok, err)
	}
	if data, _ := os.ReadFile(d.file(wgKeyFile)); len(data) != 32 {
		t.Errorf("wg.key holds %d bytes", len(data))
	}
	if err := os.WriteFile(d.file(wgKeyFile), []byte("short"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := d.loadWGKey(); err == nil {
		t.Error("a short wg.key read without an error")
	}
	if err := os.WriteFile(d.file(keyFile), []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := d.loadKey(); err == nil {
		t.Error("a damaged tls.key read without an error")
	}

	// join-tried: hashes appended once each; other lines skipped.
	h1, h2 := pki.HashJoinSecret("a"), pki.HashJoinSecret("b")
	if err := os.WriteFile(d.file(triedFile), []byte("not a hash\n"+h1), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, h := range []string{h1, h2, h2} {
		if err := d.addTried(h); err != nil {
			t.Fatal(err)
		}
	}
	tried, err := d.loadTried()
	if err != nil || len(tried) != 2 || !tried[h1] || !tried[h2] {
		t.Errorf("join-tried %v, %v", tried, err)
	}
	if data, _ := os.ReadFile(d.file(triedFile)); strings.Count(string(data), h2) != 1 || !strings.HasSuffix(string(data), h2+"\n") {
		t.Errorf("join-tried content %q", data)
	}

	// removed round-trips and is cleared.
	const removal = "ingress ing-0 was removed; join it again"
	if err := d.saveRemoved(removal); err != nil {
		t.Fatal(err)
	}
	if text, ok, err := d.loadRemoved(); !ok || text != removal || err != nil {
		t.Errorf("removed %q %v %v", text, ok, err)
	}
	if fi, err := os.Stat(d.file(removedFile)); err != nil {
		t.Error(err)
	} else if fi.Mode().Perm() != fileMode {
		t.Errorf("removed has mode %v", fi.Mode().Perm())
	}
	if err := d.clearRemoved(); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := d.loadRemoved(); ok || err != nil {
		t.Errorf("removed after clearing: %v %v", ok, err)
	}

	// cert.json is set aside under the time of the loss, never over an
	// earlier one.
	now := time.Unix(1_800_000_000, 0)
	for i, want := range []string{"cert.json.lost-1800000000", "cert.json.lost-1800000000-1"} {
		if err := d.saveCert(wire.Certificate{SpiffeID: "x"}); err != nil {
			t.Fatal(err)
		}
		lost, err := d.loseCert(now)
		if err != nil || lost != want {
			t.Errorf("loss %d: %q, %v; want %q", i, lost, err, want)
		}
	}
	if fileExists(d.file(certFile)) {
		t.Error("cert.json is still there")
	}

	// mesh.json and table.json round-trip.
	place := meshPlace{PodNetwork: "fd00:1:2::/48", Prefix: "fd00:1:2:1::/64", Address: "fd00:1:2:1::1"}
	if err := d.saveMesh(place); err != nil {
		t.Fatal(err)
	}
	if got, err := d.loadMesh(); err != nil || got == nil || *got != place {
		t.Errorf("mesh.json %+v, %v", got, err)
	}
	s := wire.NewState()
	if _, err := s.Apply(&wire.Table{Seq: 1, Whole: true, Envs: []wire.Env{testEnv("e1")}, Mesh: &wire.Mesh{Version: 2}}); err != nil {
		t.Fatal(err)
	}
	saved := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	if err := d.saveTable(s, saved); err != nil {
		t.Fatal(err)
	}
	got, at, err := d.loadTable()
	if err != nil || got == nil || !at.Equal(saved) || len(got.Envs) != 1 || got.Mesh.Version != 2 || got.Seq != 1 {
		t.Errorf("table.json %+v at %v, %v", got, at, err)
	}
}

func TestCorruptCacheIsIgnoredWithAWarning(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	d, err := openStateDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.write(tableFile, []byte(`{"savedAt": "yesterday", "state": [`)); err != nil {
		t.Fatal(err)
	}
	if err := d.write(meshFile, []byte(`not json`)); err != nil {
		t.Fatal(err)
	}
	logs := &logBuf{}
	rec := &recorder{}
	mesh := &fakeMesh{}
	a, err := New(Config{
		Name: testName, StateDir: dir, ControlPlane: []string{"127.0.0.1:1"}, ControlPlaneName: "ferun.ingress.test",
		TrustDomain: "ingress.test", MeshEndpoint: "10.0.0.5:51820",
	}, Deps{Mesh: mesh, OnState: rec.onState, Log: slog.New(slog.NewTextHandler(logs, nil))})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	a.startFromCache()
	if a.HasState() || a.MeshConfigured() {
		t.Errorf("hasState %v, meshConfigured %v from damaged files", a.HasState(), a.MeshConfigured())
	}
	if s, _ := rec.lastState(); s != nil {
		t.Error("OnState called with a damaged cache")
	}
	if _, n := mesh.lastConfig(); n != 0 {
		t.Error("the mesh configured from a damaged mesh.json")
	}
	if !logs.contains("ignoring a damaged table cache") || !logs.contains("level=WARN") {
		t.Errorf("no warning about the cache:\n%s", strings.Join(logs.lines, ""))
	}
	if !logs.contains("ignoring a damaged mesh.json") {
		t.Error("no warning about mesh.json")
	}
	// The damaged cache does not stop the identity's files from being
	// made.
	for _, name := range []string{keyFile, wgKeyFile} {
		if !fileExists(filepath.Join(dir, name)) {
			t.Errorf("%s was not made", name)
		}
	}
}

func TestDamagedCertificateIsSetAside(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	d, err := openStateDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.newKey(); err != nil {
		t.Fatal(err)
	}
	if err := d.write(certFile, []byte(`{"chain": ["AAAA"]}`)); err != nil {
		t.Fatal(err)
	}
	keyBefore, _ := os.ReadFile(d.file(keyFile))
	a, err := New(Config{
		Name: testName, StateDir: dir, ControlPlane: []string{"127.0.0.1:1"}, ControlPlaneName: "ferun.ingress.test",
		TrustDomain: "ingress.test", MeshEndpoint: "10.0.0.5:51820",
	}, Deps{Mesh: &fakeMesh{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.load(); err != nil {
		t.Fatal(err)
	}
	if a.identity() != nil {
		t.Error("an identity from a damaged certificate")
	}
	if fileExists(d.file(certFile)) || len(lostFiles(t, dir)) != 1 {
		t.Error("the damaged certificate was not set aside")
	}
	if keyAfter, _ := os.ReadFile(d.file(keyFile)); string(keyAfter) == string(keyBefore) {
		t.Error("the next join would reuse the old key")
	}
}
