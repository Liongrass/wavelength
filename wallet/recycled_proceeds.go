package wallet

import (
	"context"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/ledger"
)

// emitRecycledProceedsChange credits back the change of a boarding deposit
// funded by coins the client already credited to wallet_balance.
//
// Own-wallet proceeds -- a unilateral exit's sweep output, a cooperative
// leave paying a script the daemon minted, or the change of an earlier such
// spend -- are coins the ledger has already booked onto wallet_balance at a
// known outpoint. Boarding them credits the same satoshis a second time
// through the deposit's own leg, so the ledger reverses the earlier credit
// for every funding input it recognises. That reversal removes the whole
// input, including the part that came straight back as change, so the change
// has to be credited again or the client is understated by it.
//
// The wallet's job here is only to report facts: which transaction funded the
// deposit, and which of its other outputs pay scripts this daemon minted. The
// reversal itself lives in the ledger actor, where every handler runs
// serialized in its own committed transaction and the two halves of the story
// can arrive in either order.
//
// This depends on the owned-script registry, which is written at mint time. A
// change output paying a script minted before the registry existed is not
// recognised and stays uncredited. That understates the client rather than
// overstating it, and it does not compound: an unrecognised change output is
// never credited, so there is no credit for a later board of it to reverse.
func (a *Ark) emitRecycledProceedsChange(ctx context.Context,
	fundingTx *wire.MsgTx, depositOutpoint wire.OutPoint,
	blockHeight int32) error {

	if a.ledgerSink.IsNone() || a.ownedScripts == nil || fundingTx == nil {
		return nil
	}

	fundingTxid := fundingTx.TxHash()

	for index, txOut := range fundingTx.TxOut {
		if txOut == nil || txOut.Value <= 0 {
			continue
		}

		// The boarding output itself is booked by the deposit leg.
		outpoint := wire.OutPoint{
			Hash:  fundingTxid,
			Index: uint32(index),
		}
		if outpoint == depositOutpoint {
			continue
		}

		owned, err := a.ownedScripts.IsOwnedWalletScript(
			ctx, txOut.PkScript,
		)
		if err != nil {
			return err
		}
		if !owned {
			continue
		}

		utxo := &Utxo{
			Outpoint: outpoint,
			PkScript: txOut.PkScript,
			Amount:   btcutil.Amount(txOut.Value),
		}
		if err := a.emitUTXOCreated(
			ctx, utxo, blockHeight,
			ledger.ClassificationRecycledChange, nil,
		); err != nil {
			return err
		}
	}

	return nil
}

// fundingInputs returns the previous outpoints a transaction spent, for the
// ledger to match against the own-wallet proceeds it has recorded.
func fundingInputs(tx *wire.MsgTx) []wire.OutPoint {
	if tx == nil {
		return nil
	}

	inputs := make([]wire.OutPoint, 0, len(tx.TxIn))
	for _, txIn := range tx.TxIn {
		if txIn == nil {
			continue
		}

		inputs = append(inputs, txIn.PreviousOutPoint)
	}

	return inputs
}
