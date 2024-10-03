package main

// create a webserver

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"time"

	"decred.org/dcrdex/dex/encode"
	dexeth "decred.org/dcrdex/dex/networks/eth"

	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/consensus/misc/eip1559"
	ethcore "github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/eth/ethconfig"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rpc"
)

var alphaHTTPAddress = "http://localhost:38556"

type rpcRequest struct {
	JSONRPC string        `json:"jsonrpc"`
	ID      int           `json:"id"`
	Method  string        `json:"method"`
	Params  []interface{} `json:"params"`
}

type rpcResponse struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      int         `json:"id"`
	Result  interface{} `json:"result"`
}

type bundler struct {
	pk                *ecdsa.PrivateKey
	address           common.Address
	entryPoint        *EntryPoint
	entryPointAddress common.Address
	client            *rpc.Client
	chainCfg          *params.ChainConfig
	handlers          map[string]func(w http.ResponseWriter, req *rpcRequest)
}

// simnetDataDir returns the data directory for Ethereum simnet.
func simnetDataDir() (string, error) {
	u, err := user.Current()
	if err != nil {
		return "", fmt.Errorf("error getting current user: %w", err)
	}

	return filepath.Join(u.HomeDir, "dextest", "eth"), nil
}

// readSimnetGenesisFile reads the simnet genesis file.
func readSimnetGenesisFile() (*ethcore.Genesis, error) {
	dataDir, err := simnetDataDir()
	if err != nil {
		return nil, err
	}

	genesisFile := filepath.Join(dataDir, "genesis.json")
	genesisCfg, err := dexeth.LoadGenesisFile(genesisFile)
	if err != nil {
		return nil, fmt.Errorf("error reading genesis file: %v", err)
	}

	return genesisCfg, nil
}

func newBundler(privKey string) (*bundler, error) {
	if len(privKey) == 0 {
		privKey = hex.EncodeToString(encode.RandomBytes(32))
	}

	fmt.Println("Priv key is ", privKey)

	pk, err := crypto.HexToECDSA(privKey)
	if err != nil {
		return nil, err
	}

	fmt.Println("Bundler address is", crypto.PubkeyToAddress(pk.PublicKey))

	timedCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := rpc.DialContext(timedCtx, alphaHTTPAddress)
	if err != nil {
		return nil, err
	}

	epAddress := getEntryPointAddress()

	entryPoint, err := NewEntryPoint(epAddress, ethclient.NewClient(client))
	if err != nil {
		return nil, err
	}

	genesis, err := readSimnetGenesisFile()
	if err != nil {
		return nil, err
	}

	b := &bundler{
		pk:                pk,
		address:           crypto.PubkeyToAddress(pk.PublicKey),
		entryPointAddress: getEntryPointAddress(),
		entryPoint:        entryPoint,
		client:            client,
		chainCfg:          genesis.Config,
	}

	b.handlers = map[string]func(w http.ResponseWriter, req *rpcRequest){
		"eth_getUserOperationReceipt":  unsupportedEndpoint,
		"eth_supportedEntryPoints":     b.handleSupportedEntryPoints,
		"eth_getUserOperationByHash":   unsupportedEndpoint,
		"eth_sendUserOperation":        b.handleSendUserOperation,
		"eth_estimateUserOperationGas": unsupportedEndpoint,
	}

	return b, nil
}

func (b *bundler) waitForFunding(ctx context.Context) error {
	// wait 1 minute for the bundler to be funded
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for i := 0; i < 120; i++ {
		if i%10 == 0 {
			fmt.Println("Waiting for bundler to be funded...")
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			var result hexutil.Big
			err := b.client.CallContext(ctx, &result, "eth_getBalance", b.address, "pending")
			if err != nil {
				return err
			}

			if result.ToInt().Cmp(big.NewInt(0)) > 0 {
				fmt.Println("Bundler balance is", result.ToInt().String())
				return nil
			}
		}
	}

	return fmt.Errorf("bundler not funded after 2 minutes")
}

