//go:build !harness && lgpl

package eth

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"testing"
	"time"

	"decred.org/dcrdex/dex"
	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

type tEthereumClient struct {
	isOutdated        bool
	balanceAtErr      error
	headerByNumberErr error
	txError           error
}

func (t *tEthereumClient) HeaderByNumber(ctx context.Context, number *big.Int) (*types.Header, error) {
	if t.headerByNumberErr != nil {
		return nil, t.headerByNumberErr
	}

	if t.isOutdated {
		return &types.Header{
			Time: uint64(time.Now().Add(-(time.Minute * 2)).Unix()),
		}, nil
	}

	return &types.Header{
		Time: uint64(time.Now().Unix()),
	}, nil
}

func (t *tEthereumClient) SuggestGasTipCap(ctx context.Context) (*big.Int, error) {
	return nil, fmt.Errorf("not implemented")
}
func (t *tEthereumClient) SyncProgress(ctx context.Context) (*ethereum.SyncProgress, error) {
	return nil, fmt.Errorf("not implemented")
}
func (t *tEthereumClient) BlockNumber(ctx context.Context) (uint64, error) {
	return 0, fmt.Errorf("not implemented")
}
func (t *tEthereumClient) TransactionByHash(ctx context.Context, txHash common.Hash) (*types.Transaction, bool, error) {
	if t.txError != nil {
		return nil, false, t.txError
	}

	return &types.Transaction{}, false, nil
}
func (t *tEthereumClient) BalanceAt(ctx context.Context, account common.Address, blockNumber *big.Int) (*big.Int, error) {
	if t.balanceAtErr != nil {
		return nil, t.balanceAtErr
	}

	return big.NewInt(0), nil
}
func (t *tEthereumClient) Close() {}

func newEthConn() *ethConn {
	return &ethConn{
		client: &tEthereumClient{},
	}
}

func setOutdated(conn *ethConn, outdated bool) {
	conn.client.(*tEthereumClient).isOutdated = outdated
}

func setPreviouslyOutdated(conn *ethConn, outdated bool) {
	conn.outdated = outdated
}

func setBalanceAtError(conn *ethConn, err error) {
	conn.client.(*tEthereumClient).balanceAtErr = err
}

func setHeaderByNumberError(conn *ethConn, err error) {
	conn.client.(*tEthereumClient).headerByNumberErr = err
}

func setTransactionByHashError(conn *ethConn, err error) {
	conn.client.(*tEthereumClient).txError = err
}

type connState struct {
	previouslyOutdated   bool
	outdated             bool
	balanceAtErr         error
	headerByNumberErr    error
	transactionByHashErr error
}

func TestClientRotation(t *testing.T) {
	ctx := context.Background()
	tLogger = dex.StdOutLogger("ETHTEST", dex.LevelTrace)

	type test struct {
		name       string
		connStates []connState
		idx        int

		expectedIdx         int
		expectedErr         bool
		expectAllowOutdated bool
	}

	tests := []test{
		{
			name: "none outdated, no errors",
			connStates: []connState{
				{},
				{},
				{},
			},
			idx:         0,
			expectedIdx: 0,
			expectedErr: false,
		},
		{
			name: "none outdated, client 0 balance error",
			connStates: []connState{
				{balanceAtErr: errors.New("")},
				{},
				{},
			},
			idx:         0,
			expectedIdx: 1,
			expectedErr: false,
		},
		{
			name: "1 errors, 2 was and still is outdated",
			connStates: []connState{
				{},
				{balanceAtErr: errors.New("")},
				{previouslyOutdated: true, outdated: true},
			},
			idx:         1,
			expectedIdx: 0,
			expectedErr: false,
		},
		{
			name: "0 and 1 errors, 2 was but no longer outdated",
			connStates: []connState{
				{balanceAtErr: errors.New("")},
				{balanceAtErr: errors.New("")},
				{previouslyOutdated: true},
			},
			idx:         0,
			expectedIdx: 2,
			expectedErr: false,
		},
		{
			name: "all outdated",
			connStates: []connState{
				{previouslyOutdated: true, outdated: true},
				{previouslyOutdated: true, outdated: true},
				{previouslyOutdated: true, outdated: true},
			},
			idx:                 0,
			expectedIdx:         0,
			expectedErr:         false,
			expectAllowOutdated: true,
		},
	}

	for _, test := range tests {
		clients := make([]*ethConn, 0, len(test.connStates))
		for _, state := range test.connStates {
			conn := newEthConn()
			setPreviouslyOutdated(conn, state.previouslyOutdated)
			setOutdated(conn, state.outdated)
			setBalanceAtError(conn, state.balanceAtErr)
			setHeaderByNumberError(conn, state.headerByNumberErr)
			clients = append(clients, conn)
		}
		rpcClient := &rpcclient{
			clients:     clients,
			log:         tLogger,
			endpointIdx: test.idx,
		}

		_, err := rpcClient.accountBalance(ctx, BipID, common.Address{})
		if (err != nil) != test.expectedErr {
			t.Fatalf("%s: Unexpected error: %v", test.name, err)
		}

		if rpcClient.endpointIdx != test.expectedIdx {
			t.Fatalf("%s: Expected endpointIdx to be %d, got %d", test.name, test.expectedIdx, rpcClient.endpointIdx)
		}

		if rpcClient.allowOutdated != test.expectAllowOutdated {
			t.Fatalf("%s: Expected allowOutdated to be %v, got %v", test.name, test.expectAllowOutdated, rpcClient.allowOutdated)
		}
	}
}

