-- SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
-- SPDX-License-Identifier: Apache-2.0

-- What keeps a sandbox awake for its attached services (ADR 0018, "An
-- attached service idles as an image service does"): awake_until, while
-- a started process is not ready yet, until its rollout budget is spent
-- (NULL: no claim); idle_after_s, the service's idle once it runs (0:
-- never; NULL: no claim, the sandbox's own interval applies). The idle
-- loop and its recheck keep a sandbox running while one of its attached
-- services claims it.
--
-- Additive: an instance of the previous version neither reads nor writes
-- the columns, and its idle loop pauses as before.

ALTER TABLE env_attached
  ADD COLUMN awake_until timestamptz,
  ADD COLUMN idle_after_s integer CHECK (idle_after_s >= 0);
