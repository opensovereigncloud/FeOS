// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"bytes"
	"context"
	"errors"
	"net/netip"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/gardenlinux/feos/go/internal/db"
	"github.com/gardenlinux/feos/go/internal/mesh"
	"github.com/gardenlinux/feos/go/internal/model"
)

// The ingress's instances (ADR 0021): ferun-ingress joins with a join token
// of kind model.JoinKindIngress, holds certificates of its own
// (ingress_certs), and is a peer of the pods' mesh with a /64 from the
// nodes' sequence. Its address, <prefix>::1, is a member of the security
// group model.IngressPeersGroup, whose version is
// pod_network.ingress_group_version: bumped under that row's lock in the
// transaction that changes the members, so that versions follow commit
// order. Every writer locks the ingress's row first and pod_network after
// it, as JoinMesh does for a node.
//
// The writers send ferun_ingress (db.ChanIngress) in the writing
// transaction: "ingress:<name>" when an ingress's row or certificates
// changed in a way its connections or the table follow (a join again, a
// removal, a mesh join, a supersession, a revocation), "mesh" with every
// change of the mesh's peers (bumpMeshPeers), and "*" when a node's
// version of model.IngressPeersGroup changed. A change of the group's
// members also wakes the leader's env-groups loop (ferun_leader
// HintEnvGroups).

// The payloads of ferun_ingress (db.ChanIngress) besides IngressEnvPayload
// and IngressNamePayload: everything, and the mesh's peers.
const (
	IngressPayloadAll  = "*"
	IngressPayloadMesh = "mesh"
)

// IngressEnvPayload is the ferun_ingress payload of a change of an
// environment the ingress's table follows: "env:<uuid>".
func IngressEnvPayload(id uuid.UUID) string { return "env:" + id.String() }

// IngressNamePayload is the ferun_ingress payload of a change of ingress
// name's row or certificates: "ingress:<name>".
func IngressNamePayload(name string) string { return "ingress:" + name }

// ErrIngressKeyFixed is JoinIngressMesh's error for a WireGuard key other
// than the one the ingress's row holds: the key is fixed from the first
// Hello after a join, and only a join again (RejoinIngress) clears it.
var ErrIngressKeyFixed = errors.New("the mesh key of the ingress is fixed; join it again to change it")

// ErrIngressKeyRetired is JoinIngressMesh's error for the key that the
// last join again cleared: a join again always brings a new key, so that
// a copy of the instance's state directory does not stay on the mesh.
var ErrIngressKeyRetired = errors.New("the mesh key of the ingress was retired by its join again")

// ErrNotMeshed is the error of a write that needs the pods' mesh on a
// cluster that is not meshed (no pod_network row).
var ErrNotMeshed = errors.New("the cluster is not meshed")

// Ingress is a row of ingresses: an instance of ferun-ingress. A row is
// never deleted: removal is a status.
type Ingress struct {
	Name      string
	Status    string // model.IngressJoined, model.IngressRemoved
	JoinedAt  time.Time
	RemovedAt time.Time

	// The instance on the pods' mesh: its subnet ID (0: none yet), its /64
	// and its address there, <prefix>::1 (both "" without a subnet), and
	// its WireGuard key (nil: none since the last join) and endpoint.
	MeshSubnet    int
	MeshPrefix    string
	MeshAddress   string
	MeshPublicKey []byte
	MeshEndpoint  string
	// PreviousMeshPublicKey is the key the last join again cleared (nil:
	// none), which the next Hello may not bring back.
	PreviousMeshPublicKey []byte

	// GroupVersion is pod_network.ingress_group_version when its address
	// last entered or left model.IngressPeersGroup: a node that holds the
	// group at this version or later lets it in.
	GroupVersion int64

	CurrentSerial string // the newest of its certificates presented in a Hello
	Version       string // ferun-ingress's version, from Hello
	LastSeen      time.Time
	LeftAt        time.Time // it stopped cleanly and was not seen since
}

// MeshPeer reports whether the ingress is a peer of the mesh: it has its
// /64, a key and an endpoint. A removed ingress is nobody's peer, whatever
// this says.
func (g *Ingress) MeshPeer() bool {
	return g.MeshSubnet != 0 && len(g.MeshPublicKey) == mesh.KeyLen && g.MeshEndpoint != ""
}

