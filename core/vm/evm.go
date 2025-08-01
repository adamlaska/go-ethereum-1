// Copyright 2014 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package vm

import (
	"errors"
	"math/big"
	"sync/atomic"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/params"
	"github.com/holiman/uint256"
)

type (
	// CanTransferFunc is the signature of a transfer guard function
	CanTransferFunc func(StateDB, common.Address, *uint256.Int) bool
	// TransferFunc is the signature of a transfer function
	TransferFunc func(StateDB, common.Address, common.Address, *uint256.Int)
	// GetHashFunc returns the n'th block hash in the blockchain
	// and is used by the BLOCKHASH EVM op code.
	GetHashFunc func(uint64) common.Hash
)

func (evm *EVM) precompile(addr common.Address) (PrecompiledContract, bool) {
	p, ok := evm.precompiles[addr]
	return p, ok
}

// BlockContext provides the EVM with auxiliary information. Once provided
// it shouldn't be modified.
type BlockContext struct {
	// CanTransfer returns whether the account contains
	// sufficient ether to transfer the value
	CanTransfer CanTransferFunc
	// Transfer transfers ether from one account to the other
	Transfer TransferFunc
	// GetHash returns the hash corresponding to n
	GetHash GetHashFunc

	// Block information
	Coinbase    common.Address // Provides information for COINBASE
	GasLimit    uint64         // Provides information for GASLIMIT
	BlockNumber *big.Int       // Provides information for NUMBER
	Time        uint64         // Provides information for TIME
	Difficulty  *big.Int       // Provides information for DIFFICULTY
	BaseFee     *big.Int       // Provides information for BASEFEE (0 if vm runs with NoBaseFee flag and 0 gas price)
	BlobBaseFee *big.Int       // Provides information for BLOBBASEFEE (0 if vm runs with NoBaseFee flag and 0 blob gas price)
	Random      *common.Hash   // Provides information for PREVRANDAO
}

// TxContext provides the EVM with information about a transaction.
// All fields can change between transactions.
type TxContext struct {
	// Message information
	Origin       common.Address      // Provides information for ORIGIN
	GasPrice     *big.Int            // Provides information for GASPRICE (and is used to zero the basefee if NoBaseFee is set)
	BlobHashes   []common.Hash       // Provides information for BLOBHASH
	BlobFeeCap   *big.Int            // Is used to zero the blobbasefee if NoBaseFee is set
	AccessEvents *state.AccessEvents // Capture all state accesses for this tx
}

// EVM is the Ethereum Virtual Machine base object and provides
// the necessary tools to run a contract on the given state with
// the provided context. It should be noted that any error
// generated through any of the calls should be considered a
// revert-state-and-consume-all-gas operation, no checks on
// specific errors should ever be performed. The interpreter makes
// sure that any errors generated are to be considered faulty code.
//
// The EVM should never be reused and is not thread safe.
type EVM struct {
	// Context provides auxiliary blockchain related information
	Context BlockContext
	TxContext

	// StateDB gives access to the underlying state
	StateDB StateDB

	// depth is the current call stack
	depth int

	// chainConfig contains information about the current chain
	chainConfig *params.ChainConfig

	// chain rules contains the chain rules for the current epoch
	chainRules params.Rules

	// virtual machine configuration options used to initialise the evm
	Config Config

	// global (to this context) ethereum virtual machine used throughout
	// the execution of the tx
	interpreter *EVMInterpreter

	// abort is used to abort the EVM calling operations
	abort atomic.Bool

	// callGasTemp holds the gas available for the current call. This is needed because the
	// available gas is calculated in gasCall* according to the 63/64 rule and later
	// applied in opCall*.
	callGasTemp uint64

	// precompiles holds the precompiled contracts for the current epoch
	precompiles map[common.Address]PrecompiledContract

	// jumpDests stores results of JUMPDEST analysis.
	jumpDests JumpDestCache
}

