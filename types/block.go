// Copyright (c) 2020 The Decred developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package types

import (
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"

	rtypes "github.com/coinbase/rosetta-sdk-go/types"
	"github.com/decred/dcrd/blockchain/stake/v3"
	"github.com/decred/dcrd/chaincfg/v3"
	"github.com/decred/dcrd/dcrutil/v3"
	"github.com/decred/dcrd/txscript/v3"
	"github.com/decred/dcrd/wire"
)

var (
	ErrNeedsPreviousBlock = errors.New("previous block required")

	CurrencySymbol = &rtypes.Currency{
		Symbol:   "DCR",
		Decimals: 8,
	}
)

func DcrAmountToRosetta(amt dcrutil.Amount) *rtypes.Amount {
	return &rtypes.Amount{
		Value:    strconv.FormatInt(int64(amt), 10),
		Currency: CurrencySymbol,
	}
}

// VoteBitsApprovesParent returns true if the provided voteBits as included in
// some block header flags the parent block as approved according to current
// consensus rules.
func VoteBitsApprovesParent(voteBits uint16) bool {
	return voteBits&0x01 == 0x01
}

func rawPkScriptToAccountAddr(version uint16, pkScript []byte) string {
	addrBytes := make([]byte, 2+2*2+2*len(pkScript))
	addrBytes[0] = 0x30 // "0"
	addrBytes[1] = 0x78 // "x"
	versionBytes := []byte{byte(version >> 8), byte(version)}
	hex.Encode(addrBytes[2:6], versionBytes)
	hex.Encode(addrBytes[6:], pkScript)
	return string(addrBytes)
}

func dcrPkScriptToAccountAddr(version uint16, pkScript []byte, chainParams *chaincfg.Params) (string, error) {
	if version != 0 {
		// Versions other than 0 aren't standardized yet, so return as
		// a raw hex string with a "0x" prefix.
		return rawPkScriptToAccountAddr(version, pkScript), nil
	}

	_, addrs, _, err := txscript.ExtractPkScriptAddrs(version, pkScript, chainParams)
	if err != nil {
		// Currently the only possible error is due to version != 0,
		// which is handled above, but err on the side of caution.
		return "", err
	}

	if len(addrs) != 1 {
		// TODO: support 'bare' (non-p2sh) multisig?
		return rawPkScriptToAccountAddr(version, pkScript), nil
	}

	saddr := addrs[0].Address()
	return saddr, nil
}

type PrevInput struct {
	PkScript []byte
	Version  uint16
	Amount   dcrutil.Amount
}

type PrevInputsFetcher func(...*wire.OutPoint) (map[wire.OutPoint]*PrevInput, error)

type Op struct {
	Tree      int8
	Status    OpStatus
	Tx        *wire.MsgTx
	TxIndex   int
	IOIndex   int
	Account   string
	Type      OpType
	OpIndex   int64
	Amount    dcrutil.Amount
	In        *wire.TxIn
	Out       *wire.TxOut
	PrevInput *PrevInput
}

func (op *Op) ROp() *rtypes.Operation {
	account := &rtypes.AccountIdentifier{
		Address: op.Account,
	}
	var meta map[string]interface{}
	if op.Type == OpTypeDebit {
		meta = map[string]interface{}{
			"input_index":      op.IOIndex,
			"prev_hash":        op.In.PreviousOutPoint.Hash.String(),
			"prev_index":       op.In.PreviousOutPoint.Index,
			"prev_tree":        op.In.PreviousOutPoint.Tree,
			"sequence":         op.In.Sequence,
			"block_height":     op.In.BlockHeight,
			"block_index":      op.In.BlockIndex,
			"signature_script": op.In.SignatureScript,
			"script_version":   op.PrevInput.Version,
		}
	} else {
		meta = map[string]interface{}{
			"output_index":   op.IOIndex,
			"script_version": op.Out.Version,
		}
	}

	return &rtypes.Operation{
		OperationIdentifier: &rtypes.OperationIdentifier{
			Index: int64(op.OpIndex),
		},
		Type:     op.Type.RType(),
		Status:   string(op.Status),
		Account:  account,
		Amount:   DcrAmountToRosetta(op.Amount),
		Metadata: meta,
	}

}

type BlockOpCb = func(op *Op) error

