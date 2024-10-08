// Code generated - DO NOT EDIT.
// This file is a generated binding and any manual changes will be lost.

package v0

import (
	"errors"
	"math/big"
	"strings"

	ethereum "github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/event"
)

// Reference imports to suppress errors if they are not otherwise used.
var (
	_ = errors.New
	_ = big.NewInt
	_ = strings.NewReader
	_ = ethereum.NotFound
	_ = bind.Bind
	_ = common.Big1
	_ = types.BloomLookup
	_ = event.NewSubscription
	_ = abi.ConvertType
)

// ETHSwapInitiation is an auto generated low-level Go binding around an user-defined struct.
type ETHSwapInitiation struct {
	RefundTimestamp *big.Int
	SecretHash      [32]byte
	Participant     common.Address
	Value           *big.Int
}

// ETHSwapRedemption is an auto generated low-level Go binding around an user-defined struct.
type ETHSwapRedemption struct {
	Secret     [32]byte
	SecretHash [32]byte
}

// ETHSwapSwap is an auto generated low-level Go binding around an user-defined struct.
type ETHSwapSwap struct {
	Secret               [32]byte
	Value                *big.Int
	InitBlockNumber      *big.Int
	RefundBlockTimestamp *big.Int
	Initiator            common.Address
	Participant          common.Address
	State                uint8
}

// UserOperation is an auto generated low-level Go binding around an user-defined struct.
type UserOperation struct {
	Sender               common.Address
	Nonce                *big.Int
	InitCode             []byte
	CallData             []byte
	CallGasLimit         *big.Int
	VerificationGasLimit *big.Int
	PreVerificationGas   *big.Int
	MaxFeePerGas         *big.Int
	MaxPriorityFeePerGas *big.Int
	PaymasterAndData     []byte
	Signature            []byte
}

