-- SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
-- SPDX-License-Identifier: Apache-2.0

-- The ingress's access log (ADR 0021): a row per request ferun-ingress
-- answered for an environment, which is also the request's server span
-- (W3C Trace Context: trace_id, span_id, parent_span_id). The ingress
-- sends them in batches numbered by (ingress, start_id, seq); a batch
-- sent again is stored once. Partitioned by day like traffic_records,
-- kept for --access-log-retention.
--
-- 0016: private-images holds 0013 and 0014, domain certificates 0015.
--
-- Additive: an instance of the previous version never reads the table.

CREATE TABLE access_logs (
  day date NOT NULL DEFAULT (now() AT TIME ZONE 'utc')::date,
  id bigint GENERATED ALWAYS AS IDENTITY,
  ts_ns bigint NOT NULL,                       -- the request's start
  ingress text NOT NULL, start_id uuid NOT NULL, seq bigint NOT NULL,
  env_id uuid NOT NULL, owner text NOT NULL,   -- attributed when stored; no foreign key: the log outlives the environment
  service text NOT NULL, instance text NOT NULL,   -- instance: the last attempt's, '' for none
  host text NOT NULL, method text NOT NULL, path text NOT NULL,   -- path: escaped, without the query
  proto text NOT NULL, status integer NOT NULL,                  -- status: 0 when none was written
  outcome text NOT NULL,                       -- 1xx … 5xx, upgraded, aborted
  duration_ns bigint NOT NULL, held_ns bigint NOT NULL, attempts integer NOT NULL,
  bytes_in bigint NOT NULL, bytes_out bigint NOT NULL,
  client_ip text NOT NULL, user_agent text NOT NULL,
  trace_id text NOT NULL, span_id text NOT NULL, parent_span_id text NOT NULL,   -- lower case hex; parent '' for none
  PRIMARY KEY (day, id)) PARTITION BY RANGE (day);
CREATE INDEX access_logs_env ON access_logs(env_id, ts_ns);
CREATE UNIQUE INDEX access_logs_seq ON access_logs(ingress, start_id, seq, day);   -- backstop