func TestBestHeader(t *testing.T) {
	ctx := context.Background()

	tLogger = dex.StdOutLogger("ETHTEST", dex.LevelTrace)

	type test struct {
		name       string
		connStates []connState
		idx        int

		expectedIdx            int
		expectedHeaderOutdated bool
	}

	tests := []test{
		{
			name: "0 outdated, returns anyways",
			connStates: []connState{
				{outdated: true},
				{},
				{},
			},
			idx:                    0,
			expectedIdx:            1,
			expectedHeaderOutdated: true,
		},
		{
			name: "0 outdated, returns anyways",
			connStates: []connState{
				{headerByNumberErr: errors.New("")},
				{},
				{},
			},
			idx:                    0,
			expectedIdx:            1,
			expectedHeaderOutdated: false,
		},
	}

	for _, test := range tests {
		clients := make([]*ethConn, 0, len(test.connStates))
		for _, state := range test.connStates {
			conn := newEthConn()
			setPreviouslyOutdated(conn, state.previouslyOutdated)
			setOutdated(conn, state.outdated)
			setBalanceAtError(conn, state.balanceAtErr)
			setHeaderByNumberError(conn, state.headerByNumberErr)
			clients = append(clients, conn)
		}

		rpcClient := &rpcclient{
			clients:     clients,
			log:         tLogger,
			endpointIdx: test.idx,
		}

		hdr, err := rpcClient.bestHeader(ctx)
		if err != nil {
			t.Fatalf("%s: Unexpected error: %v", test.name, err)
		}

		if rpcClient.endpointIdx != test.expectedIdx {
			t.Fatalf("%s: Expected endpointIdx to be %d, got %d", test.name, test.expectedIdx, rpcClient.endpointIdx)
		}

		if rpcClient.headerIsOutdated(hdr) != test.expectedHeaderOutdated {
			t.Fatalf("%s: Expected header to be outdated: %v", test.name, test.expectedHeaderOutdated)
		}
	}
}

func TestTransaction(t *testing.T) {
	ctx := context.Background()

	tLogger = dex.StdOutLogger("ETHTEST", dex.LevelTrace)

	type test struct {
		name       string
		connStates []connState
		idx        int

		expectedIdx int
		expectedErr bool
	}

	tests := []test{
		{
			name: "conn 0 tx not found, not outdated",
			connStates: []connState{
				{transactionByHashErr: errors.New("not found")},
				{},
				{},
			},
			idx:         0,
			expectedIdx: 0,
			expectedErr: true,
		},
		{
			name: "conn 0 tx not found but outdated, found with conn 1",
			connStates: []connState{
				{outdated: true, transactionByHashErr: errors.New("not found")},
				{},
				{},
			},
			idx:         0,
			expectedIdx: 1,
			expectedErr: false,
		},
	}

	for _, test := range tests {
		clients := make([]*ethConn, 0, len(test.connStates))
		for _, state := range test.connStates {
			conn := newEthConn()
			setPreviouslyOutdated(conn, state.previouslyOutdated)
			setOutdated(conn, state.outdated)
			setBalanceAtError(conn, state.balanceAtErr)
			setHeaderByNumberError(conn, state.headerByNumberErr)
			setTransactionByHashError(conn, state.transactionByHashErr)
			clients = append(clients, conn)
		}

		rpcClient := &rpcclient{
			clients:     clients,
			log:         tLogger,
			endpointIdx: test.idx,
		}

		_, _, err := rpcClient.transaction(ctx, common.Hash{})
		if (err != nil) != test.expectedErr {
			t.Fatalf("%s: Unexpected error: %v", test.name, err)
		}

		if rpcClient.endpointIdx != test.expectedIdx {
			t.Fatalf("%s: Expected endpointIdx to be %d, got %d", test.name, test.expectedIdx, rpcClient.endpointIdx)
		}
	}
}
