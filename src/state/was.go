package state

import (
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	ethState "github.com/ethereum/go-ethereum/core/state"
	ethTypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/sirupsen/logrus"
)

// write ahead state, updated with each AppendTx
// and reset on Commit
type WriteAheadState struct {
	db       ethdb.Database
	ethState *ethState.StateDB

	signer      ethTypes.Signer
	chainConfig params.ChainConfig // vm.env is still tightly coupled with chainConfig
	vmConfig    vm.Config
	gasLimit    uint64

	txIndex      int
	transactions []*ethTypes.Transaction
	receipts     []*ethTypes.Receipt
	allLogs      []*ethTypes.Log

	totalUsedGas *big.Int
	gp           *core.GasPool

	logger *logrus.Logger
}

func NewWriteAheadState(db ethdb.Database,
	root common.Hash,
	signer ethTypes.Signer,
	chainConfig params.ChainConfig,
	vmConfig vm.Config,
	gasLimit uint64,
	logger *logrus.Logger) (*WriteAheadState, error) {

	ethState, err := ethState.New(root, ethState.NewDatabase(db))
	if err != nil {
		return nil, err
	}

	return &WriteAheadState{
		db:          db,
		ethState:    ethState,
		signer:      signer,
		chainConfig: chainConfig,
		vmConfig:    vmConfig,
		gasLimit:    gasLimit,
		logger:      logger,
	}, nil
}

func (was *WriteAheadState) Reset(root common.Hash) error {

	err := was.ethState.Reset(root)
	if err != nil {
		return err
	}

	was.txIndex = 0
	was.transactions = []*ethTypes.Transaction{}
	was.receipts = []*ethTypes.Receipt{}
	was.allLogs = []*ethTypes.Log{}

	was.totalUsedGas = new(big.Int).SetUint64(0)
	was.gp = new(core.GasPool).AddGas(was.gasLimit)

	was.logger.WithFields(logrus.Fields{
		"gasLimit":   gasLimit,
		"was.gp":   was.gp,
	}).Debug("(was *WriteAheadState) Reset(root common.Hash)")

	return nil
}

func (was *WriteAheadState) ApplyTransaction(tx ethTypes.Transaction, txIndex int, blockHash common.Hash) error {

	msg, err := tx.AsMessage(was.signer)
	if err != nil {
		was.logger.WithError(err).Error("Converting Transaction to Message")
		return err
	}

	context := vm.Context{
		CanTransfer: core.CanTransfer,
		Transfer:    core.Transfer,
		GetHash:     func(uint64) common.Hash { return blockHash },
		Origin:      msg.From(),
		GasLimit:    msg.Gas(),
		GasPrice:    msg.GasPrice(),
		BlockNumber: big.NewInt(0), // The vm has a dependency on this..
	}
	was.logger.WithFields(logrus.Fields{
		"GasLimit":   msg.Gas()}).Debug("was.ApplyTransaction")

	//Prepare the ethState with transaction Hash so that it can be used in emitted
	//logs
	was.ethState.Prepare(tx.Hash(), blockHash, txIndex)

	vmenv := vm.NewEVM(context, was.ethState, &was.chainConfig, was.vmConfig)

	// Apply the transaction to the current state (included in the env)
	_, gas, failed, err := core.ApplyMessage(vmenv, msg, was.gp)
	if err != nil {
		was.logger.WithError(err).Error("Applying transaction to WriteAheadState")
		return err
	}

	was.totalUsedGas.Add(was.totalUsedGas, new(big.Int).SetUint64(gas))

	// Create a new receipt for the transaction, storing the intermediate root and gas used by the tx
	// based on the eip phase, we're passing whether the root touch-delete accounts.
	root := was.ethState.IntermediateRoot(true) //this has side effects. It updates StateObjects (SmartContract memory)
	receipt := ethTypes.NewReceipt(root.Bytes(), failed, was.totalUsedGas.Uint64())
	receipt.TxHash = tx.Hash()
	receipt.GasUsed = gas
	// if the transaction created a contract, store the creation address in the receipt.
	if msg.To() == nil {
		receipt.ContractAddress = crypto.CreateAddress(vmenv.Context.Origin, tx.Nonce())
	}
	// Set the receipt logs and create a bloom for filtering
	receipt.Logs = was.ethState.GetLogs(tx.Hash())
	//receipt.Logs = s.was.state.Logs()
	receipt.Bloom = ethTypes.CreateBloom(ethTypes.Receipts{receipt})

	was.txIndex++
	was.transactions = append(was.transactions, &tx)
	was.receipts = append(was.receipts, receipt)
	was.allLogs = append(was.allLogs, receipt.Logs...)

	was.logger.WithField("hash", tx.Hash().Hex()).Debug("Applied tx to WAS")

	return nil
}

func (was *WriteAheadState) Commit() (common.Hash, error) {
	//commit all state changes to the database
	root, err := was.ethState.Commit(true)
	if err != nil {
		was.logger.WithError(err).Error("Committing state")
		return common.Hash{}, err
	}

	//XXX FORCE DISK WRITE
	// Apparently Geth does something smarter here... but can't figure it out
	if err := was.ethState.Database().TrieDB().Commit(root, true); err != nil {
		was.logger.WithError(err).Error("Writing root")
		return common.Hash{}, err
	}
	if err := was.writeRoot(root); err != nil {
		was.logger.WithError(err).Error("Writing root")
		return common.Hash{}, err
	}
	if err := was.writeHead(); err != nil {
		was.logger.WithError(err).Error("Writing head")
		return common.Hash{}, err
	}
	if err := was.writeTransactions(); err != nil {
		was.logger.WithError(err).Error("Writing txs")
		return common.Hash{}, err
	}
	if err := was.writeReceipts(); err != nil {
		was.logger.WithError(err).Error("Writing receipts")
		return common.Hash{}, err
	}
	return root, nil
}

func (was *WriteAheadState) writeRoot(root common.Hash) error {
	return was.db.Put(rootKey, root.Bytes())
}

func (was *WriteAheadState) writeHead() error {
	head := &ethTypes.Transaction{}
	if len(was.transactions) > 0 {
		head = was.transactions[len(was.transactions)-1]
	}
	return was.db.Put(headTxKey, head.Hash().Bytes())
}

func (was *WriteAheadState) writeTransactions() error {
	batch := was.db.NewBatch()

	for _, tx := range was.transactions {
		data, err := rlp.EncodeToBytes(tx)
		if err != nil {
			return err
		}
		if err := batch.Put(tx.Hash().Bytes(), data); err != nil {
			return err
		}
	}

	// Write the scheduled data into the database
	return batch.Write()
}

func (was *WriteAheadState) writeReceipts() error {
	batch := was.db.NewBatch()

	for _, receipt := range was.receipts {
		storageReceipt := (*ethTypes.ReceiptForStorage)(receipt)
		data, err := rlp.EncodeToBytes(storageReceipt)
		if err != nil {
			return err
		}
		if err := batch.Put(append(receiptsPrefix, receipt.TxHash.Bytes()...), data); err != nil {
			return err
		}
	}

	return batch.Write()
}