// ETHSwapMetaData contains all meta data concerning the ETHSwap contract.
var ETHSwapMetaData = &bind.MetaData{
	ABI: "[{\"inputs\":[{\"internalType\":\"addresspayable\",\"name\":\"_entryPoint\",\"type\":\"address\"}],\"stateMutability\":\"nonpayable\",\"type\":\"constructor\"},{\"anonymous\":false,\"inputs\":[{\"indexed\":false,\"internalType\":\"uint256\",\"name\":\"\",\"type\":\"uint256\"}],\"name\":\"LogShiz\",\"type\":\"event\"},{\"inputs\":[{\"components\":[{\"internalType\":\"uint256\",\"name\":\"refundTimestamp\",\"type\":\"uint256\"},{\"internalType\":\"bytes32\",\"name\":\"secretHash\",\"type\":\"bytes32\"},{\"internalType\":\"address\",\"name\":\"participant\",\"type\":\"address\"},{\"internalType\":\"uint256\",\"name\":\"value\",\"type\":\"uint256\"}],\"internalType\":\"structETHSwap.Initiation[]\",\"name\":\"initiations\",\"type\":\"tuple[]\"}],\"name\":\"initiate\",\"outputs\":[],\"stateMutability\":\"payable\",\"type\":\"function\"},{\"inputs\":[{\"internalType\":\"bytes32\",\"name\":\"secretHash\",\"type\":\"bytes32\"}],\"name\":\"isRefundable\",\"outputs\":[{\"internalType\":\"bool\",\"name\":\"\",\"type\":\"bool\"}],\"stateMutability\":\"view\",\"type\":\"function\"},{\"inputs\":[{\"components\":[{\"internalType\":\"bytes32\",\"name\":\"secret\",\"type\":\"bytes32\"},{\"internalType\":\"bytes32\",\"name\":\"secretHash\",\"type\":\"bytes32\"}],\"internalType\":\"structETHSwap.Redemption[]\",\"name\":\"redemptions\",\"type\":\"tuple[]\"}],\"name\":\"redeem\",\"outputs\":[],\"stateMutability\":\"nonpayable\",\"type\":\"function\"},{\"inputs\":[{\"components\":[{\"internalType\":\"bytes32\",\"name\":\"secret\",\"type\":\"bytes32\"},{\"internalType\":\"bytes32\",\"name\":\"secretHash\",\"type\":\"bytes32\"}],\"internalType\":\"structETHSwap.Redemption[]\",\"name\":\"redemptions\",\"type\":\"tuple[]\"}],\"name\":\"redeemAA\",\"outputs\":[],\"stateMutability\":\"nonpayable\",\"type\":\"function\"},{\"inputs\":[{\"internalType\":\"bytes32\",\"name\":\"secretHash\",\"type\":\"bytes32\"}],\"name\":\"refund\",\"outputs\":[],\"stateMutability\":\"nonpayable\",\"type\":\"function\"},{\"inputs\":[{\"internalType\":\"bytes32\",\"name\":\"secretHash\",\"type\":\"bytes32\"}],\"name\":\"swap\",\"outputs\":[{\"components\":[{\"internalType\":\"bytes32\",\"name\":\"secret\",\"type\":\"bytes32\"},{\"internalType\":\"uint256\",\"name\":\"value\",\"type\":\"uint256\"},{\"internalType\":\"uint256\",\"name\":\"initBlockNumber\",\"type\":\"uint256\"},{\"internalType\":\"uint256\",\"name\":\"refundBlockTimestamp\",\"type\":\"uint256\"},{\"internalType\":\"address\",\"name\":\"initiator\",\"type\":\"address\"},{\"internalType\":\"address\",\"name\":\"participant\",\"type\":\"address\"},{\"internalType\":\"enumETHSwap.State\",\"name\":\"state\",\"type\":\"uint8\"}],\"internalType\":\"structETHSwap.Swap\",\"name\":\"\",\"type\":\"tuple\"}],\"stateMutability\":\"view\",\"type\":\"function\"},{\"inputs\":[{\"internalType\":\"bytes32\",\"name\":\"\",\"type\":\"bytes32\"}],\"name\":\"swaps\",\"outputs\":[{\"internalType\":\"bytes32\",\"name\":\"secret\",\"type\":\"bytes32\"},{\"internalType\":\"uint256\",\"name\":\"value\",\"type\":\"uint256\"},{\"internalType\":\"uint256\",\"name\":\"initBlockNumber\",\"type\":\"uint256\"},{\"internalType\":\"uint256\",\"name\":\"refundBlockTimestamp\",\"type\":\"uint256\"},{\"internalType\":\"address\",\"name\":\"initiator\",\"type\":\"address\"},{\"internalType\":\"address\",\"name\":\"participant\",\"type\":\"address\"},{\"internalType\":\"enumETHSwap.State\",\"name\":\"state\",\"type\":\"uint8\"}],\"stateMutability\":\"view\",\"type\":\"function\"},{\"inputs\":[{\"components\":[{\"internalType\":\"address\",\"name\":\"sender\",\"type\":\"address\"},{\"internalType\":\"uint256\",\"name\":\"nonce\",\"type\":\"uint256\"},{\"internalType\":\"bytes\",\"name\":\"initCode\",\"type\":\"bytes\"},{\"internalType\":\"bytes\",\"name\":\"callData\",\"type\":\"bytes\"},{\"internalType\":\"uint256\",\"name\":\"callGasLimit\",\"type\":\"uint256\"},{\"internalType\":\"uint256\",\"name\":\"verificationGasLimit\",\"type\":\"uint256\"},{\"internalType\":\"uint256\",\"name\":\"preVerificationGas\",\"type\":\"uint256\"},{\"internalType\":\"uint256\",\"name\":\"maxFeePerGas\",\"type\":\"uint256\"},{\"internalType\":\"uint256\",\"name\":\"maxPriorityFeePerGas\",\"type\":\"uint256\"},{\"internalType\":\"bytes\",\"name\":\"paymasterAndData\",\"type\":\"bytes\"},{\"internalType\":\"bytes\",\"name\":\"signature\",\"type\":\"bytes\"}],\"internalType\":\"structUserOperation\",\"name\":\"userOp\",\"type\":\"tuple\"},{\"internalType\":\"bytes32\",\"name\":\"\",\"type\":\"bytes32\"},{\"internalType\":\"uint256\",\"name\":\"missingAccountFunds\",\"type\":\"uint256\"}],\"name\":\"validateUserOp\",\"outputs\":[{\"internalType\":\"uint256\",\"name\":\"validationData\",\"type\":\"uint256\"}],\"stateMutability\":\"nonpayable\",\"type\":\"function\"}]",
	Bin: "0x608060405234801561001057600080fd5b5060405161152538038061152583398101604081905261002f91610054565b600080546001600160a01b0319166001600160a01b0392909216919091179055610084565b60006020828403121561006657600080fd5b81516001600160a01b038116811461007d57600080fd5b9392505050565b611492806100936000396000f3fe60806040526004361061007b5760003560e01c8063a8793f941161004e578063a8793f9414610122578063d0f761c014610135578063eb84e7f214610165578063f4fd17f9146101e257600080fd5b80633a871cdd146100805780637249fbb6146100b357806376467cbd146100d557806376696f5114610102575b600080fd5b34801561008c57600080fd5b506100a061009b366004610f14565b610202565b6040519081526020015b60405180910390f35b3480156100bf57600080fd5b506100d36100ce366004610f68565b610507565b005b3480156100e157600080fd5b506100f56100f0366004610f68565b610614565b6040516100aa9190610fb9565b34801561010e57600080fd5b506100d361011d36600461101e565b6106f0565b6100d3610130366004611093565b6109fb565b34801561014157600080fd5b50610155610150366004610f68565b610bf3565b60405190151581526020016100aa565b34801561017157600080fd5b506101cf610180366004610f68565b60016020819052600091825260409091208054918101546002820154600383015460048401546005909401549293919290916001600160a01b039081169190811690600160a01b900460ff1687565b6040516100aa97969594939291906110f6565b3480156101ee57600080fd5b506100d36101fd36600461101e565b610c39565b600080546001600160a01b031633146102595760405162461bcd60e51b81526020600482015260146024820152731cd95b99195c88084f48195b9d1c9e541bda5b9d60621b60448201526064015b60405180910390fd5b6040516103e781527fc31d59c3affd669cc3ae199e4875ad4b814521f3b5b55395d8b9d880bc65d5ee9060200160405180910390a16040518281527fc31d59c3affd669cc3ae199e4875ad4b814521f3b5b55395d8b9d880bc65d5ee9060200160405180910390a160046102d06060860186611142565b905010156102e057506001610500565b6376696f5160e01b6102f56060860186611142565b61030491600491600091611190565b61030d916111ba565b6001600160e01b0319161461032457506002610500565b60006103336060860186611142565b610341916004908290611190565b81019061034e919061125a565b905060008060005b83518110156104da57600084828151811061037357610373611327565b60200260200101519050600060016000836020015181526020019081526020016000209050826000036103cc57600581015460208084015160009081526002909152604090208990556001600160a01b031694506103f2565b60058101546001600160a01b038681169116146103f25760039650505050505050610500565b60016005820154600160a01b900460ff16600381111561041457610414610f81565b146104285760049650505050505050610500565b81602001516002836000015160405160200161044691815260200190565b60408051601f19818403018152908290526104609161133d565b602060405180830381855afa15801561047d573d6000803e3d6000fd5b5050506040513d601f19601f820116820180604052508101906104a0919061136c565b146104b45760059650505050505050610500565b60018101546104c3908561139b565b9350505080806104d2906113b4565b915050610356565b50808511156104ef5760069350505050610500565b6104f885610eba565b600093505050505b9392505050565b3233146105265760405162461bcd60e51b8152600401610250906113cd565b61052f81610bf3565b61056c5760405162461bcd60e51b815260206004820152600e60248201526d6e6f7420726566756e6461626c6560901b6044820152606401610250565b600081815260016020819052604080832060058101805460ff60a01b1916600360a01b17905560048101549281015491519093926001600160a01b031691908381818185875af1925050503d80600081146105e3576040519150601f19603f3d011682016040523d82523d6000602084013e6105e8565b606091505b509091505060018115151461060f5760405162461bcd60e51b8152600401610250906113f7565b505050565b6106516040805160e081018252600080825260208201819052918101829052606081018290526080810182905260a081018290529060c082015290565b600082815260016020818152604092839020835160e081018552815481529281015491830191909152600281015492820192909252600380830154606083015260048301546001600160a01b039081166080840152600584015490811660a084015291929160c0840191600160a01b90910460ff16908111156106d6576106d6610f81565b60038111156106e7576106e7610f81565b90525092915050565b6000546001600160a01b031633146107415760405162461bcd60e51b81526020600482015260146024820152731cd95b99195c88084f48195b9d1c9e541bda5b9d60621b6044820152606401610250565b600080805b8381101561093e573685858381811061076157610761611327565b9050604002019050600060016000836020013581526020019081526020016000209050826000036107a15760058101546001600160a01b031693506107f2565b60058101546001600160a01b038581169116146107f25760405162461bcd60e51b815260206004820152600f60248201526e189859081c185c9d1a58da5c185b9d608a1b6044820152606401610250565b60016005820154600160a01b900460ff16600381111561081457610814610f81565b1461084d5760405162461bcd60e51b815260206004820152600960248201526862616420737461746560b81b6044820152606401610250565b81602001356002836000013560405160200161086b91815260200190565b60408051601f19818403018152908290526108859161133d565b602060405180830381855afa1580156108a2573d6000803e3d6000fd5b5050506040513d601f19601f820116820180604052508101906108c5919061136c565b146108ff5760405162461bcd60e51b815260206004820152600a602482015269189859081cd958dc995d60b21b6044820152606401610250565b60058101805460ff60a01b1916600160a11b179055813581556001810154610927908661139b565b945050508080610936906113b4565b915050610746565b506000600260008686600081811061095857610958611327565b9050604002016020013581526020019081526020016000205490506000826001600160a01b0316828561098b9190611420565b604051600081818185875af1925050503d80600081146109c7576040519150601f19603f3d011682016040523d82523d6000602084013e6109cc565b606091505b50909150506001811515146109f35760405162461bcd60e51b8152600401610250906113f7565b505050505050565b323314610a1a5760405162461bcd60e51b8152600401610250906113cd565b6000805b82811015610bb95736848483818110610a3957610a39611327565b90506080020190506000600160008360200135815260200190815260200160002090506000826060013511610a985760405162461bcd60e51b81526020600482015260056024820152640c081d985b60da1b6044820152606401610250565b8135610ada5760405162461bcd60e51b815260206004820152601160248201527003020726566756e6454696d657374616d7607c1b6044820152606401610250565b60006005820154600160a01b900460ff166003811115610afc57610afc610f81565b14610b345760405162461bcd60e51b8152602060048201526008602482015267064757020737761760c41b6044820152606401610250565b436002820155813560038201556004810180546001600160a01b03191633179055610b656060830160408401611433565b6005820180546060850135600185018190556001600160a01b03939093166001600160a81b031990911617600160a01b179055610ba2908561139b565b935050508080610bb1906113b4565b915050610a1e565b5034811461060f5760405162461bcd60e51b8152602060048201526007602482015266189859081d985b60ca1b6044820152606401610250565b60008181526001602081905260408220906005820154600160a01b900460ff166003811115610c2457610c24610f81565b14801561050057506003015442101592915050565b323314610c585760405162461bcd60e51b8152600401610250906113cd565b6000805b82811015610e455736848483818110610c7757610c77611327565b905060400201905060006001600083602001358152602001908152602001600020905060016003811115610cad57610cad610f81565b6005820154600160a01b900460ff166003811115610ccd57610ccd610f81565b14610d065760405162461bcd60e51b815260206004820152600960248201526862616420737461746560b81b6044820152606401610250565b60058101546001600160a01b03163314610d545760405162461bcd60e51b815260206004820152600f60248201526e189859081c185c9d1a58da5c185b9d608a1b6044820152606401610250565b816020013560028360000135604051602001610d7291815260200190565b60408051601f1981840301815290829052610d8c9161133d565b602060405180830381855afa158015610da9573d6000803e3d6000fd5b5050506040513d601f19601f82011682018060405250810190610dcc919061136c565b14610e065760405162461bcd60e51b815260206004820152600a602482015269189859081cd958dc995d60b21b6044820152606401610250565b60058101805460ff60a01b1916600160a11b179055813581556001810154610e2e908561139b565b935050508080610e3d906113b4565b915050610c5c565b50604051600090339083908381818185875af1925050503d8060008114610e88576040519150601f19603f3d011682016040523d82523d6000602084013e610e8d565b606091505b5090915050600181151514610eb45760405162461bcd60e51b8152600401610250906113f7565b50505050565b8015610f1157604051600090339060001990849084818181858888f193505050503d8060008114610f07576040519150601f19603f3d011682016040523d82523d6000602084013e610f0c565b606091505b505050505b50565b600080600060608486031215610f2957600080fd5b833567ffffffffffffffff811115610f4057600080fd5b84016101608187031215610f5357600080fd5b95602085013595506040909401359392505050565b600060208284031215610f7a57600080fd5b5035919050565b634e487b7160e01b600052602160045260246000fd5b60048110610fb557634e487b7160e01b600052602160045260246000fd5b9052565b600060e08201905082518252602083015160208301526040830151604083015260608301516060830152608083015160018060a01b0380821660808501528060a08601511660a0850152505060c083015161101760c0840182610f97565b5092915050565b6000806020838503121561103157600080fd5b823567ffffffffffffffff8082111561104957600080fd5b818501915085601f83011261105d57600080fd5b81358181111561106c57600080fd5b8660208260061b850101111561108157600080fd5b60209290920196919550909350505050565b600080602083850312156110a657600080fd5b823567ffffffffffffffff808211156110be57600080fd5b818501915085601f8301126110d257600080fd5b8135818111156110e157600080fd5b8660208260071b850101111561108157600080fd5b8781526020810187905260408101869052606081018590526001600160a01b038481166080830152831660a082015260e0810161113660c0830184610f97565b98975050505050505050565b6000808335601e1984360301811261115957600080fd5b83018035915067ffffffffffffffff82111561117457600080fd5b60200191503681900382131561118957600080fd5b9250929050565b600080858511156111a057600080fd5b838611156111ad57600080fd5b5050820193919092039150565b6001600160e01b031981358181169160048510156111e25780818660040360031b1b83161692505b505092915050565b634e487b7160e01b600052604160045260246000fd5b6040805190810167ffffffffffffffff81118282101715611223576112236111ea565b60405290565b604051601f8201601f1916810167ffffffffffffffff81118282101715611252576112526111ea565b604052919050565b6000602080838503121561126d57600080fd5b823567ffffffffffffffff8082111561128557600080fd5b818501915085601f83011261129957600080fd5b8135818111156112ab576112ab6111ea565b6112b9848260051b01611229565b818152848101925060069190911b8301840190878211156112d957600080fd5b928401925b8184101561131c57604084890312156112f75760008081fd5b6112ff611200565b8435815285850135868201528352604090930192918401916112de565b979650505050505050565b634e487b7160e01b600052603260045260246000fd5b6000825160005b8181101561135e5760208186018101518583015201611344565b506000920191825250919050565b60006020828403121561137e57600080fd5b5051919050565b634e487b7160e01b600052601160045260246000fd5b808201808211156113ae576113ae611385565b92915050565b6000600182016113c6576113c6611385565b5060010190565b60208082526010908201526f39b2b73232b910109e9037b934b3b4b760811b604082015260600190565b6020808252600f908201526e1d1c985b9cd9995c8819985a5b1959608a1b604082015260600190565b818103818111156113ae576113ae611385565b60006020828403121561144557600080fd5b81356001600160a01b038116811461050057600080fdfea2646970667358221220565a864f098990cc020c22347c61f347aac355a62e01f1782ec747092982869f64736f6c63430008120033",
}