// NewEVM constructs an EVM instance with the supplied block context, state
// database and several configs. It meant to be used throughout the entire
// state transition of a block, with the transaction context switched as
// needed by calling evm.SetTxContext.
func NewEVM(blockCtx BlockContext, statedb StateDB, chainConfig *params.ChainConfig, config Config) *EVM {
	evm := &EVM{
		Context:     blockCtx,
		StateDB:     statedb,
		Config:      config,
		chainConfig: chainConfig,
		chainRules:  chainConfig.Rules(blockCtx.BlockNumber, blockCtx.Random != nil, blockCtx.Time),
		jumpDests:   newMapJumpDests(),
	}
	evm.precompiles = activePrecompiledContracts(evm.chainRules)
	evm.interpreter = NewEVMInterpreter(evm)
	return evm
}

// SetPrecompiles sets the precompiled contracts for the EVM.
// This method is only used through RPC calls.
// It is not thread-safe.
func (evm *EVM) SetPrecompiles(precompiles PrecompiledContracts) {
	evm.precompiles = precompiles
}

// SetJumpDestCache configures the analysis cache.
func (evm *EVM) SetJumpDestCache(jumpDests JumpDestCache) {
	evm.jumpDests = jumpDests
}

// SetTxContext resets the EVM with a new transaction context.
// This is not threadsafe and should only be done very cautiously.
func (evm *EVM) SetTxContext(txCtx TxContext) {
	if evm.chainRules.IsEIP4762 {
		txCtx.AccessEvents = state.NewAccessEvents(evm.StateDB.PointCache())
	}
	evm.TxContext = txCtx
}

// Cancel cancels any running EVM operation. This may be called concurrently and
// it's safe to be called multiple times.
func (evm *EVM) Cancel() {
	evm.abort.Store(true)
}

// Cancelled returns true if Cancel has been called
func (evm *EVM) Cancelled() bool {
	return evm.abort.Load()
}

// Interpreter returns the current interpreter
func (evm *EVM) Interpreter() *EVMInterpreter {
	return evm.interpreter
}

func isSystemCall(caller common.Address) bool {
	return caller == params.SystemAddress
}

// Call executes the contract associated with the addr with the given input as
// parameters. It also handles any necessary value transfer required and takse
// the necessary steps to create accounts and reverses the state in case of an
// execution error or failed value transfer.
func (evm *EVM) Call(caller common.Address, addr common.Address, input []byte, gas uint64, value *uint256.Int) (ret []byte, leftOverGas uint64, err error) {
	// Capture the tracer start/end events in debug mode
	if evm.Config.Tracer != nil {
		evm.captureBegin(evm.depth, CALL, caller, addr, input, gas, value.ToBig())
		defer func(startGas uint64) {
			evm.captureEnd(evm.depth, startGas, leftOverGas, ret, err)
		}(gas)
	}
	// Fail if we're trying to execute above the call depth limit
	if evm.depth > int(params.CallCreateDepth) {
		return nil, gas, ErrDepth
	}
	// Fail if we're trying to transfer more than the available balance
	if !value.IsZero() && !evm.Context.CanTransfer(evm.StateDB, caller, value) {
		return nil, gas, ErrInsufficientBalance
	}
	snapshot := evm.StateDB.Snapshot()
	p, isPrecompile := evm.precompile(addr)

	if !evm.StateDB.Exist(addr) {
		if !isPrecompile && evm.chainRules.IsEIP4762 && !isSystemCall(caller) {
			// Add proof of absence to witness
			// At this point, the read costs have already been charged, either because this
			// is a direct tx call, in which case it's covered by the intrinsic gas, or because
			// of a CALL instruction, in which case BASIC_DATA has been added to the access
			// list in write mode. If there is enough gas paying for the addition of the code
			// hash leaf to the access list, then account creation will proceed unimpaired.
			// Thus, only pay for the creation of the code hash leaf here.
			wgas := evm.AccessEvents.CodeHashGas(addr, true, gas, false)
			if gas < wgas {
				evm.StateDB.RevertToSnapshot(snapshot)
				return nil, 0, ErrOutOfGas
			}
			gas -= wgas
		}

		if !isPrecompile && evm.chainRules.IsEIP158 && value.IsZero() {
			// Calling a non-existing account, don't do anything.
			return nil, gas, nil
		}
		evm.StateDB.CreateAccount(addr)
	}
	evm.Context.Transfer(evm.StateDB, caller, addr, value)

	if isPrecompile {
		ret, gas, err = RunPrecompiledContract(p, input, gas, evm.Config.Tracer)
	} else {
		// Initialise a new contract and set the code that is to be used by the EVM.
		code := evm.resolveCode(addr)
		if len(code) == 0 {
			ret, err = nil, nil // gas is unchanged
		} else {
			// The contract is a scoped environment for this execution context only.
			contract := NewContract(caller, addr, value, gas, evm.jumpDests)
			contract.IsSystemCall = isSystemCall(caller)
			contract.SetCallCode(evm.resolveCodeHash(addr), code)
			ret, err = evm.interpreter.Run(contract, input, false)
			gas = contract.Gas
		}
	}
	// When an error was returned by the EVM or when setting the creation code
	// above we revert to the snapshot and consume any gas remaining. Additionally,
	// when we're in homestead this also counts for code storage gas errors.
	if err != nil {
		evm.StateDB.RevertToSnapshot(snapshot)
		if err != ErrExecutionReverted {
			if evm.Config.Tracer != nil && evm.Config.Tracer.OnGasChange != nil {
				evm.Config.Tracer.OnGasChange(gas, 0, tracing.GasChangeCallFailedExecution)
			}

			gas = 0
		}
		// TODO: consider clearing up unused snapshots:
		//} else {
		//	evm.StateDB.DiscardSnapshot(snapshot)
	}
	return ret, gas, err
}

