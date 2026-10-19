-- SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
-- SPDX-License-Identifier: Apache-2.0

-- Environments and releases (ADR 0018): typed objects of a tenant that run
-- together (services, jobs, addons, routes, secrets), changed only by
-- numbered, immutable releases. Each pod of an environment (a service
-- replica, an addon, a job run) is a sandboxes row with environment_id
-- set: the sandbox engine places, starts, moves and deletes it as any
-- sandbox, from the row's pod_template. The environment controller (a set
-- of leader loops) inserts and deletes those rows and keeps its own view
-- of them in env_instances.
--
-- Additive: an instance of the previous version, which a rolling deploy
-- runs beside this one, never reads the new tables and inserts sandboxes
-- with the new columns' defaults (an ordinary sandbox). The DB holds no
-- secret values: env_secrets holds them sealed under --secret-key-file's
-- key, and templates name a secret by name and version.

CREATE TABLE environments (                    -- a row exists while the environment exists; deleting: going
  id uuid PRIMARY KEY,                         -- the client's or a new one; FeCode's app environment id
  owner text NOT NULL REFERENCES tenants(id),
  name text NOT NULL CHECK (name ~ '^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$'
    AND name !~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'),   -- {env} is a UUID or a name
  labels jsonb NOT NULL DEFAULT '{}',          -- string map, for ?label=k=v
  release integer NOT NULL DEFAULT 0 CHECK (release >= 0),     -- the latest desired release; 0: none
  realised integer NOT NULL DEFAULT 0 CHECK (realised >= 0),   -- the latest realised release; 0: none
  deleting boolean NOT NULL DEFAULT false,
  gen bigint NOT NULL DEFAULT 0,               -- the env security group's version (env-<id>)
  created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (owner, name));
CREATE INDEX environments_owner ON environments(owner, created_at);

CREATE TABLE env_releases (                    -- immutable desired state, numbered from 1 per environment
  env_id uuid NOT NULL REFERENCES environments(id) ON DELETE CASCADE,
  number integer NOT NULL CHECK (number >= 1),
  spec jsonb NOT NULL,                         -- normalised: images pinned to digests, secret versions pinned
  base integer NOT NULL CHECK (base >= 0),     -- the release it was made on; 0: none
  author text NOT NULL DEFAULT '', message text NOT NULL DEFAULT '',
  annotations jsonb NOT NULL DEFAULT '{}',     -- string map, at most 4 KiB (the API checks it)
  origin text NOT NULL CHECK (origin IN ('apply','revert','clone','object')),
  origin_ref text NOT NULL DEFAULT '',         -- revert: the number; clone: '<env id>/<number>'; object: '<kind>/<name>'
  state text NOT NULL DEFAULT 'pending' CHECK (state IN ('pending','rolling','realised','failed','superseded')),
  reason text NOT NULL DEFAULT '',             -- failed: why
  started_at timestamptz, finished_at timestamptz,
  created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (env_id, number));

CREATE TABLE env_tags (                        -- a movable name of a release
  env_id uuid NOT NULL,
  tag text NOT NULL CHECK (tag ~ '^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$'),
  number integer NOT NULL,
  moved_by text NOT NULL DEFAULT '', moved_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (env_id, tag),
  FOREIGN KEY (env_id, number) REFERENCES env_releases(env_id, number) ON DELETE CASCADE);

CREATE TABLE env_secrets (                     -- one version of a secret, sealed; versions are never changed
  env_id uuid NOT NULL REFERENCES environments(id) ON DELETE CASCADE,
  name text NOT NULL,
  version integer NOT NULL CHECK (version >= 1),
  sealed bytea NOT NULL,                       -- AES-256-GCM, AAD 'env_secrets|<env id>|<name>|<version>'
  system boolean NOT NULL DEFAULT false,       -- an addon's credential: 'addon.<addon>.<key>', never listed
  created_by text NOT NULL DEFAULT '',
  deleted_at timestamptz,                      -- soft delete: a release may still pin it
  created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (env_id, name, version),
  CHECK (CASE WHEN system THEN name ~ '^addon\.[a-z0-9._-]{1,121}$'
    ELSE name ~ '^[a-z0-9][a-z0-9._-]{0,62}$' AND name NOT LIKE 'addon.%' END));

ALTER TABLE sandboxes
  ADD COLUMN environment_id uuid REFERENCES environments(id),   -- an instance of the environment; fixed
  ADD COLUMN pod_template jsonb,               -- model.PodTemplate; NULL: the sandbox's own pod; fixed
  ADD COLUMN pin_node text NOT NULL DEFAULT '',   -- a create goes only to this node; '': any
  ADD COLUMN idle_after_s integer CHECK (idle_after_s >= 0),     -- NULL: --idle-pause; 0: never
  ADD COLUMN address text NOT NULL DEFAULT ''; -- the pod's IPv6 as last observed; '': none
CREATE INDEX sandboxes_environment ON sandboxes(environment_id) WHERE environment_id IS NOT NULL;

CREATE TABLE env_instances (                   -- the controller's view of an instance row of sandboxes
  sandbox_id uuid PRIMARY KEY REFERENCES sandboxes(id) ON DELETE CASCADE,
  env_id uuid NOT NULL REFERENCES environments(id),
  kind text NOT NULL CHECK (kind IN ('service','addon','job')),
  object text NOT NULL,                        -- the object's name in the spec
  replica integer NOT NULL DEFAULT 0 CHECK (replica >= 0),
  template_hash text NOT NULL,                 -- sha256 hex of the rendered template without labels
  release integer NOT NULL,                    -- the release whose target it was last kept for
  phase text NOT NULL DEFAULT 'starting'
    CHECK (phase IN ('starting','ready','unready','exited','succeeded','failed','retiring')),
  ready_since timestamptz, retire_at timestamptz,
  restarts integer NOT NULL DEFAULT 0, last_exit integer,
  probe_failures integer NOT NULL DEFAULT 0, next_restart_at timestamptz,
  exits jsonb NOT NULL DEFAULT '[]',           -- the last 5 exits, [{at, code}]
  hosts_written text NOT NULL DEFAULT '',      -- hash of the hosts block last written; '': none
  provisioned_hash text NOT NULL DEFAULT '',   -- addons: hash of the provisioning steps last run
  detail text NOT NULL DEFAULT '',
  created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now());
CREATE INDEX env_instances_env ON env_instances(env_id, kind, object, replica);

CREATE TABLE env_job_runs (                    -- one attempt of a job; written with its instance row
  id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  env_id uuid NOT NULL REFERENCES environments(id) ON DELETE CASCADE,
  job text NOT NULL,
  template_hash text NOT NULL,                 -- a succeeded run for (job, template_hash): the job has run
  release integer NOT NULL,
  attempt integer NOT NULL DEFAULT 1 CHECK (attempt >= 1),
  sandbox_id uuid,                             -- its instance row while it exists; no FK
  phase text NOT NULL DEFAULT 'running' CHECK (phase IN ('running','succeeded','failed')),
  exit_code integer,
  detail text NOT NULL DEFAULT '',
  log_tail jsonb NOT NULL DEFAULT '[]',        -- the last 50 lines, kept when the row goes
  started_at timestamptz NOT NULL DEFAULT now(), finished_at timestamptz);
CREATE INDEX env_job_runs_job ON env_job_runs(env_id, job, id);

CREATE TABLE env_attached (                    -- an attached service: a supervised process in a sandbox
  env_id uuid NOT NULL REFERENCES environments(id) ON DELETE CASCADE,
  service text NOT NULL,
  sandbox_id uuid NOT NULL,                    -- the tenant's sandbox it runs in; no FK
  sandbox_incarnation uuid,                    -- the incarnation last used; a new one starts the service anew
  template_hash text NOT NULL DEFAULT '', process_id text NOT NULL DEFAULT '',   -- '': not started
  phase text NOT NULL DEFAULT 'starting' CHECK (phase IN ('starting','ready','unready','exited','failed')),
  ready_since timestamptz,
  restarts integer NOT NULL DEFAULT 0, last_exit integer,
  probe_failures integer NOT NULL DEFAULT 0, next_restart_at timestamptz,
  hosts_written text NOT NULL DEFAULT '',
  detail text NOT NULL DEFAULT '',
  created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (env_id, service));
CREATE INDEX env_attached_sandbox ON env_attached(sandbox_id);

CREATE TABLE env_volumes (                     -- an addon's volume; created once on a node, never resized
  env_id uuid NOT NULL REFERENCES environments(id),
  object text NOT NULL,                        -- 'addon/<name>'
  volume_id uuid NOT NULL UNIQUE,
  size_mib integer NOT NULL CHECK (size_mib > 0),
  node text NOT NULL DEFAULT '',               -- the node that has it; '': none yet
  provisioned boolean NOT NULL DEFAULT false,  -- created on node: never CreateVolume again
  holder uuid,                                 -- the instance row that last held it
  addon_type text NOT NULL DEFAULT '',         -- the addon type whose data it holds; fixed
  data_version text NOT NULL DEFAULT '',       -- the newest blueprint version that ran ready on it
  deleting boolean NOT NULL DEFAULT false,     -- env-volumes deletes it on its node, then the row
  created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (env_id, object));
CREATE INDEX env_volumes_deleting ON env_volumes(env_id) WHERE deleting;

CREATE TABLE env_incidents (                   -- a condition of an object that needs a look
  id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  env_id uuid NOT NULL REFERENCES environments(id) ON DELETE CASCADE,
  object text NOT NULL,                        -- '<kind>/<name>'
  release integer NOT NULL,
  condition text NOT NULL CHECK (condition IN ('rollout_failed','crash_loop','job_failed','no_capacity',
     'addon_unhealthy','volume_unavailable','attach_failed')),
  opened_at timestamptz NOT NULL DEFAULT now(), closed_at timestamptz,
  summary text NOT NULL DEFAULT '',
  evidence jsonb NOT NULL DEFAULT '{}');       -- {exits, logTail, diff, placement}
CREATE UNIQUE INDEX env_incidents_open ON env_incidents(env_id, object, release, condition) WHERE closed_at IS NULL;
CREATE INDEX env_incidents_env ON env_incidents(env_id, id);

CREATE TABLE env_route_tokens (                -- hashed only; shown once
  hash text PRIMARY KEY,                       -- sha256 hex
  env_id uuid NOT NULL REFERENCES environments(id) ON DELETE CASCADE,
  created_at timestamptz NOT NULL DEFAULT now(), last_used_at timestamptz);
CREATE INDEX env_route_tokens_env ON env_route_tokens(env_id);