func iterateBlockOpsInTx(op *Op, fetchInputs PrevInputsFetcher, applyOp BlockOpCb, chainParams *chaincfg.Params) error {
	tx := op.Tx
	isVote := op.Tree == wire.TxTreeStake && stake.IsSSGen(tx)
	isCoinbase := op.Tree == wire.TxTreeRegular && op.TxIndex == 0

	// Fetch the relevant data for the inputs.
	prevOutpoints := make([]*wire.OutPoint, 0, len(tx.TxIn))
	for i, in := range tx.TxIn {
		if i == 0 && (isVote || isCoinbase) {
			// Coinbases don't have an input with i > 0 so this is
			// safe.
			continue
		}

		prevOutpoints = append(prevOutpoints, &in.PreviousOutPoint)
	}
	prevInputs, err := fetchInputs(prevOutpoints...)
	if err != nil {
		return err
	}

	var ok bool

	// Reset op's output attributes.
	op.Out = nil

	// Helper to process the inputs.
	addTxIns := func() error {
		for i, in := range tx.TxIn {
			if i == 0 && (isVote || isCoinbase) {
				// Coinbases don't have an input with i > 0.
				continue
			}

			op.PrevInput, ok = prevInputs[in.PreviousOutPoint]
			if !ok {
				return fmt.Errorf("missing prev outpoint %s", in.PreviousOutPoint)
			}

			op.Account, err = dcrPkScriptToAccountAddr(op.PrevInput.Version,
				op.PrevInput.PkScript, chainParams)
			if err != nil {
				return err
			}
			if op.Account == "" {
				// Might happen for OP_RETURNs, ticket
				// commitments, etc.
				continue
			}

			// Fill in op input data.
			op.IOIndex = i
			op.In = in
			op.Type = OpTypeDebit
			op.Amount = -op.PrevInput.Amount
			if op.Status == OpStatusReversed {
				op.Amount *= -1
			}

			if err := applyOp(op); err != nil {
				return err
			}

			// Track cumulative OpIndex.
			op.OpIndex += 1
		}

		return nil
	}

	// Reset op's input attributes.
	op.In = nil
	op.PrevInput = nil

	// Helper to process the outputs.
	addTxOuts := func() error {
		for i, out := range tx.TxOut {
			if out.Value == 0 {
				// Ignore OP_RETURNs and other zero-valued
				// outputs.
				//
				// TODO: decode ticket commitments?
				continue
			}

			op.Account, err = dcrPkScriptToAccountAddr(out.Version,
				out.PkScript, chainParams)
			if err != nil {
				return err
			}
			if op.Account == "" {
				continue
			}

			// Fill in op output data.
			op.IOIndex = i
			op.Out = out
			op.Type = OpTypeCredit
			op.Amount = dcrutil.Amount(out.Value)
			if op.Status == OpStatusReversed {
				op.Amount *= -1
			}

			if err := applyOp(op); err != nil {
				return err
			}

			// Track cumulative OpIndex.
			op.OpIndex += 1
		}

		return nil
	}

	if op.Status == OpStatusSuccess {
		if err := addTxIns(); err != nil {
			return err
		}
		if err := addTxOuts(); err != nil {
			return err
		}
	} else {
		// When reversing a tx we apply the update in the opposite
		// order: first roll back outputs (which were crediting an
		// amount) then inputs (which were debiting the amount + fee).
		if err := addTxOuts(); err != nil {
			return err
		}
		if err := addTxIns(); err != nil {
			return err
		}
	}

	return nil
}

func IterateBlockOps(b, prev *wire.MsgBlock, fetchInputs PrevInputsFetcher, applyOp BlockOpCb, chainParams *chaincfg.Params) error {
	approvesParent := VoteBitsApprovesParent(b.Header.VoteBits) || b.Header.Height == 0
	if !approvesParent && prev == nil {
		return ErrNeedsPreviousBlock
	}

	// Use a single op var.
	var op Op

	// Helper to apply a set of transactions.
	applyTxs := func(tree int8, status OpStatus, txs []*wire.MsgTx) error {
		op = Op{
			Tree:   tree,
			Status: status,
		}
		for i, tx := range txs {
			op.Tx = tx
			op.TxIndex = i
			op.OpIndex = 0
			err := iterateBlockOpsInTx(&op, fetchInputs, applyOp,
				chainParams)
			if err != nil {
				return err
			}
		}

		return nil
	}

	if !approvesParent {
		// Reverse regular transactions of the previous block.
		if err := applyTxs(wire.TxTreeRegular, OpStatusReversed, prev.Transactions); err != nil {
			return err
		}
	}
	if err := applyTxs(wire.TxTreeRegular, OpStatusSuccess, b.Transactions); err != nil {
		return err
	}
	if err := applyTxs(wire.TxTreeStake, OpStatusSuccess, b.STransactions); err != nil {
		return err
	}

	return nil
}

