-- SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
-- SPDX-License-Identifier: Apache-2.0

-- The schema of ferun-sink (ADR 0026, docs/architecture/sink.md), in its
-- own database. Migrate applies it once, in one transaction, under an
-- advisory lock.

CREATE TABLE schema_migrations (
  version integer PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now());

-- A stream: one log of one container of one pod, keyed as the node names
-- it (ferun.pod, ferun.container, ferun.log_id). Its labels are the first
-- write's. sid is its ID in the API and in the object keys, s_ and 16 hex
-- digits, random so that a database made anew never reuses a key.
-- Counters: lines and bytes (of text) ever stored; hot_lines, hot_bytes and
-- hot_since (when the oldest line not in a chunk arrived) of the hot stage;
-- chunked_last the offset of the last line in a chunk, -1 for none: a line
-- at or below it is stored. next_offset is where the node goes on
-- (Positions), last_off the offset of the last line stored, -1 for none. chunk_owner and chunk_lease are the cold stage's claim.
CREATE TABLE log_streams (
  id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  sid text NOT NULL UNIQUE,
  pod text NOT NULL, container text NOT NULL, log_id text NOT NULL,
  tenant text NOT NULL, node text NOT NULL,
  instance text NOT NULL DEFAULT '', environment text NOT NULL DEFAULT '',
  kind text NOT NULL DEFAULT '', object text NOT NULL DEFAULT '',
  replica integer NOT NULL DEFAULT 0, release integer NOT NULL DEFAULT 0,
  job text NOT NULL DEFAULT '', attempt integer NOT NULL DEFAULT 0,
  retention_days integer,                        -- ferun.retention_days; NULL: --log-retention
  labels jsonb NOT NULL DEFAULT '{}',            -- every resource attribute of the first write
  first_ns bigint, last_ns bigint,               -- the lines' times, Unix ns
  next_offset bigint NOT NULL DEFAULT 0, last_off bigint NOT NULL DEFAULT -1,
  lines bigint NOT NULL DEFAULT 0, bytes bigint NOT NULL DEFAULT 0,
  hot_lines bigint NOT NULL DEFAULT 0, hot_bytes bigint NOT NULL DEFAULT 0, hot_since timestamptz,
  chunked_last bigint NOT NULL DEFAULT -1,
  chunk_owner text, chunk_lease timestamptz, chunk_claimed_at timestamptz,
  created_at timestamptz NOT NULL DEFAULT now(),
  ended_at timestamptz,
  UNIQUE (pod, container, log_id));
CREATE INDEX log_streams_env ON log_streams(tenant, environment, object, replica) WHERE environment <> '';
CREATE INDEX log_streams_job ON log_streams(tenant, job) WHERE job <> '';
CREATE INDEX log_streams_instance ON log_streams(tenant, instance);
CREATE INDEX log_streams_last ON log_streams(tenant, last_ns);
CREATE INDEX log_streams_hot ON log_streams(hot_since) WHERE hot_lines > 0;

-- The hot stage: a row a line, partitioned by the day (UTC, DB clock) it
-- arrived; the cold stage deletes what it moved to a chunk, and an empty
-- day's partition is dropped. level is a bit of LevelBits; source
-- stdout, stderr or ''.
CREATE TABLE log_lines (
  day date NOT NULL DEFAULT (now() AT TIME ZONE 'utc')::date,
  stream_id bigint NOT NULL REFERENCES log_streams(id) ON DELETE CASCADE,
  off bigint NOT NULL, ts bigint NOT NULL,
  source text NOT NULL, level smallint NOT NULL, partial boolean NOT NULL,
  text text NOT NULL,
  PRIMARY KEY (stream_id, off, day)) PARTITION BY RANGE (day);
CREATE INDEX log_lines_ts ON log_lines(stream_id, ts);

-- The cold stage: a chunk of a stream's lines in the object store, gzip
-- NDJSON at key. levels has the bit of every level its lines have.
-- No cascade: a stream goes only once its chunks' objects did.
CREATE TABLE log_chunks (
  id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  stream_id bigint NOT NULL REFERENCES log_streams(id),
  first_off bigint NOT NULL, last_off bigint NOT NULL,
  first_ns bigint NOT NULL, last_ns bigint NOT NULL,
  lines integer NOT NULL, bytes bigint NOT NULL, levels smallint NOT NULL,
  key text NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (stream_id, first_off));
CREATE INDEX log_chunks_time ON log_chunks(stream_id, last_ns, first_ns);
CREATE INDEX log_chunks_expiry ON log_chunks(last_ns);

-- The trace index: a line that names a W3C trace ID.
CREATE TABLE log_traces (
  trace_id text NOT NULL,
  stream_id bigint NOT NULL REFERENCES log_streams(id) ON DELETE CASCADE,
  off bigint NOT NULL, ts bigint NOT NULL,
  PRIMARY KEY (trace_id, stream_id, off));
CREATE INDEX log_traces_stream ON log_traces(stream_id, ts);

-- The quota: bytes of text a tenant stored on a day (UTC, DB clock).
CREATE TABLE log_usage (
  tenant text NOT NULL, day date NOT NULL, bytes bigint NOT NULL,
  PRIMARY KEY (tenant, day));
