// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"log/slog"
	"slices"
	"testing"
	"time"

	networkv1 "github.com/gardenlinux/feos/go/gen/feos/network/v1"
	podv1 "github.com/gardenlinux/feos/go/gen/feos/pod/v1"
	volumev1 "github.com/gardenlinux/feos/go/gen/feos/volume/v1"
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
	if cfg.FakeExec || !cfg.CheckSecurityGroups || !cfg.CheckNameTables || cfg.NoNameService {
		t.Fatalf("fake exec %v, security groups checked %v, name tables checked %v, no name service %v", cfg.FakeExec,
			cfg.CheckSecurityGroups, cfg.CheckNameTables, cfg.NoNameService)
	}
	cfg, _, err = parse([]string{"--fake-exec", "--check-security-groups=false", "--check-name-tables=false",
		"--no-name-service"}, func(k string) string { return env[k] })
	if err != nil || !cfg.FakeExec || cfg.CheckSecurityGroups || cfg.CheckNameTables || !cfg.NoNameService {
		t.Fatalf("fake exec %v, security groups checked %v, name tables checked %v, no name service %v, %v", cfg.FakeExec,
			cfg.CheckSecurityGroups, cfg.CheckNameTables, cfg.NoNameService, err)
	}
	cfg, _, err = parse(nil, func(k string) string {
		if k == "FERUN_TESTNODE_FAKE_EXEC" {
			return "true"
		}
		return env[k]
	})
	if err != nil || !cfg.FakeExec {
		t.Fatalf("FERUN_TESTNODE_FAKE_EXEC: %v, %v", cfg.FakeExec, err)
	}
	cfg, level, err = parse([]string{"--name", "from-flag", "--disk", "512", "--log-level", "debug", "--abort-on-cancel",
		"--mesh-endpoint", "10.0.0.9:51820"}, func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
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

// --exit, --start-log and --port-addr drive an environment end to end.
func TestEnvironmentFlags(t *testing.T) {
	cfg, _, err := parse([]string{"--exit", "job/*=0@3s", "--exit", "service/{a,b}=1", "--start-log", "--fake-processes",
		"--port-addr", "8080=127.0.0.1:18080,5173=127.0.0.1:15173"}, func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.ContainerExits) != 2 || cfg.ContainerExits[0].Object != "job/*" || cfg.ContainerExits[0].After != 3*time.Second ||
		cfg.ContainerExits[1].Object != "service/{a,b}" || cfg.ContainerExits[1].Code != 1 || !cfg.StartLog || !cfg.FakeProcesses ||
		cfg.PortAddrs[8080] != "127.0.0.1:18080" || cfg.PortAddrs[5173] != "127.0.0.1:15173" {
		t.Fatalf("config %+v", cfg)
	}
	cfg, _, err = parse(nil, func(k string) string {
		return map[string]string{"FERUN_TESTNODE_EXIT": "job/*=0", "FERUN_TESTNODE_START_LOG": "true"}[k]
	})
	if err != nil || len(cfg.ContainerExits) != 1 || !cfg.StartLog {
		t.Fatalf("from the environment: %+v, %v", cfg, err)
	}
	for _, bad := range [][]string{{"--exit", "job/*"}, {"--port-addr", "8080"}, {"--port-addr", "0=127.0.0.1:1"}, {"--port-addr", "x=127.0.0.1:1"}} {
		if _, _, err := parse(bad, func(string) string { return "" }); err == nil {
			t.Errorf("%v passed", bad)
		}
	}
}

// SIGUSR1's summary names each pod's object, groups, name tables and
// volumes, each volume's holder, each group's version and members and
// each name table's version and names.
func TestSummary(t *testing.T) {
	pods := []*podv1.PodInfo{{PodId: "p1", State: podv1.PodState_POD_STATE_RUNNING, Spec: &podv1.PodSpec{
		Labels:  map[string]string{"ferun.object": "addon/postgres"},
		Network: &podv1.PodNetwork{SecurityGroups: []string{"env-1"}, NameTables: []string{"env-1"}},
		Volumes: []*podv1.PodVolume{{Name: "data", VolumeId: "v1"}},
	}}}
	vols := []*volumev1.Volume{{VolumeId: "v1", State: volumev1.VolumeState_VOLUME_STATE_ATTACHED, PodId: "p1", Labels: map[string]string{"ferun.object": "addon/postgres"}}}
	groups := []*networkv1.SecurityGroup{{Id: "env-1", Version: 3, Members: []string{"fd00::2"}, Rules: []*networkv1.Rule{{}, {}}}}
	tables := []*networkv1.NameTable{{Id: "env-1", Version: 7, Entries: []*networkv1.NameEntry{{Name: "db", Addresses: []string{"fd00::2"}}}}}
	b, err := json.Marshal(summary(pods, vols, groups, tables))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"pods":[{"id":"p1","state":"RUNNING","object":"addon/postgres","groups":["env-1"],"nameTables":["env-1"],` +
		`"volumes":["v1"]}],` +
		`"volumes":[{"id":"v1","state":"ATTACHED","pod":"p1","object":"addon/postgres"}],` +
		`"groups":[{"id":"env-1","version":3,"members":["fd00::2"],"rules":2}],` +
		`"nameTables":[{"id":"env-1","version":7,"names":{"db":["fd00::2"]}}]}`
	if string(b) != want {
		t.Fatalf("summary %s", b)
	}
	if b, _ := json.Marshal(summary(nil, nil, nil, nil)); string(b) != `{"pods":[],"volumes":[],"groups":[],"nameTables":[]}` {
		t.Fatalf("an empty node %s", b)
	}
}
