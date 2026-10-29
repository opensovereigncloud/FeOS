-- SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
-- SPDX-License-Identifier: Apache-2.0

-- The certificates of domains (ADR 0021): ferund is the ACME client.
-- domain_certs holds a domain's current certificate, its key sealed
-- under --secret-key-file (additional data: 'domain-cert:' || domain_id);
-- acme_accounts the platform's account per directory; acme_challenges the
-- HTTP-01 answers every ingress instance serves on port 80 while an order
-- is pending; ingresses.challenge_tokens the tokens an instance's Sync
-- stream sent it last, so that an order accepts its challenge only once
-- every connected instance holds it. The columns of domains under
-- "issuing" are the orders' schedule and their last failure.
--
-- 0015, not 0013: the branch private-images holds 0013 and 0014; the
-- migrator applies every version it has not, in order, so either may
-- come first.
--
-- Additive: an instance of the previous version, which a rolling deploy
-- runs beside this one, never reads the new tables nor the new columns,
-- which have defaults; the incident conditions it writes stay allowed.

ALTER TABLE domains
  ADD COLUMN next_issue_at timestamptz,                    -- the next order may start then; NULL: now
  ADD COLUMN issue_backoff_s integer NOT NULL DEFAULT 0 CHECK (issue_backoff_s >= 0),   -- 0: no failure; else 3600 s doubling to the cap
  ADD COLUMN issue_error text NOT NULL DEFAULT '',         -- the last order's failure
  ADD COLUMN issue_failing_since timestamptz;              -- orders (or renewals) have failed since

CREATE TABLE domain_certs (                    -- a domain's current certificate; replaced on renewal
  domain_id uuid PRIMARY KEY REFERENCES domains(id) ON DELETE CASCADE,
  chain bytea[] NOT NULL CHECK (cardinality(chain) >= 1),   -- DER, leaf first
  key_sealed bytea NOT NULL,                   -- PKCS#8 DER, sealed with aad 'domain-cert:' || domain_id
  not_before timestamptz NOT NULL,
  not_after timestamptz NOT NULL,
  renew_after timestamptz NOT NULL,            -- half its lifetime
  issued_at timestamptz NOT NULL DEFAULT now(),
  serial text NOT NULL);                       -- lowercase hex

CREATE TABLE acme_accounts (                   -- the platform's ACME account, one per directory
  directory text PRIMARY KEY,                  -- the directory URL
  key_sealed bytea NOT NULL,                   -- the account key, PKCS#8 DER, sealed with aad 'acme-account:' || directory
  uri text NOT NULL DEFAULT '',                -- the account's URL at the CA
  email text NOT NULL DEFAULT '',
  created_at timestamptz NOT NULL DEFAULT now());

CREATE TABLE acme_challenges (                 -- HTTP-01 answers the ingress serves on port 80
  token text PRIMARY KEY CHECK (token ~ '^[A-Za-z0-9_-]+$' AND length(token) <= 256),   -- base64url; PostgreSQL's regular expressions repeat at most 255 times
  host text NOT NULL,                          -- the domain's name: the answer is given for this host alone
  key_auth text NOT NULL,                      -- the body of the answer
  domain_id uuid REFERENCES domains(id) ON DELETE CASCADE,
  created_at timestamptz NOT NULL DEFAULT now(),
  expires_at timestamptz NOT NULL);            -- gone from the table after this, whatever the order did
CREATE INDEX acme_challenges_expires ON acme_challenges(expires_at);

ALTER TABLE ingresses ADD COLUMN challenge_tokens text[] NOT NULL DEFAULT '{}';

ALTER TABLE env_incidents DROP CONSTRAINT env_incidents_condition_check;
ALTER TABLE env_incidents ADD CONSTRAINT env_incidents_condition_check
  CHECK (condition IN ('rollout_failed','crash_loop','job_failed','no_capacity',
     'addon_unhealthy','volume_unavailable','attach_failed','names_unserved',
     'domain_record_wrong','domain_deactivated','domain_superseded','certificate_failing'));
