// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gardenlinux/feos/go/internal/db"
	"github.com/gardenlinux/feos/go/internal/db/dbtest"
	"github.com/gardenlinux/feos/go/internal/pki"
	"github.com/gardenlinux/feos/go/internal/store"
)

func noEnv(string) string { return "" }

// runCmd runs ferund with args and returns its exit code and output.
func runCmd(args []string, getenv func(string) string) (int, string, string) {
	var out, errOut strings.Builder
	code := run(args, getenv, &out, &errOut)
	return code, out.String(), errOut.String()
}

// init-ca makes the root and the issuer, which an instance loads; it
// refuses the root's directory as the issuer's and never overwrites.
func TestInitCA(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	root, dir := filepath.Join(base, "root"), filepath.Join(base, "ca")
	code, out, errOut := runCmd([]string{"init-ca", "--root-dir", root, "--dir", dir, "--trust-domain", "feos.test",
		"--dns-domain", "example.test"}, noEnv)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if !strings.Contains(out, "made the root in "+root) {
		t.Fatalf("output %q", out)
	}
	iss, err := pki.LoadIssuer(dir, "feos.test")
	if err != nil {
		t.Fatal(err)
	}
	if iss.ControlPlaneDomain() != "ferun.example.test" {
		t.Fatalf("control-plane domain %s", iss.ControlPlaneDomain())
	}
	if _, err := os.Stat(filepath.Join(dir, "root.key")); !os.IsNotExist(err) {
		t.Fatalf("root.key in the issuer's directory: %v", err)
	}
	code, _, errOut = runCmd([]string{"init-ca", "--root-dir", root, "--dir", dir, "--trust-domain", "feos.test",
		"--dns-domain", "example.test"}, noEnv)
	if code != 1 || errOut == "" {
		t.Fatalf("overwrite: exit %d %s", code, errOut)
	}
	other := filepath.Join(base, "same")
	code, _, _ = runCmd([]string{"init-ca", "--root-dir", other, "--dir", other, "--dns-domain", "example.test"}, noEnv)
	if code != 1 {
		t.Fatalf("root dir = dir: exit %d", code)
	}
	code, _, errOut = runCmd([]string{"init-ca", "--root-dir", root}, noEnv)
	if code != 2 || !strings.Contains(errOut, "--dns-domain is required") {
		t.Fatalf("usage: exit %d %s", code, errOut)
	}
}

// migrate applies the migrations to an empty database; join-token issues
// a token pinned to the issuer's root, with the trust domain of
// --trust-domain, straight into the database.
func TestMigrateAndJoinToken(t *testing.T) {
	t.Parallel()
	url := dbtest.NewDatabase(t)
	code, _, errOut := runCmd([]string{"migrate"}, func(k string) string {
		if k == "FERUN_DATABASE_URL" {
			return url
		}
		return ""
	})
	if code != 0 {
		t.Fatalf("migrate: exit %d %s", code, errOut)
	}
	ctx := context.Background()
	d, err := db.Open(ctx, url, dbtest.Logger(t))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	// Every embedded migration is applied.
	files, err := os.ReadDir("../../internal/db/migrations")
	if err != nil {
		t.Fatal(err)
	}
	var versions int
	if err := d.Pool.QueryRow(ctx, "SELECT count(*) FROM schema_migrations").Scan(&versions); err != nil || versions != len(files) {
		t.Fatalf("migrations %d of %d: %v", versions, len(files), err)
	}

	base := t.TempDir()
	dir := filepath.Join(base, "ca")
	if err := pki.InitCA(pki.InitCAOptions{RootDir: filepath.Join(base, "root"), Dir: dir, TrustDomain: "run.example.test",
		DNSDomains: []string{"example.test"}}); err != nil {
		t.Fatal(err)
	}
	iss, err := pki.LoadIssuer(dir, "run.example.test")
	if err != nil {
		t.Fatal(err)
	}
	code, out, errOut := runCmd([]string{"join-token", "--database-url", url, "--ca-dir", dir, "--trust-domain",
		"run.example.test", "--node", "n7", "--ttl", "10m"}, noEnv)
	if code != 0 {
		t.Fatalf("join-token: exit %d %s", code, errOut)
	}
	token := strings.TrimSpace(out)
	sent, pin, err := pki.SplitJoinToken(token)
	if err != nil || pin != iss.JoinPin() {
		t.Fatalf("token %q: %v", token, err)
	}
	if !strings.Contains(errOut, "for node n7") {
		t.Fatalf("stderr %q", errOut)
	}
	toks, err := store.New(d).ListJoinTokens(ctx)
	if err != nil || len(toks) != 1 || toks[0].Hash != pki.HashJoinSecret(sent) || toks[0].NodeName != "n7" ||
		toks[0].CreatedBy != "ferund join-token" {
		t.Fatalf("tokens %+v %v", toks, err)
	}
	// The issuer of another trust domain is refused.
	code, _, _ = runCmd([]string{"join-token", "--database-url", url, "--ca-dir", dir, "--trust-domain", "feos.test"}, noEnv)
	if code != 1 {
		t.Fatalf("wrong trust domain: exit %d", code)
	}
}

func TestCommandLine(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		args []string
		code int
		out  string
	}{
		{[]string{"--help"}, 0, "Commands:"},
		{[]string{"help", "join-token"}, 0, "--node (FERUN_NODE)"},
		{[]string{"serve", "--help"}, 0, "--database-url (FERUN_DATABASE_URL)"},
		{[]string{"migrate", "-h"}, 0, "Applies the database migrations"},
		{[]string{"version"}, 0, ""},
		{[]string{"frobnicate"}, 2, ""},
		{[]string{"serve"}, 2, ""},
		{[]string{"--listen", ":1"}, 2, ""},
	} {
		code, out, errOut := runCmd(c.args, noEnv)
		if code != c.code || !strings.Contains(out, c.out) {
			t.Errorf("%v: exit %d, out %q, err %q", c.args, code, out, errOut)
		}
	}
}