// member reports whether the ingress's address is a member of
// model.IngressPeersGroup: it is not removed and has an address.
func (g *Ingress) member() bool {
	return g.Status != model.IngressRemoved && g.MeshAddress != ""
}

const ingressCols = `name, status, joined_at, removed_at, mesh_subnet, mesh_prefix, mesh_address, mesh_public_key,
	mesh_endpoint, previous_mesh_public_key, group_version, current_serial, version, last_seen, left_at`

func scanIngress(row pgx.Row) (Ingress, error) {
	var g Ingress
	var removed, seen, left *time.Time
	var subnet *int32
	if err := row.Scan(&g.Name, &g.Status, &g.JoinedAt, &removed, &subnet, &g.MeshPrefix, &g.MeshAddress,
		&g.MeshPublicKey, &g.MeshEndpoint, &g.PreviousMeshPublicKey, &g.GroupVersion, &g.CurrentSerial, &g.Version, &seen,
		&left); err != nil {
		return Ingress{}, err
	}
	if subnet != nil {
		g.MeshSubnet = int(*subnet)
	}
	g.RemovedAt, g.LastSeen, g.LeftAt = timeOf(removed), timeOf(seen), timeOf(left)
	return g, nil
}

// InsertIngress adds an ingress named name with status joined unless the
// name exists. It reports whether it did: an unpinned join token never
// takes over an existing name, whatever the row's status.
func (s *Store) InsertIngress(ctx context.Context, name string) (bool, error) {
	tag, err := s.q.Exec(ctx, "INSERT INTO ingresses (name) VALUES ($1) ON CONFLICT DO NOTHING", name)
	return tag.RowsAffected() > 0, err
}

// GetIngress reads an ingress; ErrNoRow if there is none.
func (s *Store) GetIngress(ctx context.Context, name string) (Ingress, error) {
	return s.LockIngress(ctx, name, NoLock)
}

// LockIngress reads an ingress and locks its row; ErrNoRow if there is
// none.
func (s *Store) LockIngress(ctx context.Context, name string, lock RowLock) (Ingress, error) {
	g, err := scanIngress(s.q.QueryRow(ctx, "SELECT "+ingressCols+" FROM ingresses WHERE name = $1 "+string(lock), name))
	return g, noRow(err, ErrNoRow)
}

// ListIngresses returns every ingress, removed ones included, ordered by
// name.
func (s *Store) ListIngresses(ctx context.Context) ([]Ingress, error) {
	rows, err := s.q.Query(ctx, "SELECT "+ingressCols+" FROM ingresses ORDER BY name")
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Ingress, error) { return scanIngress(r) })
}

// bumpIngressGroup counts a change of model.IngressPeersGroup's members,
// under pod_network's row lock, and wakes the leader's env-groups loop; it
// returns the new version. The caller locked the ingress's row before, and
// commits the change of the members in the same transaction, so that the
// versions follow commit order. ErrNotMeshed when the cluster is not
// meshed, which an ingress with an address never meets.
func (s *Store) bumpIngressGroup(ctx context.Context) (int64, error) {
	var v int64
	err := s.q.QueryRow(ctx, `UPDATE pod_network SET ingress_group_version = ingress_group_version + 1
		RETURNING ingress_group_version`).Scan(&v)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrNotMeshed
	}
	if err != nil {
		return 0, err
	}
	return v, s.Notify(ctx, db.ChanLeader, HintEnvGroups)
}

