// SPDX-License-Identifier: LicenseRef-BANYAN-ENTERPRISE
//
// Enterprise License — see LICENSE.enterprise at the enterprise module
// root: free to modify and test; production use requires a subscription.

// Package wiring exists to pin the module-boundary contract at compile
// time: enterprise is a separate Go module that MAY import the MIT core's
// packages — including internal/ (the internal visibility rule is
// import-path based, and this module's path lives under the core's tree).
// If this probe ever fails to build, the boundary design broke; stop and
// reconsider (public API packages or a same-module build tag), do not
// silently widen imports.
package wiring

import (
	"testing"

	"github.com/banyan-vcs/banyan/enterprise/license"
	"github.com/banyan-vcs/banyan/internal/auth"
)

// The enterprise gate plugs into the same authorization seam the core
// ships: an enterprise policy source would implement auth.Service and be
// licensed through this module's plan gate.
func TestCoreSeamsAreImportable(t *testing.T) {
	var _ auth.Service = (*auth.StaticPolicy)(nil)
	assert := license.FeaturePolicyDB
	if assert == "" {
		t.Fatal("feature enum must not be empty")
	}
}
