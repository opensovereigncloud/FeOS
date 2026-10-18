// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package tools_test

import (
	"github.com/ironcore-dev/feos/go/internal/env/envdeps"
	"github.com/ironcore-dev/feos/go/internal/tools"
)

// The server picks the runner's detached calls up by a type assertion
// (env.Runner): a signature that drifts from envdeps.Runner fails here,
// not silently there.
var _ envdeps.Runner = (*tools.Runner)(nil)
