-- SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
-- SPDX-License-Identifier: Apache-2.0

-- Quickwit is the cold stage (ADR 0026 §10): a hot line keeps its trace
-- ID until it goes to the index, which indexes trace IDs itself, so
-- log_traces goes. chunked_last is the mark of what the index holds.

ALTER TABLE log_lines ADD COLUMN trace text NOT NULL DEFAULT '';
DROP TABLE log_traces;