type userOperationParam struct {
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

func (param *userOperationParam) userOp() (*UserOperation, error) {
	sender := common.HexToAddress(param.Sender)

	nonce := new(big.Int)
	nonce, ok := nonce.SetString(param.Nonce, 16)
	if !ok {
		return nil, fmt.Errorf("invalid nonce: %s", param.Nonce)
	}

	callGasLimit := new(big.Int)
	callGasLimit, ok = callGasLimit.SetString(param.CallGasLimit, 16)
	if !ok {
		return nil, fmt.Errorf("invalid call gas limit: %s", param.CallGasLimit)
	}

	verificationGasLimit := new(big.Int)
	verificationGasLimit, ok = verificationGasLimit.SetString(param.VerificationGasLimit, 16)
	if !ok {
		return nil, fmt.Errorf("invalid verification gas limit: %s", param.VerificationGasLimit)
	}

	preVerificationGas := new(big.Int)
	preVerificationGas, ok = preVerificationGas.SetString(param.PreVerificationGas, 16)
	if !ok {
		return nil, fmt.Errorf("invalid pre verification gas: %s", param.PreVerificationGas)
	}

	maxFeePerGas := new(big.Int)
	maxFeePerGas, ok = maxFeePerGas.SetString(param.MaxFeePerGas, 16)
	if !ok {
		return nil, fmt.Errorf("invalid max fee per gas: %s", param.MaxFeePerGas)
	}

	maxPriorityFeePerGas := new(big.Int)
	maxPriorityFeePerGas, ok = maxPriorityFeePerGas.SetString(param.MaxPriorityFeePerGas, 16)
	if !ok {
		return nil, fmt.Errorf("invalid max priority fee per gas: %s", param.MaxPriorityFeePerGas)
	}

	return &UserOperation{
		Sender:               sender,
		Nonce:                nonce,
		InitCode:             common.FromHex(param.InitCode),
		CallData:             common.FromHex(param.CallData),
		CallGasLimit:         callGasLimit,
		VerificationGasLimit: verificationGasLimit,
		PreVerificationGas:   preVerificationGas,
		MaxFeePerGas:         maxFeePerGas,
		MaxPriorityFeePerGas: maxPriorityFeePerGas,
		PaymasterAndData:     common.FromHex(param.PaymasterAndData),
		Signature:            common.FromHex(param.Signature),
	}, nil
}

func (b *bundler) nonce() (uint64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var result hexutil.Uint64
	err := b.client.CallContext(ctx, &result, "eth_getTransactionCount", b.address, "pending")
	if err != nil {
		return 0, err
	}

	return uint64(result), nil
}

func (b *bundler) bestHeader() (*types.Header, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var head *types.Header
	err := b.client.CallContext(ctx, &head, "eth_getBlockByNumber", "latest", false)
	if err == nil && head == nil {
		return nil, fmt.Errorf("failed to get latest block")
	}

	return head, err
}

func (b *bundler) currentFees() (baseFees, tipCap *big.Int, err error) {
	hdr, err := b.bestHeader()
	if err != nil {
		return nil, nil, err
	}

	baseFees = eip1559.CalcBaseFee(b.chainCfg, hdr)

	if baseFees.Cmp(ethconfig.Defaults.Miner.GasPrice) < 0 {
		baseFees.Set(ethconfig.Defaults.Miner.GasPrice)
	}

	return baseFees, dexeth.GweiToWei(2), nil
}

func (b *bundler) newTxOpts() (*bind.TransactOpts, error) {
	nonce, err := b.nonce()
	if err != nil {
		return nil, err
	}

	baseFees, tipCap, err := b.currentFees()
	if err != nil {
		return nil, err
	}

	signer := types.LatestSigner(params.AllEthashProtocolChanges)

	return &bind.TransactOpts{
		From:  b.address,
		Nonce: big.NewInt(int64(nonce)),
		Signer: func(address common.Address, tx *types.Transaction) (*types.Transaction, error) {
			return types.SignTx(tx, signer, b.pk)
		},
		GasFeeCap: new(big.Int).Mul(baseFees, big.NewInt(2)),
		GasTipCap: new(big.Int).Mul(tipCap, big.NewInt(2)),
		GasLimit:  500_000, // TODO
	}, nil
}

func (b *bundler) handleSendUserOperation(w http.ResponseWriter, req *rpcRequest) {
	userOpB, ok := req.Params[0].([]byte)
	if !ok {
		http.Error(w, "Invalid user operation", http.StatusBadRequest)
	}

	var op userOperationParam
	if err := json.Unmarshal(userOpB, &op); err != nil {
		http.Error(w, "Error unmarshalling user operation", http.StatusBadRequest)
		return
	}

	// second param is string specifying the entry point address. If not
	// equal to the entrypoint address, return an error.
	entryPointAddr, ok := req.Params[1].(string)
	if !ok {
		http.Error(w, "Invalid entry point address", http.StatusBadRequest)
		return
	}

	if entryPointAddr != b.entryPointAddress.String() {
		http.Error(w, "Invalid entry point address", http.StatusBadRequest)
		return
	}

	userOp, err := op.userOp()
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	txOpts, err := b.newTxOpts()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	tx, err := b.entryPoint.HandleOps(txOpts, []UserOperation{*userOp}, b.address)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	fmt.Println("User op sent:", tx.Hash().String())

	userOpHash, err := b.entryPoint.GetUserOpHash(&bind.CallOpts{
		From:    b.address,
		Context: context.Background(),
	}, *userOp)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	resp := rpcResponse{
		JSONRPC: req.JSONRPC,
		ID:      req.ID,
		Result:  common.Hash(userOpHash).String(),
	}

	respBytes, err := json.Marshal(resp)
	if err != nil {
		http.Error(w, "Error marshalling response", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Write(respBytes)
}

func (b *bundler) handleSupportedEntryPoints(w http.ResponseWriter, req *rpcRequest) {
	resp := rpcResponse{
		JSONRPC: req.JSONRPC,
		ID:      req.ID,
		Result:  []string{b.entryPointAddress.String()},
	}

	respBytes, err := json.Marshal(resp)
	if err != nil {
		fmt.Println("Error marshalling response:", err)
		http.Error(w, "Error marshalling response", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")

	w.Write(respBytes)
}

func (b *bundler) handleRequest(w http.ResponseWriter, reqBody []byte) bool {
	var req rpcRequest
	if err := json.Unmarshal(reqBody, &req); err != nil {
		http.Error(w, "Error unmarshalling request", http.StatusBadRequest)
		return false
	}

	handler, ok := b.handlers[req.Method]
	if !ok {
		http.Error(w, "Unsupported endpoint", http.StatusNotFound)
		return false
	}

	handler(w, &req)
	return true
}

func unsupportedEndpoint(w http.ResponseWriter, req *rpcRequest) {
	http.Error(w, "Unsupported endpoint", http.StatusNotFound)
}

func getEntryPointAddress() common.Address {
	usr, err := user.Current()
	if err != nil {
		panic(err)
	}

	harnessDir := filepath.Join(usr.HomeDir, "dextest", "eth")
	fi, err := os.Stat(harnessDir)
	if err != nil {
		panic(err)
	}
	if !fi.IsDir() {
		panic(fmt.Errorf("%s is not a directory", harnessDir))
	}

	fileName := filepath.Join(harnessDir, "entrypoint_address.txt")

	addrBytes, err := os.ReadFile(fileName)
	if err != nil {
		if os.IsNotExist(err) {
			panic(fmt.Errorf("contract address file not found: %v", fileName))
		}
		panic(err)
	}
	addrLen := len(addrBytes)
	if addrLen == 0 {
		panic(fmt.Errorf("contract address file is empty: %v", fileName))
	}

	addrStr := string(addrBytes[:addrLen-1])
	return common.HexToAddress(addrStr)
}

func forwardRequest(w http.ResponseWriter, r *http.Request, body []byte) {
	forwardReq, err := http.NewRequest(r.Method, alphaHTTPAddress, bytes.NewReader(body))
	if err != nil {
		fmt.Println("Error creating forward request:", err)
		http.Error(w, "Error creating forward request", http.StatusInternalServerError)
		return
	}

	// Copy headers from the original request to the forward request
	for k, v := range r.Header {
		forwardReq.Header[k] = v
	}

	forwardClient := http.Client{}
	forwardResp, err := forwardClient.Do(forwardReq)
	if err != nil {
		fmt.Println("Error forwarding request:", err)
		http.Error(w, "Error forwarding request", http.StatusInternalServerError)
		return
	}
	defer forwardResp.Body.Close()

	// Copy the forward response body to the original response
	io.Copy(w, forwardResp.Body)
}

func mainErr() error {
	var privKey string
	flag.StringVar(&privKey, "privkey", "", "private key for the bundler")
	flag.Parse()

	bundler, err := newBundler(privKey)
	if err != nil {
		return err
	}

	if err := bundler.waitForFunding(context.Background()); err != nil {
		return err
	}

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Sec-Websocket-Version") != "" {
			w.WriteHeader(http.StatusNotFound)
			return
		}

		body, err := io.ReadAll(r.Body)
		if err != nil {
			fmt.Println("Error reading request body:", err)
			http.Error(w, "Error reading request body", http.StatusInternalServerError)
			return
		}

		if bundler.handleRequest(w, body) {
			return
		}

		forwardRequest(w, r, body)
	})

	done := make(chan error)
	go func() {
		err := http.ListenAndServe(":38557", nil)
		if err != http.ErrServerClosed {
			done <- err
		}
		close(done)
	}()

	fmt.Println("Bundler server started on :38557")

	return <-done
}

func main() {
	if err := mainErr(); err != nil {
		fmt.Fprint(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}
