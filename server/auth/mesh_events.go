// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package auth

import (
	"context"

	"decred.org/dcrdex/server/mesh"
)

// MeshService provides the mesh operations used by auth.
type MeshService interface {
	ProxyClientMessage(context.Context, *mesh.ClientProxyMessage) error
}
