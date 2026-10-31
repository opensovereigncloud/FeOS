-- SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
-- SPDX-License-Identifier: Apache-2.0

-- Traces (ADR 0026 §10, docs/architecture/traces.md): a span waits here
-- from its ingest until its index has published it. doc is the span as
-- the index holds it, bytes its size; claim_owner and claim_lease are the
-- indexer's claim.
CREATE TABLE trace_spans (
  id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  tenant text NOT NULL, trace_id text NOT NULL, span_id text NOT NULL,
  start_ns bigint NOT NULL,
  doc jsonb NOT NULL, bytes integer NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  claim_owner text, claim_lease timestamptz);
CREATE INDEX trace_spans_trace ON trace_spans(tenant, trace_id);

-- The quota: bytes of documents a tenant stored on a day (UTC, DB clock).
CREATE TABLE trace_usage (
  tenant text NOT NULL, day date NOT NULL, bytes bigint NOT NULL,
  PRIMARY KEY (tenant, day));
