package eth

import (
	"context"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/rpc"
)

type bundler struct {
	rpcClient  *rpc.Client
	epAddress  common.Address
	entryPoint *EntryPointCaller
}

func newBundler(ctx context.Context, endpoint string, epAddr common.Address, backend bind.ContractCaller) (*bundler, error) {
	rpcClient, err := rpc.DialContext(ctx, endpoint)
	if err != nil {
		return nil, err
	}

	entryPoint, err := NewEntryPointCaller(epAddr, backend)
	if err != nil {
		return nil, err
	}

	b := &bundler{
		rpcClient:  rpcClient,
		entryPoint: entryPoint,
		epAddress:  epAddr,
	}

	// Check if the entry point is supported
	entryPoints, err := b.supportedEntryPoints(ctx)
	if err != nil {
		return nil, err
	}

	found := false
	for _, ep := range entryPoints {
		if ep == epAddr {
			found = true
			break
		}
	}

	if !found {
		return nil, fmt.Errorf("entry point %s is not supported", epAddr.Hex())
	}

	return b, nil
}

func (b *bundler) supportedEntryPoints(ctx context.Context) ([]common.Address, error) {
	var res []string
	err := b.rpcClient.CallContext(ctx, &res, "eth_supportedEntryPoints")
	if err != nil {
		return nil, err
	}

	entryPoints := make([]common.Address, len(res))
	for i, v := range res {
		entryPoints[i] = common.HexToAddress(v)
	}

	return entryPoints, nil
}

type UserOperationParam struct {
	Sender               string `json:"sender"`
	Nonce                string `json:"nonce"`
	InitCode             string `json:"initCode"`
	CallData             string `json:"callData"`
	CallGasLimit         string `json:"callGasLimit"`
	VerificationGasLimit string `json:"verificationGasLimit"`
	PreVerificationGas   string `json:"preVerificationGas"`
	MaxFeePerGas         string `json:"maxFeePerGas"`
	MaxPriorityFeePerGas string `json:"maxPriorityFeePerGas"`
	PaymasterAndData     string `json:"paymasterAndData"`
	Signature            string `json:"signature"`
}

func (b *bundler) sendUserOp(ctx context.Context, userOp *UserOperationParam) error {
	var res string

	err := b.rpcClient.CallContext(ctx, &res, "eth_sendUserOperation", *userOp, b.epAddress)
	if err != nil {
		return err
	}

	fmt.Println(res)

	return nil
}

func (b *bundler) getNonce(opts *bind.CallOpts, ethSwapAddr, participantAddr common.Address) (*big.Int, error) {
	keyB := make([]byte, 24)
	copy(keyB[:20], participantAddr[:])
	key := new(big.Int).SetBytes(keyB)
	return b.entryPoint.GetNonce(opts, ethSwapAddr, key)
}
