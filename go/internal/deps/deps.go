// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//go:build tools

// Package deps names the module's dependencies, so that go.mod and go.sum
// hold them before the code that uses them is written. Changing a
// dependency is a change to this file and go.mod together.
package deps

import (
	_ "github.com/google/uuid"
	_ "github.com/jackc/pgx/v5"
	_ "github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/modelcontextprotocol/go-sdk/mcp"
	_ "github.com/twmb/franz-go/pkg/kadm"
	_ "github.com/twmb/franz-go/pkg/kgo"
	_ "golang.org/x/crypto/acme"
	_ "golang.org/x/crypto/ssh"
	_ "golang.org/x/sync/errgroup"
	_ "google.golang.org/grpc"
	_ "google.golang.org/protobuf/proto"
)
