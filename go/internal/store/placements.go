// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package store

// PlacementCandidate is one node's verdict in a placement decision. The
// decision's audit is the activity row of kind placement, which names the
// verdict of every node.
type PlacementCandidate struct {
	Node     string  `json:"node"`
	Feasible bool    `json:"feasible"`
	Score    float64 `json:"score"`
	Reason   string  `json:"reason"`
}
