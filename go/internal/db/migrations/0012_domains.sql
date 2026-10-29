-- SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
-- SPDX-License-Identifier: Apache-2.0

-- Domains (ADR 0021): a tenant's claim on one name, proven by a TXT record
-- of the claim's token at <--domain-challenge-label>.<name>, never by the
-- record that routes the name. Claims are not exclusive while unverified:
-- one name may have any number of claims, of one tenant or several; at
-- most one is verified (domains_verified_name), the one whose token DNS
-- shows. A claim is bound to at most one environment of its tenant
-- (env_id), by a call apart from the claim; deleting the environment ends
-- the binding and keeps the claim (ON DELETE SET NULL). ferund checks a
-- claim's records at next_check_at; a record of a verified domain that
-- answers wrong starts its grace (record_missing_since for the TXT,
-- dns_moved_since for the routing record), and one still wrong when
-- --domain-grace has passed deactivates the domain: lapsed, or inactive.
-- A failed lookup (check_error) is no wrong answer and starts no grace.
--
-- Certificates come from ACME (HTTP-01 through the ingress's table):
-- domain_certs holds a domain's current certificate with its key sealed
-- under --secret-key-file (additional data: 'domain-cert:' || domain_id),
-- acme_accounts the platform's account per directory, acme_challenges the
-- HTTP-01 answers every ingress instance serves on port 80 while an order
-- is pending.
--
-- Additive: an instance of the previous version, which a rolling deploy
-- runs beside this one, never reads the new tables; the incident
-- conditions it writes stay allowed.

CREATE TABLE domains (
  id uuid PRIMARY KEY,
  owner text NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  name text NOT NULL CHECK (name ~ '^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z][a-z0-9-]{0,61}[a-z0-9]$'
                            AND length(name) <= 253),   -- lowercase A-labels, no trailing dot
  labels jsonb NOT NULL DEFAULT '{}',
  token text NOT NULL CHECK (token ~ '^[a-z2-7]{32}$'),  -- the claim's token, the TXT record's value
  created_at timestamptz NOT NULL DEFAULT now(),

  -- the binding
  env_id uuid REFERENCES environments(id) ON DELETE SET NULL,
  bound_at timestamptz,                        -- DB clock of the last bind or move; an environment's domains are ordered by it

  -- the proof
  verification text NOT NULL DEFAULT 'unverified'
    CHECK (verification IN ('unverified','verified','lapsed','superseded')),
  verified_at timestamptz,                     -- the last change to verified
  superseded_by uuid,                          -- the claim that took the name (no FK: it may be deleted since)

  -- the checks
  next_check_at timestamptz NOT NULL DEFAULT now(),
  check_backoff_s integer NOT NULL DEFAULT 60 CHECK (check_backoff_s > 0),   -- an unverified claim's backoff, 60 s doubling to 3600 s
  checked_at timestamptz,                      -- the last check that got an answer
  check_error text NOT NULL DEFAULT '',        -- the last lookup's failure; '' when it got an answer
  check_failing_since timestamptz,             -- lookups have failed since (no answer); NULL: the last one answered
  apex boolean,                                -- the name is its zone's apex (SOA); NULL: not known yet
  found jsonb NOT NULL DEFAULT '{}',           -- what the last answer showed: {txt: [...], cname: [...], addresses: [...]}
  routed boolean NOT NULL DEFAULT false,       -- the last answer resolved the name to the edge
  was_routed boolean NOT NULL DEFAULT false,   -- it has resolved to the edge since it was last bound: a wrong answer now starts a grace
  record_missing_since timestamptz,            -- a verified domain's TXT has answered wrong since: its grace
  dns_moved_since timestamptz,                 -- a routed domain's name has resolved elsewhere since: its grace

  -- the serving state, kept by ferund from the columns above and the certificate
  state text NOT NULL DEFAULT 'unverified'
    CHECK (state IN ('unverified','lapsed','superseded','unbound','unpublished','waiting-for-dns',
                     'issuing','served','dns-moved','inactive','failed')),
  reason text NOT NULL DEFAULT '',             -- why failed, or what a state waits for

  -- issuing
  next_issue_at timestamptz,                   -- the next order may start then; NULL: now
  issue_backoff_s integer NOT NULL DEFAULT 0 CHECK (issue_backoff_s >= 0),   -- 0: no failure; else 3600 s doubling to 86400 s
  issue_error text NOT NULL DEFAULT '',        -- the last order's failure
  issue_failing_since timestamptz,             -- orders (or renewals) have failed since

  CHECK (verification <> 'verified' OR verified_at IS NOT NULL),
  CHECK ((env_id IS NULL) OR (bound_at IS NOT NULL)));
CREATE UNIQUE INDEX domains_verified_name ON domains(name) WHERE verification = 'verified';
CREATE INDEX domains_name ON domains(name);
CREATE INDEX domains_owner ON domains(owner, created_at);
CREATE INDEX domains_env ON domains(env_id, bound_at) WHERE env_id IS NOT NULL;
CREATE INDEX domains_next_check ON domains(next_check_at);

CREATE TABLE domain_certs (                    -- a domain's current certificate; replaced on renewal
  domain_id uuid PRIMARY KEY REFERENCES domains(id) ON DELETE CASCADE,
  chain bytea[] NOT NULL CHECK (cardinality(chain) >= 1),   -- DER, leaf first
  key_sealed bytea NOT NULL,                   -- PKCS#8 DER, sealed with aad 'domain-cert:' || domain_id
  not_before timestamptz NOT NULL,
  not_after timestamptz NOT NULL,
  renew_after timestamptz NOT NULL,            -- two thirds of its lifetime
  issued_at timestamptz NOT NULL DEFAULT now(),
  serial text NOT NULL);                       -- lowercase hex

CREATE TABLE acme_accounts (                   -- the platform's ACME account, one per directory
  directory text PRIMARY KEY,                  -- the directory URL
  key_sealed bytea NOT NULL,                   -- the account key, PKCS#8 DER, sealed with aad 'acme-account:' || directory
  uri text NOT NULL DEFAULT '',                -- the account's URL at the CA
  email text NOT NULL DEFAULT '',
  created_at timestamptz NOT NULL DEFAULT now());

CREATE TABLE acme_challenges (                 -- HTTP-01 answers the ingress serves on port 80
  token text PRIMARY KEY CHECK (token ~ '^[A-Za-z0-9_-]{1,256}$'),
  host text NOT NULL,                          -- the domain's name: the answer is given for this host alone
  key_auth text NOT NULL,                      -- the body of the answer
  domain_id uuid REFERENCES domains(id) ON DELETE CASCADE,
  created_at timestamptz NOT NULL DEFAULT now(),
  expires_at timestamptz NOT NULL);            -- gone from the table after this, whatever the order did
CREATE INDEX acme_challenges_expires ON acme_challenges(expires_at);

ALTER TABLE env_incidents DROP CONSTRAINT env_incidents_condition_check;
ALTER TABLE env_incidents ADD CONSTRAINT env_incidents_condition_check
  CHECK (condition IN ('rollout_failed','crash_loop','job_failed','no_capacity',
     'addon_unhealthy','volume_unavailable','attach_failed',
     'domain_record_wrong','domain_deactivated','domain_superseded','certificate_failing'));