// CallCode executes the contract associated with the addr with the given input
// as parameters. It also handles any necessary value transfer required and takes
// the necessary steps to create accounts and reverses the state in case of an
// execution error or failed value transfer.
//
// CallCode differs from Call in the sense that it executes the given address'
// code with the caller as context.
func (evm *EVM) CallCode(caller common.Address, addr common.Address, input []byte, gas uint64, value *uint256.Int) (ret []byte, leftOverGas uint64, err error) {
	// Invoke tracer hooks that signal entering/exiting a call frame
	if evm.Config.Tracer != nil {
		evm.captureBegin(evm.depth, CALLCODE, caller, addr, input, gas, value.ToBig())
		defer func(startGas uint64) {
			evm.captureEnd(evm.depth, startGas, leftOverGas, ret, err)
		}(gas)
	}
	// Fail if we're trying to execute above the call depth limit
	if evm.depth > int(params.CallCreateDepth) {
		return nil, gas, ErrDepth
	}
	// Fail if we're trying to transfer more than the available balance
	// Note although it's noop to transfer X ether to caller itself. But
	// if caller doesn't have enough balance, it would be an error to allow
	// over-charging itself. So the check here is necessary.
	if !evm.Context.CanTransfer(evm.StateDB, caller, value) {
		return nil, gas, ErrInsufficientBalance
	}
	var snapshot = evm.StateDB.Snapshot()

	// It is allowed to call precompiles, even via delegatecall
	if p, isPrecompile := evm.precompile(addr); isPrecompile {
		ret, gas, err = RunPrecompiledContract(p, input, gas, evm.Config.Tracer)
	} else {
		// Initialise a new contract and set the code that is to be used by the EVM.
		// The contract is a scoped environment for this execution context only.
		contract := NewContract(caller, caller, value, gas, evm.jumpDests)
		contract.SetCallCode(evm.resolveCodeHash(addr), evm.resolveCode(addr))
		ret, err = evm.interpreter.Run(contract, input, false)
		gas = contract.Gas
	}
	if err != nil {
		evm.StateDB.RevertToSnapshot(snapshot)
		if err != ErrExecutionReverted {
			if evm.Config.Tracer != nil && evm.Config.Tracer.OnGasChange != nil {
				evm.Config.Tracer.OnGasChange(gas, 0, tracing.GasChangeCallFailedExecution)
			}
			gas = 0
		}
	}
	return ret, gas, err
}

