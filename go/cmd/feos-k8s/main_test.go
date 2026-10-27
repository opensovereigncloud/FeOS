// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"io"
	"strings"
	"testing"
	"time"
)

func TestParse(t *testing.T) {
	env := map[string]string{
		"FEOS_K8S_FERUN_URL":              "http://ferund.ferun:8100",
		"FEOS_K8S_FERUN_CREDENTIALS_FILE": "/creds",
		"FEOS_K8S_NODE_NAME_PREFIX":       "shoot--myproject--myshoot-",
		"FEOS_K8S_INTERVAL":               "10s",
	}
	o, err := parse([]string{"--interval", "3s", "--namespace", "x"}, func(k string) string { return env[k] }, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if o.ferunURL != "http://ferund.ferun:8100" || o.ferunCreds != "/creds" || o.ctl.NodeNamePrefix != "shoot--myproject--myshoot-" ||
		o.ctl.Interval != 3*time.Second || o.ctl.Namespace != "x" || o.ctl.JoinSecret != "feosd-join" ||
		o.ctl.GuardImage != "registry.k8s.io/pause:3.10" || o.ctl.NodeSelector != "feos.fecode.dev/node=true" ||
		o.ctl.JoinTokenTTL != time.Hour {
		t.Fatalf("options %+v", o)
	}

	for _, args := range [][]string{
		{"--ferun-credentials-file", "/c", "--node-name-prefix", "p"},
		{"--ferun-url", "http://f", "--node-name-prefix", "p"},
		{"--ferun-url", "http://f", "--ferun-credentials-file", "/c"},
		{"--ferun-url", "http://f", "--ferun-credentials-file", "/c", "--node-name-prefix", "p", "--interval", "0s"},
	} {
		if _, err := parse(args, func(string) string { return "" }, io.Discard); err == nil {
			t.Errorf("%q: no error", args)
		}
	}
	if code := run([]string{"--node-name-prefix", "p"}, func(string) string { return "" }, io.Discard); code != 2 {
		t.Fatalf("exit %d; want 2", code)
	}
	var out strings.Builder
	if code := run([]string{"--help"}, func(string) string { return "" }, &out); code != 0 ||
		!strings.Contains(out.String(), "-node-name-prefix") {
		t.Fatalf("exit %d, %s", code, out.String())
	}
}
