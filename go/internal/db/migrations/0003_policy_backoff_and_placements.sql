-- SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
-- SPDX-License-Identifier: Apache-2.0

-- A pause that the idle policy began, or a hibernation that the
-- hibernate-after policy began, and that failed (the pod still runs, or
-- stays suspended) backs its policy off: that policy's loop begins no op
-- for the sandbox before policy_not_before, which doubles per consecutive
-- failure of the same policy; the other policy's loop is not held back. A
-- pause or a hibernation that reaches its goal, and a move to another
-- node, clear all three.
ALTER TABLE sandboxes
  ADD COLUMN policy_failed text NOT NULL DEFAULT '',      -- the policy backed off: idle, hibernate_after; '': none
  ADD COLUMN policy_failures integer NOT NULL DEFAULT 0,  -- its consecutive failed ops
  ADD COLUMN policy_not_before timestamptz;               -- its next op not before; NULL: none

-- The placement audit is the activity row of kind placement, which
-- activity retention drops. The placements table held the same decisions,
-- grew without bound, outlived its sandboxes and was read by nothing; no
-- instance writes it from this version on. It is emptied here and dropped
-- by a later migration: an instance of an earlier version, which a rolling
-- deploy runs beside this one for a while, still inserts into it in every
-- create's and restore's begin.
TRUNCATE placements;
