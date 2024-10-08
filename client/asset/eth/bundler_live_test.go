//go:build bundlerlive

package eth

import (
	"context"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

func TestBundler(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	entryPoint := common.HexToAddress("0x0ab33178da901e8504a1de8d4d64dbb37cf6ab57")
	bundler, err := newBundler(ctx, "http://localhost:38557", entryPoint)
	if err != nil {
		t.Fatalf("newBundler error: %v", err)
	}

	userOpParam := &UserOperationParam{
		Sender: "0x123",
	}

	if err := bundler.sendUserOp(ctx, userOpParam); err != nil {
		t.Fatalf("sendUserOpError: %v", err)
	}
}