// ETHSwapABI is the input ABI used to generate the binding from.
// Deprecated: Use ETHSwapMetaData.ABI instead.
var ETHSwapABI = ETHSwapMetaData.ABI

// ETHSwapBin is the compiled bytecode used for deploying new contracts.
// Deprecated: Use ETHSwapMetaData.Bin instead.
var ETHSwapBin = ETHSwapMetaData.Bin

// DeployETHSwap deploys a new Ethereum contract, binding an instance of ETHSwap to it.
func DeployETHSwap(auth *bind.TransactOpts, backend bind.ContractBackend, _entryPoint common.Address) (common.Address, *types.Transaction, *ETHSwap, error) {
	parsed, err := ETHSwapMetaData.GetAbi()
	if err != nil {
		return common.Address{}, nil, nil, err
	}
	if parsed == nil {
		return common.Address{}, nil, nil, errors.New("GetABI returned nil")
	}

	address, tx, contract, err := bind.DeployContract(auth, *parsed, common.FromHex(ETHSwapBin), backend, _entryPoint)
	if err != nil {
		return common.Address{}, nil, nil, err
	}
	return address, tx, &ETHSwap{ETHSwapCaller: ETHSwapCaller{contract: contract}, ETHSwapTransactor: ETHSwapTransactor{contract: contract}, ETHSwapFilterer: ETHSwapFilterer{contract: contract}}, nil
}

