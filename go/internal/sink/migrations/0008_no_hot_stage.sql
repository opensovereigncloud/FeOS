-- SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
-- SPDX-License-Identifier: Apache-2.0

-- Redpanda is the log of record (ADR 0026 §11): the hot stage of before
-- (log_batches, log_lines before it, trace_spans) and the indexers'
-- claims and marks go. Their rows were moved to the topics on 2026-10-31.

DROP TABLE log_batches;
DROP TABLE log_lines;
DROP TABLE trace_spans;
DROP INDEX log_streams_hot;
ALTER TABLE log_streams DROP COLUMN hot_lines, DROP COLUMN hot_bytes, DROP COLUMN hot_since,
  DROP COLUMN chunked_last, DROP COLUMN chunk_owner, DROP COLUMN chunk_lease, DROP COLUMN chunk_claimed_at;
