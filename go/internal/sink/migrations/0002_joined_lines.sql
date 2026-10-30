-- SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
-- SPDX-License-Identifier: Apache-2.0

-- A line that a partial one goes on with (sink.md, Reading): cont marks
-- it, so that a read answers it joined to the line it continues and never
-- on its own. A stream's open_sources has the bit of each source whose
-- last line stored is partial (1 stdout, 2 stderr, 4 none).

ALTER TABLE log_lines ADD COLUMN cont boolean NOT NULL DEFAULT false;
ALTER TABLE log_streams ADD COLUMN open_sources smallint NOT NULL DEFAULT 0;