// RejoinIngress resets an ingress for a join with a token pinned to its
// name: the row (inserted if missing, then locked) becomes joined and not
// removed, without a WireGuard key, endpoint or current serial; it keeps
// its subnet, prefix and address. The key it held becomes its previous
// one, which its next Hello may not bring back (a join again without a
// key since keeps the previous one). A removed ingress with an address
// re-enters model.IngressPeersGroup (its group version is bumped). Every
// earlier certificate is revoked ("rejoined"); an ingress that was a peer
// of the mesh stops being one until its next Hello; ferun_ingress
// "ingress:<name>" is sent. It runs in a transaction of its own unless the
// Store is in one.
func (s *Store) RejoinIngress(ctx context.Context, name string) error {
	return s.InTx(ctx, func(tx *Store) error {
		if _, err := tx.InsertIngress(ctx, name); err != nil {
			return err
		}
		g, err := tx.LockIngress(ctx, name, ForUpdate)
		if err != nil {
			return err
		}
		version := g.GroupVersion
		if g.Status == model.IngressRemoved && g.MeshAddress != "" {
			if version, err = tx.bumpIngressGroup(ctx); err != nil {
				return err
			}
		}
		if _, err := tx.q.Exec(ctx, `UPDATE ingresses SET status = 'joined', removed_at = NULL,
			previous_mesh_public_key = coalesce(mesh_public_key, previous_mesh_public_key), mesh_public_key = NULL,
			mesh_endpoint = '', current_serial = '', group_version = $2 WHERE name = $1`, name, version); err != nil {
			return err
		}
		if _, err := tx.RevokeIngressCerts(ctx, name, RevokedRejoined); err != nil {
			return err
		}
		if g.MeshPeer() && g.Status != model.IngressRemoved {
			if _, err := tx.bumpMeshPeers(ctx); err != nil {
				return err
			}
		}
		return tx.Notify(ctx, db.ChanIngress, IngressNamePayload(name))
	})
}

// RemoveIngress marks an ingress removed (status, removed_at) and revokes
// every certificate of it ("removed"). An ingress that was not removed yet
// leaves model.IngressPeersGroup (its group version is bumped, when it had
// an address) and stops being a peer of the mesh; its /64 is never given
// out again. ferun_ingress "ingress:<name>" is sent. It reports whether
// the ingress exists. It runs in a transaction of its own unless the Store
// is in one.
func (s *Store) RemoveIngress(ctx context.Context, name string) (bool, error) {
	found := false
	err := s.InTx(ctx, func(tx *Store) error {
		found = false
		g, err := tx.LockIngress(ctx, name, ForUpdate)
		if errors.Is(err, ErrNoRow) {
			return nil
		}
		if err != nil {
			return err
		}
		found = true
		version := g.GroupVersion
		if g.member() {
			if version, err = tx.bumpIngressGroup(ctx); err != nil {
				return err
			}
		}
		if _, err := tx.q.Exec(ctx, `UPDATE ingresses SET status = 'removed', removed_at = coalesce(removed_at, now()),
			group_version = $2 WHERE name = $1`, name, version); err != nil {
			return err
		}
		if _, err := tx.RevokeIngressCerts(ctx, name, RevokedRemoved); err != nil {
			return err
		}
		if g.MeshPeer() && g.Status != model.IngressRemoved {
			if _, err := tx.bumpMeshPeers(ctx); err != nil {
				return err
			}
		}
		return tx.Notify(ctx, db.ChanIngress, IngressNamePayload(name))
	})
	return found, err
}

// IngressMeshJoin is what JoinIngressMesh made of an ingress: its subnet
// ID, /64 and address; Changed when its subnet, prefix, key or endpoint
// changed, which bumped the peers' version; AddressChanged when its
// address entered model.IngressPeersGroup (or moved), which bumped the
// group's version.
type IngressMeshJoin struct {
	Subnet         int
	Prefix         string
	Address        string
	Changed        bool
	AddressChanged bool
}

