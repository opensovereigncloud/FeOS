-- SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
-- SPDX-License-Identifier: Apache-2.0

-- Tool calls that outlive their caller (ADR 0023): a call of a tool with a
-- side effect (bash, write, edit, process_start, process_stop) that carries
-- an idempotency key is a row here while it runs and for an hour after it
-- ended. sent is committed before the call's request leaves for the guest,
-- so a row whose instance died is interrupted when sent and not_run
-- otherwise. The instance that runs a call renews lease_until; the leader,
-- a waiter or a restart under the same instance ID ends a running row whose
-- lease passed. A not_run row is replaced by a new run of the same call.
-- Additive: an instance of the previous version neither reads nor writes it.
CREATE TABLE tool_calls (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  sandbox_id text COLLATE "C" NOT NULL REFERENCES sandboxes(id) ON DELETE CASCADE,
  call_key text COLLATE "C" NOT NULL,
  tool text NOT NULL,
  args_hash bytea NOT NULL,
  instance_id text NOT NULL,
  state text NOT NULL DEFAULT 'running',
  sent boolean NOT NULL DEFAULT false,
  lease_until timestamptz NOT NULL,
  started_at timestamptz NOT NULL DEFAULT now(),
  finished_at timestamptz,
  exit_code integer NOT NULL DEFAULT 0,
  output bytea NOT NULL DEFAULT ''::bytea,
  truncated boolean NOT NULL DEFAULT false,
  is_error boolean NOT NULL DEFAULT false,
  error text NOT NULL DEFAULT '',
  CONSTRAINT tool_calls_key UNIQUE (sandbox_id, call_key),
  CONSTRAINT tool_calls_state CHECK (state IN ('running', 'done', 'interrupted', 'not_run')),
  CONSTRAINT tool_calls_finished_at CHECK ((state = 'running') = (finished_at IS NULL)),
  CONSTRAINT tool_calls_key_format CHECK (call_key ~ '^[A-Za-z0-9._~:@-]{1,255}$'),
  CONSTRAINT tool_calls_args_hash CHECK (octet_length(args_hash) = 32),
  CONSTRAINT tool_calls_output_size CHECK (octet_length(output) <= 65536),
  CONSTRAINT tool_calls_error_size CHECK (octet_length(error) <= 8192)
);
CREATE INDEX tool_calls_running ON tool_calls (lease_until) WHERE state = 'running';
CREATE INDEX tool_calls_ended ON tool_calls (finished_at) WHERE state <> 'running';
