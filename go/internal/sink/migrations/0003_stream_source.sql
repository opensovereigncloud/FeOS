-- SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
-- SPDX-License-Identifier: Apache-2.0

-- A stream's source (ADR 0026 §8): 'container', a container's output
-- shipped by a node, or 'ingress', the access log an ingress writes for
-- a service. The selector source picks them.

ALTER TABLE log_streams ADD COLUMN source text NOT NULL DEFAULT 'container';