// DelegateCall executes the contract associated with the addr with the given input
// as parameters. It reverses the state in case of an execution error.
//
// DelegateCall differs from CallCode in the sense that it executes the given address'
// code with the caller as context and the caller is set to the caller of the caller.
func (evm *EVM) DelegateCall(originCaller common.Address, caller common.Address, addr common.Address, input []byte, gas uint64, value *uint256.Int) (ret []byte, leftOverGas uint64, err error) {
	// Invoke tracer hooks that signal entering/exiting a call frame
	if evm.Config.Tracer != nil {
		// DELEGATECALL inherits value from parent call
		evm.captureBegin(evm.depth, DELEGATECALL, caller, addr, input, gas, value.ToBig())
		defer func(startGas uint64) {
			evm.captureEnd(evm.depth, startGas, leftOverGas, ret, err)
		}(gas)
	}
	// Fail if we're trying to execute above the call depth limit
	if evm.depth > int(params.CallCreateDepth) {
		return nil, gas, ErrDepth
	}
	var snapshot = evm.StateDB.Snapshot()

	// It is allowed to call precompiles, even via delegatecall
	if p, isPrecompile := evm.precompile(addr); isPrecompile {
		ret, gas, err = RunPrecompiledContract(p, input, gas, evm.Config.Tracer)
	} else {
		// Initialise a new contract and make initialise the delegate values
		//
		// Note: The value refers to the original value from the parent call.
		contract := NewContract(originCaller, caller, value, gas, evm.jumpDests)
		contract.SetCallCode(evm.resolveCodeHash(addr), evm.resolveCode(addr))
		ret, err = evm.interpreter.Run(contract, input, false)
		gas = contract.Gas
	}
	if err != nil {
		evm.StateDB.RevertToSnapshot(snapshot)
		if err != ErrExecutionReverted {
			if evm.Config.Tracer != nil && evm.Config.Tracer.OnGasChange != nil {
				evm.Config.Tracer.OnGasChange(gas, 0, tracing.GasChangeCallFailedExecution)
			}
			gas = 0
		}
	}
	return ret, gas, err
}

// StaticCall executes the contract associated with the addr with the given input
// as parameters while disallowing any modifications to the state during the call.
// Opcodes that attempt to perform such modifications will result in exceptions
// instead of performing the modifications.
func (evm *EVM) StaticCall(caller common.Address, addr common.Address, input []byte, gas uint64) (ret []byte, leftOverGas uint64, err error) {
	// Invoke tracer hooks that signal entering/exiting a call frame
	if evm.Config.Tracer != nil {
		evm.captureBegin(evm.depth, STATICCALL, caller, addr, input, gas, nil)
		defer func(startGas uint64) {
			evm.captureEnd(evm.depth, startGas, leftOverGas, ret, err)
		}(gas)
	}
	// Fail if we're trying to execute above the call depth limit
	if evm.depth > int(params.CallCreateDepth) {
		return nil, gas, ErrDepth
	}
	// We take a snapshot here. This is a bit counter-intuitive, and could probably be skipped.
	// However, even a staticcall is considered a 'touch'. On mainnet, static calls were introduced
	// after all empty accounts were deleted, so this is not required. However, if we omit this,
	// then certain tests start failing; stRevertTest/RevertPrecompiledTouchExactOOG.json.
	// We could change this, but for now it's left for legacy reasons
	var snapshot = evm.StateDB.Snapshot()

	// We do an AddBalance of zero here, just in order to trigger a touch.
	// This doesn't matter on Mainnet, where all empties are gone at the time of Byzantium,
	// but is the correct thing to do and matters on other networks, in tests, and potential
	// future scenarios
	evm.StateDB.AddBalance(addr, new(uint256.Int), tracing.BalanceChangeTouchAccount)

	if p, isPrecompile := evm.precompile(addr); isPrecompile {
		ret, gas, err = RunPrecompiledContract(p, input, gas, evm.Config.Tracer)
	} else {
		// Initialise a new contract and set the code that is to be used by the EVM.
		// The contract is a scoped environment for this execution context only.
		contract := NewContract(caller, addr, new(uint256.Int), gas, evm.jumpDests)
		contract.SetCallCode(evm.resolveCodeHash(addr), evm.resolveCode(addr))

		// When an error was returned by the EVM or when setting the creation code
		// above we revert to the snapshot and consume any gas remaining. Additionally
		// when we're in Homestead this also counts for code storage gas errors.
		ret, err = evm.interpreter.Run(contract, input, true)
		gas = contract.Gas
	}
	if err != nil {
		evm.StateDB.RevertToSnapshot(snapshot)
		if err != ErrExecutionReverted {
			if evm.Config.Tracer != nil && evm.Config.Tracer.OnGasChange != nil {
				evm.Config.Tracer.OnGasChange(gas, 0, tracing.GasChangeCallFailedExecution)
			}

			gas = 0
		}
	}
	return ret, gas, err
}

