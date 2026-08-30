// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package market

import (
	"decred.org/dcrdex/dex/msgjson"
	"decred.org/dcrdex/dex/order"
)

// noMatchMessage builds the owner notification sent when an order leaves the
// epoch without a taker-side match outcome notification. Clients interpret this
// as "booked" for standing limits and "done" for immediate orders.
func noMatchMessage(oid order.OrderID) (*msgjson.Message, error) {
	return msgjson.NewNotification(msgjson.NoMatchRoute, &msgjson.NoMatch{
		OrderID: oid[:],
	})
}
