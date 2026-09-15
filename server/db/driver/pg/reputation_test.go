// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package pg

import (
	"testing"

	"decred.org/dcrdex/server/account"
)

func TestSetReputationInputsListenerOnce(t *testing.T) {
	archiver := new(Archiver)
	archiver.SetReputationInputsListener(func(...account.AccountID) {})
	defer func() {
		if recover() == nil {
			t.Fatal("second SetReputationInputsListener did not panic")
		}
	}()
	archiver.SetReputationInputsListener(func(...account.AccountID) {})
}