// ETHSwap is an auto generated Go binding around an Ethereum contract.
type ETHSwap struct {
	ETHSwapCaller     // Read-only binding to the contract
	ETHSwapTransactor // Write-only binding to the contract
	ETHSwapFilterer   // Log filterer for contract events
}

// ETHSwapCaller is an auto generated read-only Go binding around an Ethereum contract.
type ETHSwapCaller struct {
	contract *bind.BoundContract // Generic contract wrapper for the low level calls
}

// ETHSwapTransactor is an auto generated write-only Go binding around an Ethereum contract.
type ETHSwapTransactor struct {
	contract *bind.BoundContract // Generic contract wrapper for the low level calls
}

// ETHSwapFilterer is an auto generated log filtering Go binding around an Ethereum contract events.
type ETHSwapFilterer struct {
	contract *bind.BoundContract // Generic contract wrapper for the low level calls
}

// ETHSwapSession is an auto generated Go binding around an Ethereum contract,
// with pre-set call and transact options.
type ETHSwapSession struct {
	Contract     *ETHSwap          // Generic contract binding to set the session for
	CallOpts     bind.CallOpts     // Call options to use throughout this session
	TransactOpts bind.TransactOpts // Transaction auth options to use throughout this session
}

// ETHSwapCallerSession is an auto generated read-only Go binding around an Ethereum contract,
// with pre-set call options.
type ETHSwapCallerSession struct {
	Contract *ETHSwapCaller // Generic contract caller binding to set the session for
	CallOpts bind.CallOpts  // Call options to use throughout this session
}

