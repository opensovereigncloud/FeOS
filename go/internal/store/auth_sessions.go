// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// This file holds the typed queries of human sign-in (internal/api, §11
// "Human sign-in"): the sign-ins under way (auth_logins) and the UI
// sessions (ui_sessions), migration 0002.

// LoginTTL is how long a sign-in may take from its start to the callback.
const LoginTTL = 10 * time.Minute

// sessionSeenInterval is how often a session's last_seen_at is written at
// most.
const sessionSeenInterval = "1 minute"

// AuthLogin is a row of auth_logins: a sign-in under way.
type AuthLogin struct {
	StateHash string // sha256 hex of the state
	Verifier  string // the PKCE code verifier
	Nonce     string
	Next      string // the local path to return to
	CreatedAt time.Time
}

// InsertAuthLogin records a sign-in under way.
func (s *Store) InsertAuthLogin(ctx context.Context, l AuthLogin) error {
	_, err := s.q.Exec(ctx, `INSERT INTO auth_logins (state_hash, verifier, nonce, next) VALUES ($1, $2, $3, $4)`,
		l.StateHash, l.Verifier, l.Nonce, l.Next)
	return err
}

// TakeAuthLogin deletes and returns the sign-in of stateHash begun less
// than LoginTTL ago: a sign-in completes once. ErrNoRow when there is
// none (unknown, used or expired).
func (s *Store) TakeAuthLogin(ctx context.Context, stateHash string) (AuthLogin, error) {
	var l AuthLogin
	err := s.q.QueryRow(ctx, `DELETE FROM auth_logins WHERE state_hash = $1 AND created_at > now() - $2::interval
		RETURNING state_hash, verifier, nonce, next, created_at`, stateHash, interval(LoginTTL)).
		Scan(&l.StateHash, &l.Verifier, &l.Nonce, &l.Next, &l.CreatedAt)
	return l, noRow(err, ErrNoRow)
}

// DeleteStaleAuthLogins removes the sign-ins begun LoginTTL ago or
// earlier. It returns how many.
func (s *Store) DeleteStaleAuthLogins(ctx context.Context) (int64, error) {
	tag, err := s.q.Exec(ctx, "DELETE FROM auth_logins WHERE created_at <= now() - $1::interval", interval(LoginTTL))
	return tag.RowsAffected(), err
}

// UISession is a row of ui_sessions: a signed-in person.
type UISession struct {
	IDHash     string // sha256 hex of the session id
	Sub        string // the provider's stable user id
	Email      string
	Name       string
	Username   string
	Picture    string
	Groups     []string
	Admin      bool
	IDToken    string // the ID token, the sign-out's id_token_hint
	CreatedAt  time.Time
	ExpiresAt  time.Time
	LastSeenAt time.Time
}

const uiSessionCols = `id_hash, sub, email, name, username, picture, groups, admin, id_token, created_at, expires_at,
	last_seen_at`

func scanUISession(row pgx.Row) (UISession, error) {
	var u UISession
	err := row.Scan(&u.IDHash, &u.Sub, &u.Email, &u.Name, &u.Username, &u.Picture, &u.Groups, &u.Admin, &u.IDToken,
		&u.CreatedAt, &u.ExpiresAt, &u.LastSeenAt)
	u.Groups = nonNil(u.Groups)
	return u, err
}

// InsertUISession stores a session that expires ttl from now() (the
// fields of u that the database sets are ignored) and returns it as
// stored.
func (s *Store) InsertUISession(ctx context.Context, u UISession, ttl time.Duration) (UISession, error) {
	return scanUISession(s.q.QueryRow(ctx, `INSERT INTO ui_sessions (id_hash, sub, email, name, username, picture, groups,
			admin, id_token, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, now() + $10::interval) RETURNING `+uiSessionCols,
		u.IDHash, u.Sub, u.Email, u.Name, u.Username, u.Picture, nonNil(u.Groups), u.Admin, u.IDToken, interval(ttl)))
}

// AdminSession returns the session of idHash when it has not expired and
// is an admin's; ErrNoRow when there is none.
func (s *Store) AdminSession(ctx context.Context, idHash string) (UISession, error) {
	u, err := scanUISession(s.q.QueryRow(ctx, "SELECT "+uiSessionCols+` FROM ui_sessions
		WHERE id_hash = $1 AND expires_at > now() AND admin`, idHash))
	return u, noRow(err, ErrNoRow)
}

// TouchUISession sets a session's last_seen_at = now(), at most once a
// minute.
func (s *Store) TouchUISession(ctx context.Context, idHash string) error {
	_, err := s.q.Exec(ctx, `UPDATE ui_sessions SET last_seen_at = now() WHERE id_hash = $1
		AND last_seen_at < now() - interval '`+sessionSeenInterval+`'`, idHash)
	return err
}

// DeleteUISession removes a session (a sign-out) and returns it. ErrNoRow
// when there was none.
func (s *Store) DeleteUISession(ctx context.Context, idHash string) (UISession, error) {
	u, err := scanUISession(s.q.QueryRow(ctx, "DELETE FROM ui_sessions WHERE id_hash = $1 RETURNING "+uiSessionCols, idHash))
	if errors.Is(err, pgx.ErrNoRows) {
		return UISession{}, ErrNoRow
	}
	return u, err
}

// DeleteExpiredUISessions removes the sessions past their expiry. It
// returns how many.
func (s *Store) DeleteExpiredUISessions(ctx context.Context) (int64, error) {
	tag, err := s.q.Exec(ctx, "DELETE FROM ui_sessions WHERE expires_at <= now()")
	return tag.RowsAffected(), err
}
