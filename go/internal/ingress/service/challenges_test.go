// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"slices"
	"testing"
	"time"

	"github.com/gardenlinux/feos/go/internal/ingress/wire"
	"github.com/gardenlinux/feos/go/internal/store"
)

// The stream carries the ACME challenges: all of them in the whole table
// (none at first), then all of them again when they change. Once it has
// sent them it records their tokens as the ingress's
// (challenge_tokens); an ingress connected without a stream does not
// hold them.
func TestSyncChallenges(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	h.world()
	_, c, _ := h.connect("in-0", 1, "10.0.0.1:51820")
	h.connect("in-1", 2, "10.0.0.2:51820")

	sy := h.sync(c)
	first := sy.next(10 * time.Second)
	if !first.Whole || first.Challenges == nil || len(first.Challenges.Items) != 0 {
		t.Fatalf("first %+v", first)
	}

	if err := h.st.PutACMEChallenge(bg, store.ACMEChallenge{Token: "tok-1", Host: "dashboard.fecore.dev",
		KeyAuth: "tok-1.thumb"}, time.Minute); err != nil {
		t.Fatal(err)
	}
	got := sy.until(time.Second, "the challenge", func(t *wire.Table) bool { return t.Challenges != nil })
	want := []wire.Challenge{{Token: "tok-1", Host: "dashboard.fecore.dev", KeyAuth: "tok-1.thumb"}}
	if !slices.Equal(got.Challenges.Items, want) || !slices.Equal(sy.state.Challenges, want) || len(got.Envs) != 0 {
		t.Fatalf("delta %+v, state %+v", got, sy.state.Challenges)
	}
	var missing []string
	var connected int
	var err error
	eventually(t, 2*time.Second, "in-0 holding the challenge", func() bool {
		missing, connected, _, err = h.st.IngressChallengeHolders(bg, "tok-1", 45*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		return slices.Equal(missing, []string{"in-1"})
	})
	if connected != 2 {
		t.Fatalf("connected %d", connected)
	}

	// Deleted: an empty set.
	if _, err := h.st.ExpireACMEChallenges(bg); err != nil {
		t.Fatal(err)
	}
	h.exec("DELETE FROM acme_challenges")
	h.exec("SELECT pg_notify('ferun_ingress', $1)", store.IngressPayloadChallenges)
	got = sy.until(time.Second, "no challenge", func(t *wire.Table) bool { return t.Challenges != nil })
	if len(got.Challenges.Items) != 0 || len(sy.state.Challenges) != 0 {
		t.Fatalf("delta %+v", got)
	}
}