// ETHSwapTransactorSession is an auto generated write-only Go binding around an Ethereum contract,
// with pre-set transact options.
type ETHSwapTransactorSession struct {
	Contract     *ETHSwapTransactor // Generic contract transactor binding to set the session for
	TransactOpts bind.TransactOpts  // Transaction auth options to use throughout this session
}

// ETHSwapRaw is an auto generated low-level Go binding around an Ethereum contract.
type ETHSwapRaw struct {
	Contract *ETHSwap // Generic contract binding to access the raw methods on
}

// ETHSwapCallerRaw is an auto generated low-level read-only Go binding around an Ethereum contract.
type ETHSwapCallerRaw struct {
	Contract *ETHSwapCaller // Generic read-only contract binding to access the raw methods on
}

// ETHSwapTransactorRaw is an auto generated low-level write-only Go binding around an Ethereum contract.
type ETHSwapTransactorRaw struct {
	Contract *ETHSwapTransactor // Generic write-only contract binding to access the raw methods on
}

// NewETHSwap creates a new instance of ETHSwap, bound to a specific deployed contract.
func NewETHSwap(address common.Address, backend bind.ContractBackend) (*ETHSwap, error) {
	contract, err := bindETHSwap(address, backend, backend, backend)
	if err != nil {
		return nil, err
	}
	return &ETHSwap{ETHSwapCaller: ETHSwapCaller{contract: contract}, ETHSwapTransactor: ETHSwapTransactor{contract: contract}, ETHSwapFilterer: ETHSwapFilterer{contract: contract}}, nil
}

// NewETHSwapCaller creates a new read-only instance of ETHSwap, bound to a specific deployed contract.
func NewETHSwapCaller(address common.Address, caller bind.ContractCaller) (*ETHSwapCaller, error) {
	contract, err := bindETHSwap(address, caller, nil, nil)
	if err != nil {
		return nil, err
	}
	return &ETHSwapCaller{contract: contract}, nil
}

// NewETHSwapTransactor creates a new write-only instance of ETHSwap, bound to a specific deployed contract.
func NewETHSwapTransactor(address common.Address, transactor bind.ContractTransactor) (*ETHSwapTransactor, error) {
	contract, err := bindETHSwap(address, nil, transactor, nil)
	if err != nil {
		return nil, err
	}
	return &ETHSwapTransactor{contract: contract}, nil
}

// NewETHSwapFilterer creates a new log filterer instance of ETHSwap, bound to a specific deployed contract.
func NewETHSwapFilterer(address common.Address, filterer bind.ContractFilterer) (*ETHSwapFilterer, error) {
	contract, err := bindETHSwap(address, nil, nil, filterer)
	if err != nil {
		return nil, err
	}
	return &ETHSwapFilterer{contract: contract}, nil
}

// bindETHSwap binds a generic wrapper to an already deployed contract.
func bindETHSwap(address common.Address, caller bind.ContractCaller, transactor bind.ContractTransactor, filterer bind.ContractFilterer) (*bind.BoundContract, error) {
	parsed, err := ETHSwapMetaData.GetAbi()
	if err != nil {
		return nil, err
	}
	return bind.NewBoundContract(address, *parsed, caller, transactor, filterer), nil
}

// Call invokes the (constant) contract method with params as input values and
// sets the output to result. The result type might be a single field for simple
// returns, a slice of interfaces for anonymous returns and a struct for named
// returns.
func (_ETHSwap *ETHSwapRaw) Call(opts *bind.CallOpts, result *[]interface{}, method string, params ...interface{}) error {
	return _ETHSwap.Contract.ETHSwapCaller.contract.Call(opts, result, method, params...)
}

// Transfer initiates a plain transaction to move funds to the contract, calling
// its default method if one is available.
func (_ETHSwap *ETHSwapRaw) Transfer(opts *bind.TransactOpts) (*types.Transaction, error) {
	return _ETHSwap.Contract.ETHSwapTransactor.contract.Transfer(opts)
}

// Transact invokes the (paid) contract method with params as input values.
func (_ETHSwap *ETHSwapRaw) Transact(opts *bind.TransactOpts, method string, params ...interface{}) (*types.Transaction, error) {
	return _ETHSwap.Contract.ETHSwapTransactor.contract.Transact(opts, method, params...)
}

// Call invokes the (constant) contract method with params as input values and
// sets the output to result. The result type might be a single field for simple
// returns, a slice of interfaces for anonymous returns and a struct for named
// returns.
func (_ETHSwap *ETHSwapCallerRaw) Call(opts *bind.CallOpts, result *[]interface{}, method string, params ...interface{}) error {
	return _ETHSwap.Contract.contract.Call(opts, result, method, params...)
}

// Transfer initiates a plain transaction to move funds to the contract, calling
// its default method if one is available.
func (_ETHSwap *ETHSwapTransactorRaw) Transfer(opts *bind.TransactOpts) (*types.Transaction, error) {
	return _ETHSwap.Contract.contract.Transfer(opts)
}

// Transact invokes the (paid) contract method with params as input values.
func (_ETHSwap *ETHSwapTransactorRaw) Transact(opts *bind.TransactOpts, method string, params ...interface{}) (*types.Transaction, error) {
	return _ETHSwap.Contract.contract.Transact(opts, method, params...)
}

// IsRefundable is a free data retrieval call binding the contract method 0xd0f761c0.
//
// Solidity: function isRefundable(bytes32 secretHash) view returns(bool)
func (_ETHSwap *ETHSwapCaller) IsRefundable(opts *bind.CallOpts, secretHash [32]byte) (bool, error) {
	var out []interface{}
	err := _ETHSwap.contract.Call(opts, &out, "isRefundable", secretHash)

	if err != nil {
		return *new(bool), err
	}

	out0 := *abi.ConvertType(out[0], new(bool)).(*bool)

	return out0, err

}