// JoinIngressMesh puts ingress in on the mesh of ula with its WireGuard
// key and endpoint (both checked by the caller). The key is fixed: a key
// other than the row's is ErrIngressKeyFixed while the row holds one.
// While it holds none (after a join), the key that the last join again
// cleared is ErrIngressKeyRetired, and a key that a node or another
// ingress holds is ErrMeshKeyTaken (checkMeshKey). An
// ingress without a subnet ID gets the next one of the nodes' sequence
// (ErrMeshExhausted when none is left); its prefix is the subnet's /64 of
// ula and its address the prefix's mesh.HostAddress. When its address
// enters model.IngressPeersGroup, the group's version goes up and becomes
// the row's group_version. When the subnet, prefix, key or endpoint
// changed, the peers' version goes up and the leader is woken
// (HintMeshPeers); on any change ferun_ingress "ingress:<name>" is sent.
// The caller holds in's row FOR UPDATE, read in the same transaction, and
// has refused a removed ingress.
func (s *Store) JoinIngressMesh(ctx context.Context, in Ingress, ula netip.Prefix, key []byte, endpoint string) (IngressMeshJoin, error) {
	if !ula.IsValid() {
		return IngressMeshJoin{}, ErrNotMeshed
	}
	if in.MeshPublicKey != nil && !bytes.Equal(key, in.MeshPublicKey) {
		return IngressMeshJoin{}, ErrIngressKeyFixed
	}
	if in.MeshPublicKey == nil {
		if in.PreviousMeshPublicKey != nil && bytes.Equal(key, in.PreviousMeshPublicKey) {
			return IngressMeshJoin{}, ErrIngressKeyRetired
		}
		if err := s.checkMeshKey(ctx, key, "", in.Name); err != nil {
			return IngressMeshJoin{}, err
		}
	}
	j := IngressMeshJoin{Subnet: in.MeshSubnet}
	if j.Subnet == 0 {
		next, err := s.nextMeshSubnet(ctx)
		if err != nil {
			return IngressMeshJoin{}, err
		}
		j.Subnet = next
	}
	// The prefix and address follow from ula and the subnet; they differ
	// from the row's only for a new subnet, or after the ULA was changed
	// by hand.
	prefix := mesh.NodePrefix(ula, j.Subnet)
	j.Prefix, j.Address = prefix.String(), mesh.HostAddress(prefix).String()
	j.AddressChanged = j.Address != in.MeshAddress
	j.Changed = j.Subnet != in.MeshSubnet || j.Prefix != in.MeshPrefix || !bytes.Equal(key, in.MeshPublicKey) ||
		endpoint != in.MeshEndpoint
	if !j.Changed && !j.AddressChanged {
		return j, nil
	}
	version := in.GroupVersion
	if j.AddressChanged && in.Status != model.IngressRemoved {
		var err error
		if version, err = s.bumpIngressGroup(ctx); err != nil {
			return IngressMeshJoin{}, err
		}
	}
	if _, err := s.q.Exec(ctx, `UPDATE ingresses SET mesh_subnet = $2, mesh_prefix = $3, mesh_address = $4,
		mesh_public_key = $5, mesh_endpoint = $6, group_version = $7 WHERE name = $1`,
		in.Name, j.Subnet, j.Prefix, j.Address, key, endpoint, version); err != nil {
		return IngressMeshJoin{}, err
	}
	if j.Changed && in.Status != model.IngressRemoved {
		if _, err := s.bumpMeshPeers(ctx); err != nil {
			return IngressMeshJoin{}, err
		}
	}
	return j, s.Notify(ctx, db.ChanIngress, IngressNamePayload(in.Name))
}

// ingressCertNewer is the condition under which the certificate c of
// ingress g was issued after the ingress's current certificate, or the
// ingress has none.
const ingressCertNewer = `g.current_serial <> c.serial AND coalesce(
	c.issued_at > (SELECT cur.issued_at FROM ingress_certs cur WHERE cur.serial = g.current_serial AND cur.ingress = g.name),
	true)`

// PresentIngressCert records that ingress name presented the certificate
// serial in a Hello that passed the ingress's check: with the ingress's
// row locked, if the certificate was issued after the current one (or
// there is none), it becomes current, and every older unrevoked
// certificate of the ingress is revoked one grace ahead ("superseded");
// ferun_ingress "ingress:<name>" is sent when it superseded any, so that
// every instance re-checks the ingress's connections. It returns how many
// it superseded; an older certificate supersedes nothing. ErrNoRow when the
// ingress or the certificate does not exist. It runs in a transaction of
// its own unless the Store is in one.
func (s *Store) PresentIngressCert(ctx context.Context, name, serial string, grace time.Duration) (int64, error) {
	var superseded int64
	err := s.InTx(ctx, func(tx *Store) error {
		superseded = 0
		if _, err := tx.LockIngress(ctx, name, ForUpdate); err != nil {
			return err
		}
		var newer bool
		var issuedAt time.Time
		err := tx.q.QueryRow(ctx, `SELECT c.issued_at, `+ingressCertNewer+`
			FROM ingress_certs c JOIN ingresses g ON g.name = c.ingress WHERE c.serial = $1 AND c.ingress = $2`,
			serial, name).Scan(&issuedAt, &newer)
		if err != nil {
			return noRow(err, ErrNoRow)
		}
		if !newer {
			return nil
		}
		if _, err := tx.q.Exec(ctx, "UPDATE ingresses SET current_serial = $2 WHERE name = $1", name, serial); err != nil {
			return err
		}
		tag, err := tx.q.Exec(ctx, `UPDATE ingress_certs SET revoked_at = now() + $3::interval, revoked_reason = 'superseded'
			WHERE ingress = $1 AND serial <> $2 AND revoked_at IS NULL AND issued_at < $4`,
			name, serial, interval(grace), issuedAt)
		if err != nil {
			return err
		}
		superseded = tag.RowsAffected()
		if superseded > 0 {
			return tx.Notify(ctx, db.ChanIngress, IngressNamePayload(name))
		}
		return nil
	})
	return superseded, err
}

