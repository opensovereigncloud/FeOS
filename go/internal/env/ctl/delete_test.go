// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package ctl

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ironcore-dev/feos/go/internal/store"
)

// Environments whose deletion has to wait (here: a volume on a node that
// is down, not removed) go to the end of env-delete's queue, so that
// more of them than a pass takes never hold back an environment deleted
// after them; once the node is removed, they go too.
func TestDeleteRotatesWaitingEnvironments(t *testing.T) {
	h := newHarness(t, options{})
	if _, err := h.st.InsertNode(bg, "down"); err != nil {
		t.Fatal(err)
	}
	var stuck []store.Environment
	for range deleteBatch {
		e := h.newEnv("stuck-" + uuid.NewString()[:8])
		if _, err := h.st.InsertEnvVolume(bg, store.EnvVolume{EnvID: e.ID, Object: "addon/pg", VolumeID: uuid.New(),
			SizeMiB: 1024, Node: "down", Provisioned: true, AddonType: "postgres"}); err != nil {
			t.Fatal(err)
		}
		stuck = append(stuck, e)
	}
	last := h.newEnv("last")
	h.exec(`UPDATE environments SET deleting = true`)
	eventually(t, 30*time.Second, "the environment deleted after the waiting ones", func() bool {
		h.c.notify(bg, LoopDelete)
		_, err := h.st.GetEnvironment(bg, last.ID)
		return errors.Is(err, store.ErrNoRow)
	})
	for _, e := range stuck {
		if cur, err := h.st.GetEnvironment(bg, e.ID); err != nil || !cur.Deleting {
			t.Fatalf("a waiting environment %+v %v", cur, err)
		}
	}
	h.exec(`UPDATE nodes SET status = 'removed' WHERE name = 'down'`)
	eventually(t, 60*time.Second, "the waiting environments deleted", func() bool {
		h.c.notify(bg, LoopDelete)
		for _, e := range stuck {
			if _, err := h.st.GetEnvironment(bg, e.ID); !errors.Is(err, store.ErrNoRow) {
				return false
			}
		}
		return true
	})
}