func txMetaToRosetta(tx *wire.MsgTx) *rtypes.Transaction {
	return &rtypes.Transaction{
		TransactionIdentifier: &rtypes.TransactionIdentifier{
			Hash: tx.TxHash().String(),
		},
		Operations: []*rtypes.Operation{},
		Metadata: map[string]interface{}{
			"version":  tx.Version,
			"expiry":   tx.Expiry,
			"locktime": tx.LockTime,
		},
	}

}

// WireBlockToRosetta converts the given block in wire representation to the
// block in rosetta representation. The previous block is needed when the
// current block disapproved the regular transactions of the previous one, in
// which case it must be specified or this function errors.
func WireBlockToRosetta(b, prev *wire.MsgBlock, fetchInputs PrevInputsFetcher, chainParams *chaincfg.Params) (*rtypes.Block, error) {

	approvesParent := VoteBitsApprovesParent(b.Header.VoteBits) || b.Header.Height == 0
	if !approvesParent && prev == nil {
		return nil, ErrNeedsPreviousBlock
	}

	var txs []*rtypes.Transaction
	nbTxs := len(b.Transactions) + len(b.STransactions)
	if !approvesParent {
		nbTxs += len(prev.Transactions) + len(prev.STransactions)
	}
	txs = make([]*rtypes.Transaction, 0, nbTxs)

	// Closure that builds the list of transactions/ops by iterating over
	// the block's transactions.
	var tx *rtypes.Transaction
	applyOp := func(op *Op) error {
		if op.OpIndex == 0 {
			// Starting a new transaction.
			tx = txMetaToRosetta(op.Tx)
			txs = append(txs, tx)
		}
		tx.Operations = append(tx.Operations, op.ROp())
		return nil
	}

	// Build the list of transactions.
	err := IterateBlockOps(b, prev, fetchInputs, applyOp, chainParams)
	if err != nil {
		return nil, err
	}

	blockHash := b.Header.BlockHash()
	prevHeight := b.Header.Height - 1
	prevHash := b.Header.PrevBlock
	if b.Header.Height == 0 {
		// https://www.rosetta-api.org/docs/common_mistakes.html#malformed-genesis-block
		// currently (2020-05-24) recommends returning the same
		// identifier on both BlockIdentifier and ParentBlockIdentifier
		// on the genesis block.
		prevHeight = 0
		prevHash = blockHash
	}

	r := &rtypes.Block{
		BlockIdentifier: &rtypes.BlockIdentifier{
			Index: int64(b.Header.Height),
			Hash:  blockHash.String(),
		},
		ParentBlockIdentifier: &rtypes.BlockIdentifier{
			Index: int64(prevHeight),
			Hash:  prevHash.String(),
		},
		Timestamp:    b.Header.Timestamp.Unix() * 1000,
		Transactions: txs,
		Metadata: map[string]interface{}{
			"block_version":   b.Header.Version,
			"merkle_root":     b.Header.MerkleRoot.String(),
			"stake_root":      b.Header.StakeRoot.String(),
			"approves_parent": approvesParent,
			"vote_bits":       b.Header.VoteBits,
			"bits":            b.Header.Bits,
			"sbits":           b.Header.SBits,
		},
	}
	return r, nil
}

// MempoolTxToRosetta converts a wire tx that is known to be on the mempool to
// a rosetta tx.
func MempoolTxToRosetta(tx *wire.MsgTx, fetchInputs PrevInputsFetcher, chainParams *chaincfg.Params) (*rtypes.Transaction, error) {
	rtx := txMetaToRosetta(tx)
	applyOp := func(op *Op) error {
		rtx.Operations = append(rtx.Operations, op.ROp())
		return nil
	}

	txType := stake.DetermineTxType(tx)
	tree := wire.TxTreeRegular
	if txType != stake.TxTypeRegular {
		tree = wire.TxTreeStake
	}

	op := Op{
		Tree:   tree,
		Status: OpStatusSuccess,

		// Coinbase txs are never seen on the mempool so it's safe to
		// use a negative txidx.
		TxIndex: -1,
	}
	err := iterateBlockOpsInTx(&op, fetchInputs, applyOp,
		chainParams)
	if err != nil {
		return nil, err
	}

	return rtx, nil
}
