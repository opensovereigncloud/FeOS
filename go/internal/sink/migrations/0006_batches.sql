-- SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
-- SPDX-License-Identifier: Apache-2.0

-- The hot stage holds a row per (stream, ingested batch) instead of a row
-- per line (ADR 0026 §4): a write, an index entry and a delete a batch.
-- data holds the batch's lines in offset order (encodeBatch); first_off and
-- last_off its first and last line's offsets, min_ts and max_ts its lines'
-- times (Unix ns), lines and bytes (of text) their counts, levels the bit
-- of every level its lines have. Partitioned like log_lines by the day
-- (UTC, DB clock) it arrived; the indexer deletes a batch once Quickwit
-- holds its lines, and an empty day's partition is dropped.
--
-- log_lines stays for now: instances of before write it until they are
-- gone, and the indexer drains what they left.

CREATE TABLE log_batches (
  day date NOT NULL DEFAULT (now() AT TIME ZONE 'utc')::date,
  stream_id bigint NOT NULL REFERENCES log_streams(id) ON DELETE CASCADE,
  first_off bigint NOT NULL, last_off bigint NOT NULL,
  min_ts bigint NOT NULL, max_ts bigint NOT NULL,
  lines integer NOT NULL, bytes bigint NOT NULL, levels smallint NOT NULL,
  data bytea COMPRESSION lz4 NOT NULL,
  PRIMARY KEY (stream_id, first_off, day)) PARTITION BY RANGE (day);
CREATE INDEX log_batches_ts ON log_batches(stream_id, max_ts);
