-- SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
-- SPDX-License-Identifier: Apache-2.0

-- The chunks the sink wrote before Quickwit (ADR 0026 §10) all moved into
-- the index on 2026-10-31; their table goes with the code that moved them.

DROP TABLE log_chunks;