// SeenIngress records that ingress name was seen (a Hello, a heartbeat on
// Sync, a Hold call): last_seen = now(), left_at cleared, and its version
// unless version is "". ErrNoRow if there is no such ingress.
func (s *Store) SeenIngress(ctx context.Context, name, version string) error {
	tag, err := s.q.Exec(ctx, `UPDATE ingresses SET last_seen = now(), left_at = NULL,
		version = CASE WHEN $2::text = '' THEN version ELSE $2::text END WHERE name = $1`, name, version)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNoRow
	}
	return err
}

// IngressGroup returns model.IngressPeersGroup at its version, in one
// statement: the mesh addresses of the ingresses that are joined and have
// one, sorted. A cluster that is not meshed has none, at version 0.
func (s *Store) IngressGroup(ctx context.Context) (members []string, version int64, err error) {
	err = s.q.QueryRow(ctx, `SELECT p.ingress_group_version, array(SELECT g.mesh_address FROM ingresses g
			WHERE g.status = 'joined' AND g.mesh_address <> '' ORDER BY 1)
		FROM pod_network p`).Scan(&version, &members)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, err
	}
	return members, version, nil
}

// IngressCert is a row of ingress_certs: one ingress certificate issued.
// Its kinds (CertJoin, CertRejoin, CertRenew) and revocation reasons
// (RevokedRejoined, RevokedSuperseded, RevokedRemoved) are a node
// certificate's.
type IngressCert struct {
	Serial         string // lowercase hex
	Ingress        string
	Kind           string // CertJoin, CertRejoin, CertRenew
	KeyHash        string // sha256 hex of the key's SPKI DER
	PreviousSerial string // renew: the certificate that asked
	TokenID        uuid.UUID
	Peer           string // remote address of the issuing call
	NotBefore      time.Time
	NotAfter       time.Time
	IssuedAt       time.Time
	RevokedAt      time.Time // may lie ahead: superseded, in its grace
	RevokedReason  string
}

const ingressCertCols = `serial, ingress, kind, key_hash, previous_serial, token_id, peer, not_before, not_after,
	issued_at, revoked_at, revoked_reason`

func scanIngressCert(row pgx.Row) (IngressCert, error) {
	var c IngressCert
	var tokenID *uuid.UUID
	var revoked *time.Time
	if err := row.Scan(&c.Serial, &c.Ingress, &c.Kind, &c.KeyHash, &c.PreviousSerial, &tokenID, &c.Peer,
		&c.NotBefore, &c.NotAfter, &c.IssuedAt, &revoked, &c.RevokedReason); err != nil {
		return IngressCert{}, err
	}
	c.TokenID, c.RevokedAt = uuidOf(tokenID), timeOf(revoked)
	return c, nil
}

// InsertIngressCert records an issued ingress certificate (issued_at =
// now()).
func (s *Store) InsertIngressCert(ctx context.Context, c IngressCert) (IngressCert, error) {
	return scanIngressCert(s.q.QueryRow(ctx, `INSERT INTO ingress_certs (serial, ingress, kind, key_hash,
			previous_serial, token_id, peer, not_before, not_after)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) RETURNING `+ingressCertCols,
		c.Serial, c.Ingress, c.Kind, c.KeyHash, c.PreviousSerial, nullUUID(c.TokenID), c.Peer, c.NotBefore, c.NotAfter))
}

