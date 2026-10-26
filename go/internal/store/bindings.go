// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ironcore-dev/feos/go/internal/model"
	"github.com/ironcore-dev/feos/go/internal/podid"
)

// KubeBinding is a row of kube_bindings: a Kubernetes API a sandbox
// reaches through its node, with the credential sealed at rest.
type KubeBinding struct {
	Name        string
	Port        int
	Server      string
	ServerName  string
	CAPEM       string
	AuthType    string // token, client_cert
	Sealed      []byte // 0x01 || nonce(12) || AES-256-GCM(credential JSON)
	Fingerprint string // "sha256:<16 hex>"
	CreatedAt   time.Time
}

// KubeBindings returns a sandbox's Kubernetes set: its version and its
// bindings by port, read in one statement; model.ErrNotFound when the
// sandbox does not exist.
func (s *Store) KubeBindings(ctx context.Context, sandboxID podid.ID) (string, []KubeBinding, error) {
	rows, err := s.q.Query(ctx, `SELECT s.kube_version, b.name, b.port, b.server, b.server_name, b.ca_pem, b.auth_type,
			b.sealed, b.fingerprint, b.created_at
		FROM sandboxes s LEFT JOIN kube_bindings b ON b.sandbox_id = s.id WHERE s.id = $1 ORDER BY b.port`, sandboxID)
	if err != nil {
		return "", nil, err
	}
	defer rows.Close()
	var version string
	found := false
	bs := []KubeBinding{}
	for rows.Next() {
		found = true
		var name, server, serverName, ca, authType, fingerprint *string
		var port *int
		var sealed []byte
		var created *time.Time
		if err := rows.Scan(&version, &name, &port, &server, &serverName, &ca, &authType, &sealed, &fingerprint, &created); err != nil {
			return "", nil, err
		}
		if name == nil {
			continue
		}
		bs = append(bs, KubeBinding{Name: *name, Port: *port, Server: *server, ServerName: *serverName, CAPEM: *ca,
			AuthType: *authType, Sealed: sealed, Fingerprint: *fingerprint, CreatedAt: timeOf(created)})
	}
	if err := rows.Err(); err != nil {
		return "", nil, err
	}
	if !found {
		return "", nil, model.ErrNotFound
	}
	return version, bs, nil
}

// SetKubeBindings replaces a sandbox's Kubernetes set with bs and version
// ("" and none to delete it); model.ErrNotFound when the sandbox does not
// exist. The caller holds the sandbox's row FOR UPDATE.
func (s *Store) SetKubeBindings(ctx context.Context, sandboxID podid.ID, version string, bs []KubeBinding) error {
	tag, err := s.q.Exec(ctx, "UPDATE sandboxes SET kube_version = $2 WHERE id = $1", sandboxID, version)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return model.ErrNotFound
	}
	if _, err := s.q.Exec(ctx, "DELETE FROM kube_bindings WHERE sandbox_id = $1", sandboxID); err != nil {
		return err
	}
	for _, b := range bs {
		if _, err := s.q.Exec(ctx, `INSERT INTO kube_bindings (sandbox_id, name, port, server, server_name, ca_pem,
				auth_type, sealed, fingerprint) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
			sandboxID, b.Name, b.Port, b.Server, b.ServerName, b.CAPEM, b.AuthType, b.Sealed, b.Fingerprint); err != nil {
			return err
		}
	}
	return nil
}

// KubePorts returns the ports of a sandbox's Kubernetes bindings, which are
// never forwarded.
func (s *Store) KubePorts(ctx context.Context, sandboxID podid.ID) ([]int, error) {
	rows, err := s.q.Query(ctx, "SELECT port FROM kube_bindings WHERE sandbox_id = $1 ORDER BY port", sandboxID)
	if err != nil {
		return nil, err
	}
	ports, err := pgx.CollectRows(rows, pgx.RowTo[int32])
	if err != nil {
		return nil, err
	}
	out := make([]int, len(ports))
	for i, p := range ports {
		out[i] = int(p)
	}
	return out, nil
}

// SSHBinding is a row of ssh_bindings: an SSH identity a sandbox uses
// through its node, with the private key sealed at rest.
type SSHBinding struct {
	Name        string
	Server      string // ssh://host[:port]; "" = any server
	User        string
	HostKeys    []string // "<type> <base64>"
	AuthType    string   // private_key, generate
	KeyType     string
	Fingerprint string // "SHA256:..."
	PublicKey   string
	Sealed      []byte // 0x01 || nonce(12) || AES-256-GCM({"private_key":...})
	CreatedAt   time.Time
}

// SSHBindings returns a sandbox's SSH set: its version and its bindings by
// name, read in one statement; model.ErrNotFound when the sandbox does not
// exist.
func (s *Store) SSHBindings(ctx context.Context, sandboxID podid.ID) (string, []SSHBinding, error) {
	rows, err := s.q.Query(ctx, `SELECT s.ssh_version, b.name, b.server, b.ssh_user, b.host_keys, b.auth_type, b.key_type,
			b.fingerprint, b.public_key, b.sealed, b.created_at
		FROM sandboxes s LEFT JOIN ssh_bindings b ON b.sandbox_id = s.id WHERE s.id = $1 ORDER BY b.name`, sandboxID)
	if err != nil {
		return "", nil, err
	}
	defer rows.Close()
	var version string
	found := false
	bs := []SSHBinding{}
	for rows.Next() {
		found = true
		var name, server, user, authType, keyType, fingerprint, publicKey *string
		var hostKeys []string
		var sealed []byte
		var created *time.Time
		if err := rows.Scan(&version, &name, &server, &user, &hostKeys, &authType, &keyType, &fingerprint, &publicKey,
			&sealed, &created); err != nil {
			return "", nil, err
		}
		if name == nil {
			continue
		}
		bs = append(bs, SSHBinding{Name: *name, Server: *server, User: *user, HostKeys: nonNil(hostKeys),
			AuthType: *authType, KeyType: *keyType, Fingerprint: *fingerprint, PublicKey: *publicKey, Sealed: sealed,
			CreatedAt: timeOf(created)})
	}
	if err := rows.Err(); err != nil {
		return "", nil, err
	}
	if !found {
		return "", nil, model.ErrNotFound
	}
	return version, bs, nil
}

// SetSSHBindings replaces a sandbox's SSH set with bs and version ("" and
// none to delete it); model.ErrNotFound when the sandbox does not exist.
// The caller holds the sandbox's row FOR UPDATE.
func (s *Store) SetSSHBindings(ctx context.Context, sandboxID podid.ID, version string, bs []SSHBinding) error {
	tag, err := s.q.Exec(ctx, "UPDATE sandboxes SET ssh_version = $2 WHERE id = $1", sandboxID, version)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return model.ErrNotFound
	}
	if _, err := s.q.Exec(ctx, "DELETE FROM ssh_bindings WHERE sandbox_id = $1", sandboxID); err != nil {
		return err
	}
	for _, b := range bs {
		if _, err := s.q.Exec(ctx, `INSERT INTO ssh_bindings (sandbox_id, name, server, ssh_user, host_keys, auth_type,
				key_type, fingerprint, public_key, sealed) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
			sandboxID, b.Name, b.Server, b.User, nonNil(b.HostKeys), b.AuthType, b.KeyType, b.Fingerprint, b.PublicKey,
			b.Sealed); err != nil {
			return err
		}
	}
	return nil
}
