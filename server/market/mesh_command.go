// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package market

import (
	"decred.org/dcrdex/server/mesh"
)

const (
	commandKindLimit  = "limit"
	commandKindMarket = "market"
	commandKindCancel = "cancel"
)

// Commands returns the order router's mesh command executors.
func (r *OrderRouter) Commands() map[string]mesh.CommandExecutor {
	return map[string]mesh.CommandExecutor{
		commandKindLimit:  r.executeLimit,
		commandKindMarket: r.executeMarket,
		commandKindCancel: r.executeCancel,
	}
}
