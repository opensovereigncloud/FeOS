-- SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
-- SPDX-License-Identifier: Apache-2.0

-- The ingress's access log is a stream of the application's in the sink
-- (ADR 0026 §8): ferun-ingress writes it there, and …/access-logs reads
-- it from there alone. access_logs (0016), ferund's own copy from before,
-- goes with its daily partitions, which a DROP of the partitioned table
-- takes along. Its records are not moved: no history is kept (Malte,
-- 2026-10-31).
--
-- Not additive. This version neither reads nor writes the table. An
-- instance of the previous version, which a rolling deploy runs beside
-- this one for minutes, fails until it is replaced: its hourly partition
-- run (activity-partitions) errors, creating no new partitions of any
-- table in that window (today's and the next two days' exist already);
-- its PublishAccessLogs answers UNAVAILABLE, which an ingress without
-- --sink-endpoint retries; and its …/access-logs answers 500 for a page
-- the sink does not fill, where it read the rest from the table.

DROP TABLE access_logs;