// IsRefundable is a free data retrieval call binding the contract method 0xd0f761c0.
//
// Solidity: function isRefundable(bytes32 secretHash) view returns(bool)
func (_ETHSwap *ETHSwapSession) IsRefundable(secretHash [32]byte) (bool, error) {
	return _ETHSwap.Contract.IsRefundable(&_ETHSwap.CallOpts, secretHash)
}

// IsRefundable is a free data retrieval call binding the contract method 0xd0f761c0.
//
// Solidity: function isRefundable(bytes32 secretHash) view returns(bool)
func (_ETHSwap *ETHSwapCallerSession) IsRefundable(secretHash [32]byte) (bool, error) {
	return _ETHSwap.Contract.IsRefundable(&_ETHSwap.CallOpts, secretHash)
}

// Swap is a free data retrieval call binding the contract method 0x76467cbd.
//
// Solidity: function swap(bytes32 secretHash) view returns((bytes32,uint256,uint256,uint256,address,address,uint8))
func (_ETHSwap *ETHSwapCaller) Swap(opts *bind.CallOpts, secretHash [32]byte) (ETHSwapSwap, error) {
	var out []interface{}
	err := _ETHSwap.contract.Call(opts, &out, "swap", secretHash)

	if err != nil {
		return *new(ETHSwapSwap), err
	}

	out0 := *abi.ConvertType(out[0], new(ETHSwapSwap)).(*ETHSwapSwap)

	return out0, err

}

// Swap is a free data retrieval call binding the contract method 0x76467cbd.
//
// Solidity: function swap(bytes32 secretHash) view returns((bytes32,uint256,uint256,uint256,address,address,uint8))
func (_ETHSwap *ETHSwapSession) Swap(secretHash [32]byte) (ETHSwapSwap, error) {
	return _ETHSwap.Contract.Swap(&_ETHSwap.CallOpts, secretHash)
}

// Swap is a free data retrieval call binding the contract method 0x76467cbd.
//
// Solidity: function swap(bytes32 secretHash) view returns((bytes32,uint256,uint256,uint256,address,address,uint8))
func (_ETHSwap *ETHSwapCallerSession) Swap(secretHash [32]byte) (ETHSwapSwap, error) {
	return _ETHSwap.Contract.Swap(&_ETHSwap.CallOpts, secretHash)
}

// Swaps is a free data retrieval call binding the contract method 0xeb84e7f2.
//
// Solidity: function swaps(bytes32 ) view returns(bytes32 secret, uint256 value, uint256 initBlockNumber, uint256 refundBlockTimestamp, address initiator, address participant, uint8 state)
func (_ETHSwap *ETHSwapCaller) Swaps(opts *bind.CallOpts, arg0 [32]byte) (struct {
	Secret               [32]byte
	Value                *big.Int
	InitBlockNumber      *big.Int
	RefundBlockTimestamp *big.Int
	Initiator            common.Address
	Participant          common.Address
	State                uint8
}, error) {
	var out []interface{}
	err := _ETHSwap.contract.Call(opts, &out, "swaps", arg0)

	outstruct := new(struct {
		Secret               [32]byte
		Value                *big.Int
		InitBlockNumber      *big.Int
		RefundBlockTimestamp *big.Int
		Initiator            common.Address
		Participant          common.Address
		State                uint8
	})
	if err != nil {
		return *outstruct, err
	}

	outstruct.Secret = *abi.ConvertType(out[0], new([32]byte)).(*[32]byte)
	outstruct.Value = *abi.ConvertType(out[1], new(*big.Int)).(**big.Int)
	outstruct.InitBlockNumber = *abi.ConvertType(out[2], new(*big.Int)).(**big.Int)
	outstruct.RefundBlockTimestamp = *abi.ConvertType(out[3], new(*big.Int)).(**big.Int)
	outstruct.Initiator = *abi.ConvertType(out[4], new(common.Address)).(*common.Address)
	outstruct.Participant = *abi.ConvertType(out[5], new(common.Address)).(*common.Address)
	outstruct.State = *abi.ConvertType(out[6], new(uint8)).(*uint8)

	return *outstruct, err

}

// Swaps is a free data retrieval call binding the contract method 0xeb84e7f2.
//
// Solidity: function swaps(bytes32 ) view returns(bytes32 secret, uint256 value, uint256 initBlockNumber, uint256 refundBlockTimestamp, address initiator, address participant, uint8 state)
func (_ETHSwap *ETHSwapSession) Swaps(arg0 [32]byte) (struct {
	Secret               [32]byte
	Value                *big.Int
	InitBlockNumber      *big.Int
	RefundBlockTimestamp *big.Int
	Initiator            common.Address
	Participant          common.Address
	State                uint8
}, error) {
	return _ETHSwap.Contract.Swaps(&_ETHSwap.CallOpts, arg0)
}

// Swaps is a free data retrieval call binding the contract method 0xeb84e7f2.
//
// Solidity: function swaps(bytes32 ) view returns(bytes32 secret, uint256 value, uint256 initBlockNumber, uint256 refundBlockTimestamp, address initiator, address participant, uint8 state)
func (_ETHSwap *ETHSwapCallerSession) Swaps(arg0 [32]byte) (struct {
	Secret               [32]byte
	Value                *big.Int
	InitBlockNumber      *big.Int
	RefundBlockTimestamp *big.Int
	Initiator            common.Address
	Participant          common.Address
	State                uint8
}, error) {
	return _ETHSwap.Contract.Swaps(&_ETHSwap.CallOpts, arg0)
}