// create creates a new contract using code as deployment code.
func (evm *EVM) create(caller common.Address, code []byte, gas uint64, value *uint256.Int, address common.Address, typ OpCode) (ret []byte, createAddress common.Address, leftOverGas uint64, err error) {
	if evm.Config.Tracer != nil {
		evm.captureBegin(evm.depth, typ, caller, address, code, gas, value.ToBig())
		defer func(startGas uint64) {
			evm.captureEnd(evm.depth, startGas, leftOverGas, ret, err)
		}(gas)
	}
	// Depth check execution. Fail if we're trying to execute above the
	// limit.
	if evm.depth > int(params.CallCreateDepth) {
		return nil, common.Address{}, gas, ErrDepth
	}
	if !evm.Context.CanTransfer(evm.StateDB, caller, value) {
		return nil, common.Address{}, gas, ErrInsufficientBalance
	}
	nonce := evm.StateDB.GetNonce(caller)
	if nonce+1 < nonce {
		return nil, common.Address{}, gas, ErrNonceUintOverflow
	}
	evm.StateDB.SetNonce(caller, nonce+1, tracing.NonceChangeContractCreator)

	// Charge the contract creation init gas in verkle mode
	if evm.chainRules.IsEIP4762 {
		statelessGas := evm.AccessEvents.ContractCreatePreCheckGas(address, gas)
		if statelessGas > gas {
			return nil, common.Address{}, 0, ErrOutOfGas
		}
		if evm.Config.Tracer != nil && evm.Config.Tracer.OnGasChange != nil {
			evm.Config.Tracer.OnGasChange(gas, gas-statelessGas, tracing.GasChangeWitnessContractCollisionCheck)
		}
		gas = gas - statelessGas
	}

	// We add this to the access list _before_ taking a snapshot. Even if the
	// creation fails, the access-list change should not be rolled back.
	if evm.chainRules.IsEIP2929 {
		evm.StateDB.AddAddressToAccessList(address)
	}
	// Ensure there's no existing contract already at the designated address.
	// Account is regarded as existent if any of these three conditions is met:
	// - the nonce is non-zero
	// - the code is non-empty
	// - the storage is non-empty
	contractHash := evm.StateDB.GetCodeHash(address)
	storageRoot := evm.StateDB.GetStorageRoot(address)
	if evm.StateDB.GetNonce(address) != 0 ||
		(contractHash != (common.Hash{}) && contractHash != types.EmptyCodeHash) || // non-empty code
		(storageRoot != (common.Hash{}) && storageRoot != types.EmptyRootHash) { // non-empty storage
		if evm.Config.Tracer != nil && evm.Config.Tracer.OnGasChange != nil {
			evm.Config.Tracer.OnGasChange(gas, 0, tracing.GasChangeCallFailedExecution)
		}
		return nil, common.Address{}, 0, ErrContractAddressCollision
	}
	// Create a new account on the state only if the object was not present.
	// It might be possible the contract code is deployed to a pre-existent
	// account with non-zero balance.
	snapshot := evm.StateDB.Snapshot()
	if !evm.StateDB.Exist(address) {
		evm.StateDB.CreateAccount(address)
	}
	// CreateContract means that regardless of whether the account previously existed
	// in the state trie or not, it _now_ becomes created as a _contract_ account.
	// This is performed _prior_ to executing the initcode,  since the initcode
	// acts inside that account.
	evm.StateDB.CreateContract(address)

	if evm.chainRules.IsEIP158 {
		evm.StateDB.SetNonce(address, 1, tracing.NonceChangeNewContract)
	}
	// Charge the contract creation init gas in verkle mode
	if evm.chainRules.IsEIP4762 {
		consumed, wanted := evm.AccessEvents.ContractCreateInitGas(address, gas)
		if consumed < wanted {
			return nil, common.Address{}, 0, ErrOutOfGas
		}
		if evm.Config.Tracer != nil && evm.Config.Tracer.OnGasChange != nil {
			evm.Config.Tracer.OnGasChange(gas, gas-consumed, tracing.GasChangeWitnessContractInit)
		}
		gas = gas - consumed
	}
	evm.Context.Transfer(evm.StateDB, caller, address, value)

	// Initialise a new contract and set the code that is to be used by the EVM.
	// The contract is a scoped environment for this execution context only.
	contract := NewContract(caller, address, value, gas, evm.jumpDests)

	// Explicitly set the code to a null hash to prevent caching of jump analysis
	// for the initialization code.
	contract.SetCallCode(common.Hash{}, code)
	contract.IsDeployment = true

	ret, err = evm.initNewContract(contract, address)
	if err != nil && (evm.chainRules.IsHomestead || err != ErrCodeStoreOutOfGas) {
		evm.StateDB.RevertToSnapshot(snapshot)
		if err != ErrExecutionReverted {
			contract.UseGas(contract.Gas, evm.Config.Tracer, tracing.GasChangeCallFailedExecution)
		}
	}
	return ret, address, contract.Gas, err
}

