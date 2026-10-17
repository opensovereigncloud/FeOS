// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"log/slog"
	"slices"
	"testing"
	"time"
)

func TestParse(t *testing.T) {
	env := map[string]string{
		"FERUN_TESTNODE_SERVER_NAME":   "ferun.example.test",
		"FERUN_TESTNODE_ADDR":          "10.0.0.1:7443,10.0.0.2:7443",
		"FERUN_TESTNODE_NAME":          "from-env",
		"FERUN_TESTNODE_MEMORY":        "8GiB",
		"FERUN_TESTNODE_OBJECT_STORE":  "bucket",
		"FERUN_TESTNODE_PENDING_DELAY": "1s",
		"FERUN_TESTNODE_MESH":          "true",
	}
	cfg, level, err := parse([]string{"--name", "from-flag", "--disk", "512", "--log-level", "debug", "--abort-on-cancel",
		"--mesh-endpoint", "10.0.0.9:51820"}, func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Mesh || cfg.MeshEndpoint != "10.0.0.9:51820" {
		t.Fatalf("mesh %v %q", cfg.Mesh, cfg.MeshEndpoint)
	}
	if cfg.Name != "from-flag" || cfg.ServerName != "ferun.example.test" || cfg.TrustDomain != "feos.local" ||
		!slices.Equal(cfg.Addrs, []string{"10.0.0.1:7443", "10.0.0.2:7443"}) || cfg.MemoryBytes != 8<<30 || cfg.DiskBytes != 512 ||
		cfg.ObjectStore == nil || cfg.PendingDelay != time.Second || !cfg.AbortOnCancel || cfg.MaxStreams != 1024 || cfg.CPUCores != 16 ||
		level != slog.LevelDebug {
		t.Fatalf("config %+v, level %v", cfg, level)
	}
	for _, bad := range [][]string{{"--memory", "lots"}, {"--log-level", "loud"}, {"extra"}, {"--pending-delay", "soon"}} {
		if _, _, err := parse(bad, func(string) string { return "" }); err == nil {
			t.Errorf("%v passed", bad)
		}
	}
	if _, _, err := parse(nil, func(k string) string {
		if k == "FERUN_TESTNODE_CPUS" {
			return "many"
		}
		return ""
	}); err == nil {
		t.Error("a bad environment variable passed")
	}
}

// A flag wins over its variable, also the repeatable --addr.
func TestFlagWinsOverEnvironment(t *testing.T) {
	env := map[string]string{"FERUN_TESTNODE_ADDR": "10.0.0.1:7443", "FERUN_TESTNODE_CPUS": "many"}
	cfg, _, err := parse([]string{"--addr", "10.0.0.9:7443", "--addr", "10.0.0.8:7443", "--cpus", "4"}, func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(cfg.Addrs, []string{"10.0.0.9:7443", "10.0.0.8:7443"}) || cfg.CPUCores != 4 {
		t.Fatalf("addrs %v, cpus %d", cfg.Addrs, cfg.CPUCores)
	}
}

// --object-store is a store of this process; --object-store-dir one that
// every process naming the directory shares.
func TestObjectStoreFlags(t *testing.T) {
	none := func(string) string { return "" }
	a, _, err := parse([]string{"--object-store", "bucket"}, none)
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := parse([]string{"--object-store", "bucket"}, none)
	if err != nil {
		t.Fatal(err)
	}
	if a.ObjectStore.ID() == b.ObjectStore.ID() {
		t.Fatal("two processes' in-memory stores report one ID")
	}
	dir := t.TempDir()
	c, _, err := parse([]string{"--object-store-dir", dir}, none)
	if err != nil {
		t.Fatal(err)
	}
	d, _, err := parse(nil, func(k string) string {
		if k == "FERUN_TESTNODE_OBJECT_STORE_DIR" {
			return dir
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	if c.ObjectStore.ID() != d.ObjectStore.ID() {
		t.Fatal("two processes naming one directory report different IDs")
	}
	if _, _, err := parse([]string{"--object-store", "bucket", "--object-store-dir", dir}, none); err == nil {
		t.Fatal("both stores passed")
	}
}