// Initiate is a paid mutator transaction binding the contract method 0xa8793f94.
//
// Solidity: function initiate((uint256,bytes32,address,uint256)[] initiations) payable returns()
func (_ETHSwap *ETHSwapTransactor) Initiate(opts *bind.TransactOpts, initiations []ETHSwapInitiation) (*types.Transaction, error) {
	return _ETHSwap.contract.Transact(opts, "initiate", initiations)
}

// Initiate is a paid mutator transaction binding the contract method 0xa8793f94.
//
// Solidity: function initiate((uint256,bytes32,address,uint256)[] initiations) payable returns()
func (_ETHSwap *ETHSwapSession) Initiate(initiations []ETHSwapInitiation) (*types.Transaction, error) {
	return _ETHSwap.Contract.Initiate(&_ETHSwap.TransactOpts, initiations)
}

// Initiate is a paid mutator transaction binding the contract method 0xa8793f94.
//
// Solidity: function initiate((uint256,bytes32,address,uint256)[] initiations) payable returns()
func (_ETHSwap *ETHSwapTransactorSession) Initiate(initiations []ETHSwapInitiation) (*types.Transaction, error) {
	return _ETHSwap.Contract.Initiate(&_ETHSwap.TransactOpts, initiations)
}

// Redeem is a paid mutator transaction binding the contract method 0xf4fd17f9.
//
// Solidity: function redeem((bytes32,bytes32)[] redemptions) returns()
func (_ETHSwap *ETHSwapTransactor) Redeem(opts *bind.TransactOpts, redemptions []ETHSwapRedemption) (*types.Transaction, error) {
	return _ETHSwap.contract.Transact(opts, "redeem", redemptions)
}

// Redeem is a paid mutator transaction binding the contract method 0xf4fd17f9.
//
// Solidity: function redeem((bytes32,bytes32)[] redemptions) returns()
func (_ETHSwap *ETHSwapSession) Redeem(redemptions []ETHSwapRedemption) (*types.Transaction, error) {
	return _ETHSwap.Contract.Redeem(&_ETHSwap.TransactOpts, redemptions)
}

// Redeem is a paid mutator transaction binding the contract method 0xf4fd17f9.
//
// Solidity: function redeem((bytes32,bytes32)[] redemptions) returns()
func (_ETHSwap *ETHSwapTransactorSession) Redeem(redemptions []ETHSwapRedemption) (*types.Transaction, error) {
	return _ETHSwap.Contract.Redeem(&_ETHSwap.TransactOpts, redemptions)
}

// RedeemAA is a paid mutator transaction binding the contract method 0x76696f51.
//
// Solidity: function redeemAA((bytes32,bytes32)[] redemptions) returns()
func (_ETHSwap *ETHSwapTransactor) RedeemAA(opts *bind.TransactOpts, redemptions []ETHSwapRedemption) (*types.Transaction, error) {
	return _ETHSwap.contract.Transact(opts, "redeemAA", redemptions)
}

// RedeemAA is a paid mutator transaction binding the contract method 0x76696f51.
//
// Solidity: function redeemAA((bytes32,bytes32)[] redemptions) returns()
func (_ETHSwap *ETHSwapSession) RedeemAA(redemptions []ETHSwapRedemption) (*types.Transaction, error) {
	return _ETHSwap.Contract.RedeemAA(&_ETHSwap.TransactOpts, redemptions)
}

// RedeemAA is a paid mutator transaction binding the contract method 0x76696f51.
//
// Solidity: function redeemAA((bytes32,bytes32)[] redemptions) returns()
func (_ETHSwap *ETHSwapTransactorSession) RedeemAA(redemptions []ETHSwapRedemption) (*types.Transaction, error) {
	return _ETHSwap.Contract.RedeemAA(&_ETHSwap.TransactOpts, redemptions)
}

// Refund is a paid mutator transaction binding the contract method 0x7249fbb6.
//
// Solidity: function refund(bytes32 secretHash) returns()
func (_ETHSwap *ETHSwapTransactor) Refund(opts *bind.TransactOpts, secretHash [32]byte) (*types.Transaction, error) {
	return _ETHSwap.contract.Transact(opts, "refund", secretHash)
}

// Refund is a paid mutator transaction binding the contract method 0x7249fbb6.
//
// Solidity: function refund(bytes32 secretHash) returns()
func (_ETHSwap *ETHSwapSession) Refund(secretHash [32]byte) (*types.Transaction, error) {
	return _ETHSwap.Contract.Refund(&_ETHSwap.TransactOpts, secretHash)
}

// Refund is a paid mutator transaction binding the contract method 0x7249fbb6.
//
// Solidity: function refund(bytes32 secretHash) returns()
func (_ETHSwap *ETHSwapTransactorSession) Refund(secretHash [32]byte) (*types.Transaction, error) {
	return _ETHSwap.Contract.Refund(&_ETHSwap.TransactOpts, secretHash)
}

// ValidateUserOp is a paid mutator transaction binding the contract method 0x3a871cdd.
//
// Solidity: function validateUserOp((address,uint256,bytes,bytes,uint256,uint256,uint256,uint256,uint256,bytes,bytes) userOp, bytes32 , uint256 missingAccountFunds) returns(uint256 validationData)
func (_ETHSwap *ETHSwapTransactor) ValidateUserOp(opts *bind.TransactOpts, userOp UserOperation, arg1 [32]byte, missingAccountFunds *big.Int) (*types.Transaction, error) {
	return _ETHSwap.contract.Transact(opts, "validateUserOp", userOp, arg1, missingAccountFunds)
}

