-- SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
-- SPDX-License-Identifier: Apache-2.0

-- Human sign-in to the UI and the admin API through OIDC (§11 "Human
-- sign-in"). Both tables live in the database, so that a sign-in begun on
-- one instance completes on another behind the balancer, and a sign-out
-- ends the session on every instance. Hashes are the SHA-256 hex of the
-- random values; the raw values live only in the browser's cookies.

CREATE TABLE auth_logins (                     -- a sign-in under way: single use, 10 minutes
  state_hash text PRIMARY KEY,                 -- sha256 hex of the state (cookie ferun_login)
  verifier text NOT NULL,                      -- the PKCE code verifier
  nonce text NOT NULL,
  next text NOT NULL,                          -- the local path to return to
  created_at timestamptz NOT NULL DEFAULT now());

CREATE TABLE ui_sessions (                     -- a signed-in admin (cookie ferun_session)
  id_hash text PRIMARY KEY,                    -- sha256 hex of the session id
  sub text NOT NULL, email text NOT NULL, name text NOT NULL, username text NOT NULL,
  picture text NOT NULL, groups text[] NOT NULL, admin boolean NOT NULL,
  id_token text NOT NULL,                      -- the id_token_hint of the sign-out
  created_at timestamptz NOT NULL DEFAULT now(),
  expires_at timestamptz NOT NULL,             -- absolute: created_at + --session-ttl
  last_seen_at timestamptz NOT NULL DEFAULT now());
CREATE INDEX ui_sessions_sub ON ui_sessions(sub);
CREATE INDEX ui_sessions_expires ON ui_sessions(expires_at);
