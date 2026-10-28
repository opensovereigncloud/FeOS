// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func noEnv(string) string { return "" }

// runCmd runs ferun-ingress with args and returns its exit code and output.
func runCmd(args []string, getenv func(string) string) (int, string, string) {
	var out, errOut strings.Builder
	code := run(args, getenv, &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestCommands(t *testing.T) {
	t.Parallel()
	if code, out, _ := runCmd([]string{"version"}, noEnv); code != 0 || strings.TrimSpace(out) == "" {
		t.Fatalf("version: %d %q", code, out)
	}
	if code, _, errOut := runCmd([]string{"frobnicate"}, noEnv); code != 2 || !strings.Contains(errOut, `unknown command "frobnicate"`) {
		t.Fatalf("unknown command: %d %q", code, errOut)
	}
	if code, out, _ := runCmd([]string{"--help"}, noEnv); code != 0 || !strings.Contains(out, "prepare") {
		t.Fatalf("--help: %d %q", code, out)
	}
	if code, out, _ := runCmd([]string{"serve", "--help"}, noEnv); code != 0 || !strings.Contains(out, "-listen-https") {
		t.Fatalf("serve --help: %d %q", code, out)
	}
	// serve without its required flags is a usage error, and so is the
	// default command.
	for _, args := range [][]string{{"serve"}, {"--name", "ferun-ingress-0"}} {
		if code, _, errOut := runCmd(args, noEnv); code != 2 || !strings.Contains(errOut, "--control-plane is required") {
			t.Fatalf("%q: %d %q", args, code, errOut)
		}
	}
}

func TestStatus(t *testing.T) {
	t.Parallel()
	ready := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/statusz" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"name":"ferun-ingress-0","ready":true}` + "\n"))
	}))
	defer ready.Close()
	addr := strings.TrimPrefix(ready.URL, "http://")
	env := func(k string) string {
		if k == "FERUN_INGRESS_HEALTH_LISTEN" {
			return addr
		}
		return ""
	}
	code, out, errOut := runCmd([]string{"status"}, env)
	if code != 0 || out != `{"name":"ferun-ingress-0","ready":true}`+"\n" {
		t.Fatalf("status: %d %q %q", code, out, errOut)
	}

	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer broken.Close()
	code, _, errOut = runCmd([]string{"status", "--health-listen", strings.TrimPrefix(broken.URL, "http://")}, env)
	if code != 1 || !strings.Contains(errOut, "500") || !strings.Contains(errOut, "boom") {
		t.Fatalf("status of a failing server: %d %q", code, errOut)
	}
	broken.Close()
	if code, _, _ := runCmd([]string{"status", "--health-listen", strings.TrimPrefix(broken.URL, "http://")}, env); code != 1 {
		t.Fatalf("status of a closed listener: %d", code)
	}
	if code, _, errOut := runCmd([]string{"status"}, noEnv); code != 2 || !strings.Contains(errOut, "FERUN_INGRESS_HEALTH_LISTEN") {
		t.Fatalf("status without the listener: %d %q", code, errOut)
	}
}