// ValidateUserOp is a paid mutator transaction binding the contract method 0x3a871cdd.
//
// Solidity: function validateUserOp((address,uint256,bytes,bytes,uint256,uint256,uint256,uint256,uint256,bytes,bytes) userOp, bytes32 , uint256 missingAccountFunds) returns(uint256 validationData)
func (_ETHSwap *ETHSwapSession) ValidateUserOp(userOp UserOperation, arg1 [32]byte, missingAccountFunds *big.Int) (*types.Transaction, error) {
	return _ETHSwap.Contract.ValidateUserOp(&_ETHSwap.TransactOpts, userOp, arg1, missingAccountFunds)
}

// ValidateUserOp is a paid mutator transaction binding the contract method 0x3a871cdd.
//
// Solidity: function validateUserOp((address,uint256,bytes,bytes,uint256,uint256,uint256,uint256,uint256,bytes,bytes) userOp, bytes32 , uint256 missingAccountFunds) returns(uint256 validationData)
func (_ETHSwap *ETHSwapTransactorSession) ValidateUserOp(userOp UserOperation, arg1 [32]byte, missingAccountFunds *big.Int) (*types.Transaction, error) {
	return _ETHSwap.Contract.ValidateUserOp(&_ETHSwap.TransactOpts, userOp, arg1, missingAccountFunds)
}

// ETHSwapLogShizIterator is returned from FilterLogShiz and is used to iterate over the raw logs and unpacked data for LogShiz events raised by the ETHSwap contract.
type ETHSwapLogShizIterator struct {
	Event *ETHSwapLogShiz // Event containing the contract specifics and raw log

	contract *bind.BoundContract // Generic contract to use for unpacking event data
	event    string              // Event name to use for unpacking event data

	logs chan types.Log        // Log channel receiving the found contract events
	sub  ethereum.Subscription // Subscription for errors, completion and termination
	done bool                  // Whether the subscription completed delivering logs
	fail error                 // Occurred error to stop iteration
}

// Next advances the iterator to the subsequent event, returning whether there
// are any more events found. In case of a retrieval or parsing error, false is
// returned and Error() can be queried for the exact failure.
func (it *ETHSwapLogShizIterator) Next() bool {
	// If the iterator failed, stop iterating
	if it.fail != nil {
		return false
	}
	// If the iterator completed, deliver directly whatever's available
	if it.done {
		select {
		case log := <-it.logs:
			it.Event = new(ETHSwapLogShiz)
			if err := it.contract.UnpackLog(it.Event, it.event, log); err != nil {
				it.fail = err
				return false
			}
			it.Event.Raw = log
			return true

		default:
			return false
		}
	}
	// Iterator still in progress, wait for either a data or an error event
	select {
	case log := <-it.logs:
		it.Event = new(ETHSwapLogShiz)
		if err := it.contract.UnpackLog(it.Event, it.event, log); err != nil {
			it.fail = err
			return false
		}
		it.Event.Raw = log
		return true

	case err := <-it.sub.Err():
		it.done = true
		it.fail = err
		return it.Next()
	}
}

// Error returns any retrieval or parsing error occurred during filtering.
func (it *ETHSwapLogShizIterator) Error() error {
	return it.fail
}

// Close terminates the iteration process, releasing any pending underlying
// resources.
func (it *ETHSwapLogShizIterator) Close() error {
	it.sub.Unsubscribe()
	return nil
}

// ETHSwapLogShiz represents a LogShiz event raised by the ETHSwap contract.
type ETHSwapLogShiz struct {
	Arg0 *big.Int
	Raw  types.Log // Blockchain specific contextual infos
}

// FilterLogShiz is a free log retrieval operation binding the contract event 0xc31d59c3affd669cc3ae199e4875ad4b814521f3b5b55395d8b9d880bc65d5ee.
//
// Solidity: event LogShiz(uint256 arg0)
func (_ETHSwap *ETHSwapFilterer) FilterLogShiz(opts *bind.FilterOpts) (*ETHSwapLogShizIterator, error) {

	logs, sub, err := _ETHSwap.contract.FilterLogs(opts, "LogShiz")
	if err != nil {
		return nil, err
	}
	return &ETHSwapLogShizIterator{contract: _ETHSwap.contract, event: "LogShiz", logs: logs, sub: sub}, nil
}

// WatchLogShiz is a free log subscription operation binding the contract event 0xc31d59c3affd669cc3ae199e4875ad4b814521f3b5b55395d8b9d880bc65d5ee.
//
// Solidity: event LogShiz(uint256 arg0)
func (_ETHSwap *ETHSwapFilterer) WatchLogShiz(opts *bind.WatchOpts, sink chan<- *ETHSwapLogShiz) (event.Subscription, error) {

	logs, sub, err := _ETHSwap.contract.WatchLogs(opts, "LogShiz")
	if err != nil {
		return nil, err
	}
	return event.NewSubscription(func(quit <-chan struct{}) error {
		defer sub.Unsubscribe()
		for {
			select {
			case log := <-logs:
				// New log arrived, parse the event and forward to the user
				event := new(ETHSwapLogShiz)
				if err := _ETHSwap.contract.UnpackLog(event, "LogShiz", log); err != nil {
					return err
				}
				event.Raw = log

				select {
				case sink <- event:
				case err := <-sub.Err():
					return err
				case <-quit:
					return nil
				}
			case err := <-sub.Err():
				return err
			case <-quit:
				return nil
			}
		}
	}), nil
}

// ParseLogShiz is a log parse operation binding the contract event 0xc31d59c3affd669cc3ae199e4875ad4b814521f3b5b55395d8b9d880bc65d5ee.
//
// Solidity: event LogShiz(uint256 arg0)
func (_ETHSwap *ETHSwapFilterer) ParseLogShiz(log types.Log) (*ETHSwapLogShiz, error) {
	event := new(ETHSwapLogShiz)
	if err := _ETHSwap.contract.UnpackLog(event, "LogShiz", log); err != nil {
		return nil, err
	}
	event.Raw = log
	return event, nil
}
