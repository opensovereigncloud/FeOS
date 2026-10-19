-- SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
-- SPDX-License-Identifier: Apache-2.0

-- An environment's names come from feosd's resolver (ADR 0018, ADR 0010):
-- the controller puts name tables on the nodes as it puts security
-- groups, and writes no hosts blocks any more. An instance whose node's
-- feosd serves no names opens names_unserved.
--
-- hosts_written of env_instances and env_attached is read and written by
-- nothing from here on, and goes in a later migration: the previous
-- version, which a rolling deploy runs beside this one, still reads and
-- writes it.

ALTER TABLE env_incidents DROP CONSTRAINT env_incidents_condition_check;
ALTER TABLE env_incidents ADD CONSTRAINT env_incidents_condition_check
  CHECK (condition IN ('rollout_failed','crash_loop','job_failed','no_capacity',
     'addon_unhealthy','volume_unavailable','attach_failed','names_unserved'));
