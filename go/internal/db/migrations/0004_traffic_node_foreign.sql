-- SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
-- SPDX-License-Identifier: Apache-2.0

-- A node's foreign traffic records, those attributed to no sandbox, newest
-- first: a page of a node's activity (store.ListNodeTraffic). Without this
-- index such a page read every record of the node over the whole retention
-- and sorted them. Partial, it holds only the foreign records, and serves
-- the filter and the order with a backward scan of each day's partition.
CREATE INDEX traffic_node_foreign ON traffic_records(node, ts_ns, id) WHERE sandbox_id IS NULL;