// initNewContract runs a new contract's creation code, performs checks on the
// resulting code that is to be deployed, and consumes necessary gas.
func (evm *EVM) initNewContract(contract *Contract, address common.Address) ([]byte, error) {
	ret, err := evm.interpreter.Run(contract, nil, false)
	if err != nil {
		return ret, err
	}

	// Check whether the max code size has been exceeded, assign err if the case.
	if evm.chainRules.IsEIP158 && len(ret) > params.MaxCodeSize {
		return ret, ErrMaxCodeSizeExceeded
	}

	// Reject code starting with 0xEF if EIP-3541 is enabled.
	if len(ret) >= 1 && ret[0] == 0xEF && evm.chainRules.IsLondon {
		return ret, ErrInvalidCode
	}

	if !evm.chainRules.IsEIP4762 {
		createDataGas := uint64(len(ret)) * params.CreateDataGas
		if !contract.UseGas(createDataGas, evm.Config.Tracer, tracing.GasChangeCallCodeStorage) {
			return ret, ErrCodeStoreOutOfGas
		}
	} else {
		consumed, wanted := evm.AccessEvents.CodeChunksRangeGas(address, 0, uint64(len(ret)), uint64(len(ret)), true, contract.Gas)
		contract.UseGas(consumed, evm.Config.Tracer, tracing.GasChangeWitnessCodeChunk)
		if len(ret) > 0 && (consumed < wanted) {
			return ret, ErrCodeStoreOutOfGas
		}
	}

	evm.StateDB.SetCode(address, ret)
	return ret, nil
}