// GetIngressCert reads an ingress certificate by serial; ErrNoRow if none.
func (s *Store) GetIngressCert(ctx context.Context, serial string) (IngressCert, error) {
	c, err := scanIngressCert(s.q.QueryRow(ctx, "SELECT "+ingressCertCols+" FROM ingress_certs WHERE serial = $1", serial))
	return c, noRow(err, ErrNoRow)
}

// ListIngressCerts returns an ingress's certificates, newest first.
func (s *Store) ListIngressCerts(ctx context.Context, name string) ([]IngressCert, error) {
	rows, err := s.q.Query(ctx, "SELECT "+ingressCertCols+" FROM ingress_certs WHERE ingress = $1 ORDER BY issued_at DESC, serial",
		name)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (IngressCert, error) { return scanIngressCert(r) })
}

// NewestIngressCert returns the serial of ingress name's newest
// certificate that is not revoked, not even from a time ahead; "" when it
// has none.
func (s *Store) NewestIngressCert(ctx context.Context, name string) (string, error) {
	var serial string
	err := s.q.QueryRow(ctx, `SELECT serial FROM ingress_certs WHERE ingress = $1 AND revoked_at IS NULL
		ORDER BY issued_at DESC, serial DESC LIMIT 1`, name).Scan(&serial)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return serial, err
}

// RevokeIngressCerts revokes, from now on, every certificate of ingress
// name that is not revoked yet or whose revocation lies ahead, for reason.
// It returns how many it revoked, and sends ferun_ingress
// "ingress:<name>" when that is any.
func (s *Store) RevokeIngressCerts(ctx context.Context, name, reason string) (int64, error) {
	tag, err := s.q.Exec(ctx, `UPDATE ingress_certs SET revoked_at = now(), revoked_reason = $2
		WHERE ingress = $1 AND (revoked_at IS NULL OR revoked_at > now())`, name, reason)
	if err != nil {
		return 0, err
	}
	if tag.RowsAffected() > 0 {
		if err := s.Notify(ctx, db.ChanIngress, IngressNamePayload(name)); err != nil {
			return 0, err
		}
	}
	return tag.RowsAffected(), nil
}

// IngressCertCheck is what the check of an ingress's connection decides
// on, read in one statement with the database clock.
type IngressCertCheck struct {
	Found         bool   // the serial exists and is the ingress's
	IngressStatus string // model.IngressJoined, model.IngressRemoved
	RevokedAt     time.Time
	RevokedReason string
	NotAfter      time.Time
	Now           time.Time // the database clock of the read
}

// Revoked reports a revocation in effect.
func (c IngressCertCheck) Revoked() bool {
	return !c.RevokedAt.IsZero() && !c.RevokedAt.After(c.Now)
}

// RevokeAhead returns a revocation time that lies ahead (a superseded
// certificate in its grace), or zero.
func (c IngressCertCheck) RevokeAhead() time.Time {
	if c.RevokedAt.After(c.Now) {
		return c.RevokedAt
	}
	return time.Time{}
}

// Expired reports a certificate past its NotAfter on the database clock.
func (c IngressCertCheck) Expired() bool {
	return !c.NotAfter.After(c.Now)
}

// CheckIngressCert reads what decides whether a connection presenting
// serial for ingress name is allowed: the certificate's revocation and
// expiry, the ingress's status, and now(). Found is false for an unknown
// serial or one of another ingress.
func (s *Store) CheckIngressCert(ctx context.Context, name, serial string) (IngressCertCheck, error) {
	var c IngressCertCheck
	var revoked *time.Time
	err := s.q.QueryRow(ctx, `SELECT c.revoked_at, c.revoked_reason, c.not_after, g.status, now()
		FROM ingress_certs c JOIN ingresses g ON g.name = c.ingress WHERE c.serial = $1 AND c.ingress = $2`, serial, name).
		Scan(&revoked, &c.RevokedReason, &c.NotAfter, &c.IngressStatus, &c.Now)
	if errors.Is(err, pgx.ErrNoRows) {
		return IngressCertCheck{}, nil
	}
	if err != nil {
		return IngressCertCheck{}, err
	}
	c.Found = true
	c.RevokedAt = timeOf(revoked)
	return c, nil
}
