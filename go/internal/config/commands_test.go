// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"strings"
	"testing"

	"github.com/gardenlinux/feos/go/internal/model"
)

// join-token's --kind: node by default, ingress for an instance of
// ferun-ingress (with or without a pinned name), from the flag or its
// variable; anything else is refused.
func TestJoinTokenKind(t *testing.T) {
	t.Parallel()
	vars := map[string]string{"FERUN_DATABASE_URL": testURL}
	jt, err := ParseJoinToken(nil, env(vars))
	if err != nil || jt.Kind != model.JoinKindNode {
		t.Fatalf("default %+v %v", jt, err)
	}
	jt, err = ParseJoinToken([]string{"--kind", "ingress", "--node", "ferun-ingress-0"}, env(vars))
	if err != nil || jt.Kind != model.JoinKindIngress || jt.Node != "ferun-ingress-0" {
		t.Fatalf("ingress %+v %v", jt, err)
	}
	jt, err = ParseJoinToken([]string{"--kind=node"}, env(map[string]string{"FERUN_DATABASE_URL": testURL, "FERUN_KIND": "ingress"}))
	if err != nil || jt.Kind != model.JoinKindNode {
		t.Fatalf("the flag over its variable %+v %v", jt, err)
	}
	jt, err = ParseJoinToken(nil, env(map[string]string{"FERUN_DATABASE_URL": testURL, "FERUN_KIND": "ingress"}))
	if err != nil || jt.Kind != model.JoinKindIngress || jt.Node != "" {
		t.Fatalf("the variable %+v %v", jt, err)
	}
	for _, kind := range []string{"", "pod", "Ingress", "nodes"} {
		if _, err := ParseJoinToken([]string{"--kind", kind}, env(vars)); err == nil || !strings.Contains(err.Error(), "--kind") {
			t.Errorf("--kind %q: %v", kind, err)
		}
	}
	if _, err := ParseJoinToken([]string{"--kind", "ingress", "--node", "Bad_Name"}, env(vars)); err == nil {
		t.Error("a bad ingress name parsed")
	}
	var b strings.Builder
	Usage(&b, CmdJoinToken)
	if !strings.Contains(b.String(), `--kind (FERUN_KIND, default "node")`) {
		t.Errorf("help %s", b.String())
	}
}