// Create creates a new contract using code as deployment code.
func (evm *EVM) Create(caller common.Address, code []byte, gas uint64, value *uint256.Int) (ret []byte, contractAddr common.Address, leftOverGas uint64, err error) {
	contractAddr = crypto.CreateAddress(caller, evm.StateDB.GetNonce(caller))
	return evm.create(caller, code, gas, value, contractAddr, CREATE)
}

// Create2 creates a new contract using code as deployment code.
//
// The different between Create2 with Create is Create2 uses keccak256(0xff ++ msg.sender ++ salt ++ keccak256(init_code))[12:]
// instead of the usual sender-and-nonce-hash as the address where the contract is initialized at.
func (evm *EVM) Create2(caller common.Address, code []byte, gas uint64, endowment *uint256.Int, salt *uint256.Int) (ret []byte, contractAddr common.Address, leftOverGas uint64, err error) {
	inithash := crypto.HashData(evm.interpreter.hasher, code)
	contractAddr = crypto.CreateAddress2(caller, salt.Bytes32(), inithash[:])
	return evm.create(caller, code, gas, endowment, contractAddr, CREATE2)
}

// resolveCode returns the code associated with the provided account. After
// Prague, it can also resolve code pointed to by a delegation designator.
func (evm *EVM) resolveCode(addr common.Address) []byte {
	code := evm.StateDB.GetCode(addr)
	if !evm.chainRules.IsPrague {
		return code
	}
	if target, ok := types.ParseDelegation(code); ok {
		// Note we only follow one level of delegation.
		return evm.StateDB.GetCode(target)
	}
	return code
}

// resolveCodeHash returns the code hash associated with the provided address.
// After Prague, it can also resolve code hash of the account pointed to by a
// delegation designator. Although this is not accessible in the EVM it is used
// internally to associate jumpdest analysis to code.
func (evm *EVM) resolveCodeHash(addr common.Address) common.Hash {
	if evm.chainRules.IsPrague {
		code := evm.StateDB.GetCode(addr)
		if target, ok := types.ParseDelegation(code); ok {
			// Note we only follow one level of delegation.
			return evm.StateDB.GetCodeHash(target)
		}
	}
	return evm.StateDB.GetCodeHash(addr)
}

// ChainConfig returns the environment's chain configuration
func (evm *EVM) ChainConfig() *params.ChainConfig { return evm.chainConfig }

func (evm *EVM) captureBegin(depth int, typ OpCode, from common.Address, to common.Address, input []byte, startGas uint64, value *big.Int) {
	tracer := evm.Config.Tracer
	if tracer.OnEnter != nil {
		tracer.OnEnter(depth, byte(typ), from, to, input, startGas, value)
	}
	if tracer.OnGasChange != nil {
		tracer.OnGasChange(0, startGas, tracing.GasChangeCallInitialBalance)
	}
}

func (evm *EVM) captureEnd(depth int, startGas uint64, leftOverGas uint64, ret []byte, err error) {
	tracer := evm.Config.Tracer
	if leftOverGas != 0 && tracer.OnGasChange != nil {
		tracer.OnGasChange(leftOverGas, 0, tracing.GasChangeCallLeftOverReturned)
	}
	var reverted bool
	if err != nil {
		reverted = true
	}
	if !evm.chainRules.IsHomestead && errors.Is(err, ErrCodeStoreOutOfGas) {
		reverted = false
	}
	if tracer.OnExit != nil {
		tracer.OnExit(depth, ret, startGas-leftOverGas, VMErrorFromErr(err), reverted)
	}
}

// GetVMContext provides context about the block being executed as well as state
// to the tracers.
func (evm *EVM) GetVMContext() *tracing.VMContext {
	return &tracing.VMContext{
		Coinbase:    evm.Context.Coinbase,
		BlockNumber: evm.Context.BlockNumber,
		Time:        evm.Context.Time,
		Random:      evm.Context.Random,
		BaseFee:     evm.Context.BaseFee,
		StateDB:     evm.StateDB,
	}
}
