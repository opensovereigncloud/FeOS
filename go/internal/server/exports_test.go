// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/gardenlinux/feos/go/internal/db/dbtest"
	"github.com/gardenlinux/feos/go/internal/env"
	"github.com/gardenlinux/feos/go/internal/ports"
	"github.com/gardenlinux/feos/go/internal/seal"
)

// newEnvironments takes the port door's exports and the sealer by type:
// a signature that drifts from env's interfaces fails here, not silently
// at runtime.
var (
	_ env.Ports  = (*ports.Handler)(nil)
	_ env.Sealer = (*seal.Sealer)(nil)
)

// The environments seal their secrets under --secret-key-file's key, and
// refuse them without one.
func TestEnvironmentsSealer(t *testing.T) {
	caDir := testCA(t)
	without, err := New(context.Background(), testConfig(dbtest.NewDatabase(t), caDir), dbtest.Logger(t))
	if err != nil {
		t.Fatal(err)
	}
	defer without.Close()
	if without.Env.SecretsEnabled() {
		t.Fatal("secrets without --secret-key-file")
	}
	keyFile := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(keyFile, []byte(hex.EncodeToString(make([]byte, 32))), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := testConfig(dbtest.NewDatabase(t), caDir)
	cfg.SecretKeyFile = keyFile
	with, err := New(context.Background(), cfg, dbtest.Logger(t))
	if err != nil {
		t.Fatal(err)
	}
	defer with.Close()
	if !with.Env.SecretsEnabled() {
		t.Fatal("no secrets with --secret-key-file")
	}
}
