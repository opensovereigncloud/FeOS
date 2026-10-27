// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package leader

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/gardenlinux/feos/go/internal/db"
	"github.com/gardenlinux/feos/go/internal/store"
)

// HeartbeatInterval is how often an instance writes its row. An instance
// is live while its row was written within store.InstanceTTL (15 s) and it
// is not stopping (§6).
const HeartbeatInterval = 5 * time.Second

// InstanceInfo is what an instance writes about itself into its row.
type InstanceInfo struct {
	// ID is the instance's id, --instance-id.
	ID string
	// Address is its front-door listen address, informational.
	Address string
	// NodeAddress is --node-advertise-address, "host:port" that nodes dial
	// to reach this instance's nodes' port; "" keeps the instance off the
	// lists nodes get.
	NodeAddress string
	Version     string
}

// Heartbeat keeps the instance's row in instances: written at once and
// every 5 s while Run runs, marked stopping at SIGTERM (SetStopping), and
// deleted as the last step of the shutdown (Remove).
type Heartbeat struct {
	st    *store.Store
	info  InstanceInfo
	log   *slog.Logger
	every time.Duration

	// mu serialises the writes, so that no upsert in flight re-creates the
	// row after Remove deleted it.
	mu      sync.Mutex
	removed bool
}

// NewHeartbeat returns the heartbeat of the instance info describes. log
// is the instance's, which names the instance already.
func NewHeartbeat(d *db.DB, info InstanceInfo, log *slog.Logger) *Heartbeat {
	if log == nil {
		log = slog.Default()
	}
	return &Heartbeat{st: store.New(d), info: info, log: log.With("component", "heartbeat"), every: HeartbeatInterval}
}

// Run writes the instance's row (last_seen = now()) at once and every 5 s
// until ctx ends or Remove was called, and returns nil then. A failed write
// is logged and tried again at the next beat.
//
// A stopping instance keeps beating: it stays listed to nodes until its row
// is deleted (§8 "Instances for nodes"), and a row not written for 15 s
// drops off that list. So ctx must outlive §7 steps 1 to 4: it is not the
// context that ends at SIGTERM; Run ends with Remove (step 5).
func (h *Heartbeat) Run(ctx context.Context) error {
	for {
		if done, err := h.beat(ctx); done {
			return nil
		} else if err != nil && ctx.Err() == nil {
			h.log.Warn("cannot write the instance's heartbeat", "err", err)
		}
		if !sleep(ctx, h.every) {
			return nil
		}
	}
}

// beat writes the row once; done reports that the row was removed.
func (h *Heartbeat) beat(ctx context.Context) (done bool, err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.removed {
		return true, nil
	}
	bctx, cancel := context.WithTimeout(ctx, h.every)
	defer cancel()
	return false, h.st.UpsertInstance(bctx, h.storeInfo())
}

func (h *Heartbeat) storeInfo() store.InstanceInfo {
	return store.InstanceInfo{ID: h.info.ID, Address: h.info.Address, NodeAddress: h.info.NodeAddress, Version: h.info.Version}
}

// SetStopping marks the instance stopping from now on (§7 step 1), writing
// its row first if it has none yet. From then on the instance is not live:
// other instances may take over its ops. It stays listed to nodes until
// Remove.
func (h *Heartbeat) SetStopping(ctx context.Context) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.removed {
		return nil
	}
	err := h.st.InTx(ctx, func(tx *store.Store) error {
		if err := tx.UpsertInstance(ctx, h.storeInfo()); err != nil {
			return err
		}
		return tx.SetInstanceStopping(ctx, h.info.ID)
	})
	if err != nil {
		return fmt.Errorf("mark instance %s stopping: %w", h.info.ID, err)
	}
	return nil
}

// Remove deletes the instance's row (§7 step 5) and stops the beats.
func (h *Heartbeat) Remove(ctx context.Context) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.removed = true
	if err := h.st.DeleteInstance(ctx, h.info.ID); err != nil {
		return fmt.Errorf("delete the row of instance %s: %w", h.info.ID, err)
	}
	return nil
}

// OwnerLive reports whether instanceID is live (§6): its row was written
// within the instance TTL and it is not stopping. An op owned by an
// instance that is not live may be taken over.
func OwnerLive(ctx context.Context, q db.Querier, instanceID string) (bool, error) {
	return store.On(q).InstanceLive(ctx, instanceID)
}
