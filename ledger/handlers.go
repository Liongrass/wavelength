package ledger

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	fn "github.com/lightningnetwork/lnd/fn/v2"
)

// zeroRoundID is the zero value used to detect empty round IDs.
var zeroRoundID [16]byte

// zeroSessionID is the zero value used to detect empty session IDs.
var zeroSessionID [32]byte

// roundIDOrNil converts a 16-byte round ID to a slice, returning
// nil for zero-valued IDs so the database stores NULL (which
// correctly bypasses the conditional idempotency unique index).
func roundIDOrNil(id [16]byte) []byte {
	if id == zeroRoundID {
		return nil
	}

	return id[:]
}

// sessionIDOrNil converts a 32-byte session ID to a slice,
// returning nil for zero-valued IDs so the database stores NULL
// (which correctly bypasses the conditional idempotency unique
// index on session_id).
func sessionIDOrNil(id [32]byte) []byte {
	if id == zeroSessionID {
		return nil
	}

	return id[:]
}

// handleFeePaid records a fee payment by the client. Fees paid
// during boarding or refresh are debited from fees_paid and
// credited to vtxo_balance (the fee reduces the client's VTXO
// balance).
//
// Validation and entry construction run before Commit, with no writer
// lock held; only the single InsertLedgerEntry runs inside the
// lease-fenced Commit transaction.
func (a *LedgerActor) handleFeePaid(ctx context.Context, msg *FeePaidMsg,
	ax actor.Exec[ledgerTx]) fn.Result[LedgerResp] {

	const errMsg = "Failed to handle fee paid"

	// Reject non-positive amounts up front so a malformed TLV
	// (e.g. a zero payload or a uint64 that decoded past
	// math.MaxInt64) surfaces as ErrInvalidMessage instead of
	// hitting the SQL CHECK constraint and driving a durable
	// retry loop on a permanent failure.
	if msg.AmountSat <= 0 {
		return a.fail(
			ctx, errMsg, fmt.Errorf("%w: FeePaidMsg amount_sat "+
				"must be positive (got %d)", ErrInvalidMessage,
				msg.AmountSat),
		)
	}

	roundID := roundIDOrNil(msg.RoundID)

	// The credit side names the account the fee was actually paid from.
	// A boarding fee comes out of the on-chain wallet funds entering the
	// Ark layer: the boarding vtxo_received leg books the SEALED (net of
	// fee) VTXO value, so crediting wallet_balance here completes the
	// gross wallet outflow while leaving vtxo_balance equal to the sealed
	// VTXO sum. A refresh fee is carved out of forfeited VTXO value, so it
	// credits vtxo_balance. Onchain-sweep fees book against onchain_fees /
	// wallet_clearing instead — they are L1 chain costs paid by a
	// wallet-internal sweep, not Ark protocol operator fees, and the
	// fee is settled through wallet clearing rather than VTXO balance.
	var (
		eventType     string
		debitAccount  string
		creditAccount string
		idempotency   []byte
		description   string
	)
	switch msg.FeeType {
	case FeeTypeBoarding:
		eventType = EventBoardingFeePaid
		debitAccount = AccountFeesPaid
		creditAccount = AccountWalletBalance
		description = fmt.Sprintf("%s fee paid in round %x",
			msg.FeeType, msg.RoundID)

	case FeeTypeRefresh:
		eventType = EventRefreshFeePaid
		debitAccount = AccountFeesPaid
		creditAccount = AccountVTXOBalance
		description = fmt.Sprintf("%s fee paid in round %x",
			msg.FeeType, msg.RoundID)

	case FeeTypeOnchainSweep:
		eventType = EventBoardingSweepFeePaid
		debitAccount = AccountOnchainFees
		creditAccount = AccountWalletClearing

		// Onchain-sweep fees are not associated with a round.
		// Use the sweep txid (carried in IdempotencyKey by the
		// caller) to dedup replays via the
		// idx_client_ledger_idempotent_key partial unique index.
		// Round-keyed dedup is intentionally bypassed by setting
		// roundID to nil below.
		if len(msg.IdempotencyKey) != chainhash.HashSize {
			return a.fail(
				ctx, errMsg,
				fmt.Errorf(
					"%w: FeePaidMsg onchain-sweep "+
						"idempotency_key must be %d "+
						"bytes (got %d)",
					ErrInvalidMessage, chainhash.HashSize,
					len(msg.IdempotencyKey),
				),
			)
		}

		roundID = nil
		idempotency = msg.IdempotencyKey
		description = "boarding-sweep on-chain cost"

	default:
		return a.fail(
			ctx, errMsg, fmt.Errorf("%w: unknown fee type %q",
				ErrInvalidMessage, msg.FeeType),
		)
	}

	a.log.InfoS(ctx, "Recording fee payment",
		slog.String("round_id",
			fmt.Sprintf("%x", msg.RoundID)),
		slog.Int64("amount_sat", msg.AmountSat),
		slog.String("fee_type", msg.FeeType),
		slog.Uint64("block_height",
			uint64(msg.BlockHeight)),
	)

	entry := LedgerEntry{
		DebitAccount:   debitAccount,
		CreditAccount:  creditAccount,
		AmountSat:      msg.AmountSat,
		RoundID:        roundID,
		IdempotencyKey: idempotency,
		EventType:      eventType,
		Description:    description,
		CreatedAt:      a.clk.Now().Unix(),
	}

	return a.commit(ctx, ax, errMsg, func(ctx context.Context,
		q ledgerTx) error {

		return q.ledger.InsertLedgerEntry(ctx, entry)
	})
}

// handleVTXOReceived records a VTXO received by the client.
// For OOR transfers, the counterparty side is booked to
// transfers_in (debit vtxo_balance, credit transfers_in). For
// round receipts, the balance moves from wallet_balance to
// vtxo_balance.
func (a *LedgerActor) handleVTXOReceived(ctx context.Context,
	msg *VTXOReceivedMsg, ax actor.Exec[ledgerTx]) fn.Result[LedgerResp] {

	const errMsg = "Failed to handle VTXO received"

	// Reject non-positive amounts up front; see handleFeePaid
	// for the rationale.
	if msg.AmountSat <= 0 {
		return a.fail(
			ctx, errMsg, fmt.Errorf("%w: VTXOReceivedMsg "+
				"amount_sat must be positive (got %d)",
				ErrInvalidMessage, msg.AmountSat),
		)
	}

	roundID := roundIDOrNil(msg.RoundID)

	a.log.InfoS(ctx, "Recording VTXO received",
		slog.String(
			"outpoint", fmt.Sprintf("%x:%d", msg.OutpointHash,
				msg.OutpointIndex),
		),
		slog.Int64("amount_sat", msg.AmountSat),
		slog.String("source", msg.Source),
	)

	var (
		debitAccount  string
		creditAccount string
		source        = msg.Source

		// skip is set by classify when this outpoint already carries a
		// receive leg, so the commit body writes nothing.
		skip bool
	)

	// An OOR receive under a session this ledger already booked an
	// outgoing send for is the sender's own change coming back. The
	// ledger's own rows are the oracle: nothing here depends on a
	// caller-supplied idempotency key or on a session row that later
	// flips direction.
	//
	// What makes the ordering hold is the correlation key both messages
	// carry, not the mailbox's global claim order. That order is
	// (priority, available_at, created_at) with no session correlation and
	// second-granularity timestamps, so a send that gets nacked once --
	// a busy writer is enough -- has its available_at pushed past the
	// receive and would otherwise be overtaken, booking the receive as
	// transfers_in with nothing left to repair it. Both messages key their
	// lane on the session id, and the claim SQL never hands out a keyed
	// message while an earlier same-key message is still queued, retry
	// backoff included. A head that exhausts its attempts is passed over
	// rather than blocking the lane forever, so a poisoned send cannot
	// wedge its session's receives.
	// Per-VTXO idempotency key so multiple owned receives in
	// the same round (three-way directed send, multi-leg refresh,
	// a round with both a boarding intent and a received transfer)
	// don't collide on idx_client_ledger_idempotent_round. The
	// partial round/session indexes stay as defense-in-depth
	// against a caller that omits the outpoint.
	idempotencyKey := walletUTXOIdempotencyKey(
		msg.OutpointHash, msg.OutpointIndex,
	)

	classify := func(ctx context.Context, q ledgerTx) error {
		// A receive already booked at this outpoint is done, whatever
		// account pair it was booked with. The insert's unique index
		// includes the accounts, so it would let a second producer --
		// wallet recovery re-emitting a descriptor the live path
		// already booked -- through as a fresh row whenever the
		// classification below flipped in between. Chain identity is
		// the thing that cannot be booked twice, so it is what we
		// check.
		booked, err := q.ledger.HasEntryForKey(
			ctx, idempotencyKey, EventVTXOReceived,
		)
		if err != nil {
			return fmt.Errorf("look up booked receive %x:%d: %w",
				msg.OutpointHash, msg.OutpointIndex, err)
		}
		if booked {
			skip = true

			return nil
		}

		if source != SourceOOR || msg.SessionID == zeroSessionID {
			return nil
		}

		sent, err := q.ledger.HasSessionEntry(
			ctx, msg.SessionID, EventVTXOSent, AccountTransfersOut,
			AccountVTXOBalance,
		)
		if err != nil {
			return fmt.Errorf("classify OOR receive for session "+
				"%x: %w", msg.SessionID, err)
		}
		if sent {
			source = SourceOORSelfChange
			debitAccount = AccountVTXOBalance
			creditAccount = AccountTransfersOut
		}

		return nil
	}

	switch msg.Source {
	case SourceOOR:
		// OOR receive from another participant: counterparty
		// side is transfers_in. classify upgrades this to the
		// self-change booking inside the commit when the session
		// already carries an outgoing send.
		debitAccount = AccountVTXOBalance
		creditAccount = AccountTransfersIn

	case SourceRoundTransfer:
		// In-round receive from another participant: same
		// counterparty treatment as OOR.
		debitAccount = AccountVTXOBalance
		creditAccount = AccountTransfersIn

	case SourceRoundBoarding:
		// Boarding of the client's own on-chain funds: the
		// offsetting leg moves wallet_balance value into
		// vtxo_balance. Refresh is NOT booked here; refresh
		// uses SourceRoundRefresh so wallet_balance doesn't
		// drift on a flow that never touched the wallet.
		debitAccount = AccountVTXOBalance
		creditAccount = AccountWalletBalance

	case SourceOORSelfChange:
		// The sender's own change from an outgoing OOR transfer. The
		// outgoing session already debited transfers_out for the full
		// input sum, so crediting transfers_out here cancels the part
		// that never left and leaves the gross send figure equal to
		// what the recipient actually got. Booking it as transfers_in
		// would inflate both gross directions by the change amount.
		debitAccount = AccountVTXOBalance
		creditAccount = AccountTransfersOut

	case SourceRoundRefresh:
		// Refresh output (including directed-send self-change):
		// the VTXO came from a forfeited VTXO in the same round,
		// not from the wallet. Credit transfers_out so this leg
		// cancels the companion VTXOSentMsg's transfers_out
		// debit for the gross forfeited amount. Net effect on
		// transfers_out is zero; net effect on vtxo_balance is
		// exactly the operator fee, which the paired
		// FeePaidMsg(refresh) removes.
		debitAccount = AccountVTXOBalance
		creditAccount = AccountTransfersOut

	default:
		return a.fail(
			ctx, errMsg, fmt.Errorf("%w: unknown vtxo source %q",
				ErrInvalidMessage, msg.Source),
		)
	}

	// Surface the VTXO outpoint on the row's structured chain
	// fields too. Without these the consumer-facing onchain view
	// renders a "round"-kind entry with an empty txid and has to
	// string-parse the description to recover the outpoint — see
	// issue #504.
	chainVout := int32(msg.OutpointIndex)

	return a.commit(ctx, ax, errMsg, func(ctx context.Context,
		q ledgerTx) error {

		if err := classify(ctx, q); err != nil {
			return err
		}
		if skip {
			a.log.DebugS(ctx, "Skipping VTXO receive already "+
				"booked at this outpoint",
				slog.String(
					"outpoint", fmt.Sprintf("%x:%d",
						msg.OutpointHash,
						msg.OutpointIndex),
				),
			)

			return nil
		}

		entry := LedgerEntry{
			DebitAccount:  debitAccount,
			CreditAccount: creditAccount,
			AmountSat:     msg.AmountSat,
			RoundID:       roundID,
			EventType:     EventVTXOReceived,
			Description: fmt.Sprintf(
				"VTXO received via %s: %x:%d",
				source, msg.OutpointHash,
				msg.OutpointIndex,
			),
			CreatedAt:      a.clk.Now().Unix(),
			IdempotencyKey: idempotencyKey,
			ChainTxid:      msg.OutpointHash[:],
			ChainVout:      &chainVout,
		}

		return q.ledger.InsertLedgerEntry(ctx, entry)
	})
}

// handleVTXOSent records a VTXO leaving the client's balance,
// either as an out-of-round transfer (SessionID non-zero) or as
// an in-round participant-to-participant send (RoundID
// non-zero). Exactly one of the two identifiers must be set:
// both-zero is ambiguous ("unknown send context") and both-set
// is contradictory ("cannot route to both"). The counterparty
// side is debited to transfers_out so gross send flows are
// tracked independently of received flows.
func (a *LedgerActor) handleVTXOSent(ctx context.Context, msg *VTXOSentMsg,
	ax actor.Exec[ledgerTx]) fn.Result[LedgerResp] {

	const errMsg = "Failed to handle VTXO sent"

	// Reject non-positive amounts up front; see handleFeePaid
	// for the rationale.
	if msg.AmountSat <= 0 {
		return a.fail(
			ctx, errMsg, fmt.Errorf("%w: VTXOSentMsg amount_sat "+
				"must be positive (got %d)", ErrInvalidMessage,
				msg.AmountSat),
		)
	}

	sessionID := sessionIDOrNil(msg.SessionID)
	roundID := roundIDOrNil(msg.RoundID)

	switch {
	case sessionID == nil && roundID == nil:
		return a.fail(
			ctx, errMsg, fmt.Errorf("%w: VTXOSentMsg requires "+
				"one of SessionID or RoundID to be non-zero",
				ErrInvalidMessage),
		)

	case sessionID != nil && roundID != nil:
		return a.fail(
			ctx, errMsg, fmt.Errorf("%w: VTXOSentMsg cannot set "+
				"both SessionID and RoundID",
				ErrInvalidMessage),
		)
	}

	a.log.InfoS(ctx, "Recording VTXO sent",
		slog.String("session_id",
			fmt.Sprintf("%x", msg.SessionID)),
		slog.String("round_id",
			fmt.Sprintf("%x", msg.RoundID)),
		slog.String("outpoint", msg.Outpoint.String()),
		slog.Int64("amount_sat", msg.AmountSat),
	)

	var description string
	if sessionID != nil {
		description = fmt.Sprintf("VTXO sent in OOR session %x",
			msg.SessionID)
	} else {
		description = fmt.Sprintf("VTXO sent in round %x", msg.RoundID)
	}

	// A caller-supplied key is used for round-scoped sends that do
	// not correspond to a local VTXO outpoint, such as cooperative
	// leave outputs and foreign directed-send recipient outputs.
	// Otherwise an outpoint-derived key disambiguates per-VTXO sends.
	var idempotencyKey []byte
	switch {
	case len(msg.IdempotencyKey) > 0:
		idempotencyKey = msg.IdempotencyKey

	case !msg.Outpoint.Hash.IsEqual(&zeroHash):
		idempotencyKey = refreshSendIdempotencyKey(
			msg.Outpoint.Hash, msg.Outpoint.Index,
		)
	}

	now := a.clk.Now().Unix()
	entry := LedgerEntry{
		DebitAccount:   AccountTransfersOut,
		CreditAccount:  AccountVTXOBalance,
		AmountSat:      msg.AmountSat,
		SessionID:      sessionID,
		RoundID:        roundID,
		EventType:      EventVTXOSent,
		Description:    description,
		CreatedAt:      now,
		IdempotencyKey: idempotencyKey,
	}

	// A send that paid the client's own backing wallet did not leave: the
	// value crossed from the off-chain asset to the on-chain one. That
	// movement is its own leg, keyed separately, so it cancels the send
	// leg on transfers_out and lands the value on wallet_balance without
	// touching the send leg's dedup identity -- which it must not, because
	// the accounts are part of the dedup tuple and a send leg that
	// switched accounts with the flag would land in a different tuple than
	// its pre-flag twin and double-credit vtxo_balance on replay. This is
	// the same shape handleExitCost uses for an own-wallet exit.
	//
	// The proceeds leg needs its own key, and the send leg's key is
	// whatever the producer supplied, so the leg name is appended to it
	// rather than derived from an outpoint the leave does not have.
	//
	// The flag and the key are coupled: the paired leave_proceeds audit row
	// suppresses the deposit credit on the strength of this leg existing,
	// so a flag arriving without a key would leave the coins uncredited in
	// both places. Producers always set one today; fail loudly rather than
	// degrade quietly if that ever stops holding.
	if msg.ProceedsOwnWallet && len(idempotencyKey) == 0 {
		return a.fail(
			ctx, errMsg, fmt.Errorf("%w: VTXOSentMsg sets "+
				"proceeds_own_wallet without an idempotency "+
				"key, which the proceeds leg is keyed from",
				ErrInvalidMessage),
		)
	}

	var proceedsLeg fn.Option[LedgerEntry]
	if msg.ProceedsOwnWallet {
		proceedsLeg = fn.Some(LedgerEntry{
			DebitAccount:  AccountWalletBalance,
			CreditAccount: AccountTransfersOut,
			AmountSat:     msg.AmountSat,
			SessionID:     sessionID,
			RoundID:       roundID,
			EventType:     EventVTXOSent,
			Description:   description + " (own-wallet proceeds)",
			CreatedAt:     now,
			IdempotencyKey: sendProceedsIdempotencyKey(
				idempotencyKey,
			),
		})
	}

	return a.commit(ctx, ax, errMsg, func(ctx context.Context,
		q ledgerTx) error {

		if err := q.ledger.InsertLedgerEntry(ctx, entry); err != nil {
			return fmt.Errorf("send leg: %w", err)
		}

		var proceedsErr error
		proceedsLeg.WhenSome(func(leg LedgerEntry) {
			proceedsErr = q.ledger.InsertLedgerEntry(ctx, leg)
		})
		if proceedsErr != nil {
			return fmt.Errorf("send proceeds leg: %w", proceedsErr)
		}

		return nil
	})
}

// sendProceedsIdempotencyKey derives the identity of the leg that lands an
// own-wallet send's value on wallet_balance. It scopes the send's own key
// under the versioned proceeds leg name, so the proceeds leg is unique
// wherever the send leg was and the send leg's identity is untouched.
func sendProceedsIdempotencyKey(sendKey []byte) []byte {
	return ledgerIdempotencyKey(operationSend, legProceeds, sendKey)
}

// zeroHash is a convenience sentinel for detecting an absent
// wire.OutPoint hash.
var zeroHash chainhash.Hash

// handleExitCost records a unilateral exit as two ledger entries
// that together reduce vtxo_balance by the gross exited amount:
//
//  1. Send leg: debit (AmountSat - ExitCostSat) crediting
//     vtxo_balance. The debit side is wallet_balance when the exit
//     paid an output this client's own wallet controls, since the
//     value only crossed between two accounts it owns, and
//     transfers_out when the destination is foreign and the value
//     genuinely left.
//  2. Fee leg:  debit onchain_fees  += ExitCostSat crediting
//     vtxo_balance. The L1 miner fee portion.
//
// Both entries land in the durable actor's delivery transaction
// via two InsertLedgerEntry calls that join the outer tx. Either
// both commit or neither does: a handler-level error returns
// non-nil, the durable actor nacks, and the whole tx (including
// a possibly-successful first insert) rolls back. Redelivery of
// a committed message cannot happen because Ack/MarkProcessed
// land in the same tx; defensive protection against out-of-band
// replays is provided by separate, versioned operation-and-leg
// identities. Both hit the partial unique index
// idx_client_ledger_idempotent_key; the adapter accepts an
// identical winner and rejects a conflicting payload.
//
// On-chain wallet-side balance movement is intentionally not booked
// here: this handler records the VTXO-funded exit value and confirmed
// sweep cost supplied by the unroll path.
func (a *LedgerActor) handleExitCost(ctx context.Context, msg *ExitCostMsg,
	ax actor.Exec[ledgerTx]) fn.Result[LedgerResp] {

	const errMsg = "Failed to handle exit cost"

	a.log.InfoS(ctx, "Recording exit cost",
		slog.String(
			"outpoint", fmt.Sprintf("%x:%d", msg.OutpointHash,
				msg.OutpointIndex),
		),
		slog.Int64("amount_sat", msg.AmountSat),
		slog.Int64("exit_cost_sat", msg.ExitCostSat),
		slog.Uint64("block_height",
			uint64(msg.BlockHeight)),
	)

	// Guard against pathological exits where the fee consumes
	// the entire VTXO (or more). Such an exit has no send leg
	// to record and the amount_sat > 0 CHECK would reject it
	// anyway; fail fast with a clear error instead.
	if msg.ExitCostSat <= 0 || msg.AmountSat <= 0 {
		return a.fail(
			ctx, errMsg, fmt.Errorf("%w: exit cost requires "+
				"positive amount_sat and exit_cost_sat "+
				"(got %d, %d)", ErrInvalidMessage,
				msg.AmountSat, msg.ExitCostSat),
		)
	}

	if msg.ExitCostSat >= msg.AmountSat {
		return a.fail(
			ctx, errMsg, fmt.Errorf("%w: exit cost %d exceeds or "+
				"equals VTXO amount %d for %x:%d",
				ErrInvalidMessage, msg.ExitCostSat,
				msg.AmountSat, msg.OutpointHash,
				msg.OutpointIndex),
		)
	}

	now := a.clk.Now().Unix()
	netAmount := msg.AmountSat - msg.ExitCostSat
	chainVout := int32(msg.OutpointIndex)
	confirmationHeight := int32(msg.BlockHeight)
	sendKey := exitSendIdempotencyKey(
		msg.OutpointHash, msg.OutpointIndex,
	)
	feeKey := exitFeeIdempotencyKey(msg.OutpointHash, msg.OutpointIndex)

	// The exited VTXO outpoint is the stable identity shared by all
	// accounting legs. ConfirmationHeight intentionally records the final
	// sweep height that completed the exit, not a confirmation of that
	// outpoint transaction.
	//
	// The send leg always settles on transfers_out. Its accounts are part
	// of the dedup tuple in idx_client_ledger_idempotent_key, so a leg
	// whose debit account followed the destination flag would land in a
	// different tuple than its pre-flag twin and both would persist: a
	// resumed unroll job that re-emits with the flag set would credit
	// vtxo_balance a second time for the same exit. Keeping the accounts
	// fixed makes every redelivery of this outpoint dedup against the row
	// it wrote first, whichever flag it carried.
	sendLeg := LedgerEntry{
		DebitAccount:  AccountTransfersOut,
		CreditAccount: AccountVTXOBalance,
		AmountSat:     netAmount,
		EventType:     EventVTXOSent,
		Description: fmt.Sprintf(
			"unilateral exit net value for %x:%d at height %d",
			msg.OutpointHash, msg.OutpointIndex, msg.BlockHeight,
		),
		CreatedAt:          now,
		IdempotencyKey:     sendKey,
		ChainTxid:          msg.OutpointHash[:],
		ChainVout:          &chainVout,
		ConfirmationHeight: &confirmationHeight,
	}

	// Where the exited value landed decides whether that outflow stands.
	// An exit paying an output this client's own wallet controls did not
	// leave: the value crossed from the off-chain asset to the on-chain
	// one. That movement is its own leg, keyed separately, so it cancels
	// the send leg on transfers_out and lands the value on
	// wallet_balance without touching the send leg's dedup identity.
	var proceedsLeg fn.Option[LedgerEntry]
	if msg.DestinationOwnWallet {
		proceedsLeg = fn.Some(LedgerEntry{
			DebitAccount:  AccountWalletBalance,
			CreditAccount: AccountTransfersOut,
			AmountSat:     netAmount,
			EventType:     EventVTXOSent,
			Description: fmt.Sprintf(
				"unilateral exit proceeds to own wallet for "+
					"%x:%d at height %d", msg.OutpointHash,
				msg.OutpointIndex, msg.BlockHeight,
			),
			CreatedAt: now,
			IdempotencyKey: exitProceedsIdempotencyKey(
				msg.OutpointHash, msg.OutpointIndex,
			),
			ChainTxid:          msg.OutpointHash[:],
			ChainVout:          &chainVout,
			ConfirmationHeight: &confirmationHeight,
		})
	}

	feeLeg := LedgerEntry{
		DebitAccount:  AccountOnchainFees,
		CreditAccount: AccountVTXOBalance,
		AmountSat:     msg.ExitCostSat,
		EventType:     EventOnchainFeePaid,
		Description: fmt.Sprintf(
			"exit cost for %x:%d at height %d",
			msg.OutpointHash,
			msg.OutpointIndex,
			msg.BlockHeight,
		),
		CreatedAt:          now,
		IdempotencyKey:     feeKey,
		ChainTxid:          msg.OutpointHash[:],
		ChainVout:          &chainVout,
		ConfirmationHeight: &confirmationHeight,
	}

	// Book every leg via InsertLedgerEntry calls inside ONE Commit. They
	// all join the same lease-fenced writer transaction, so a crash or
	// error between them rolls back every write and the mailbox ack
	// together -- no partial-write window. The separately namespaced
	// outpoint identities make an out-of-band replay resolve to the same
	// rows via the partial unique index.
	return a.commit(ctx, ax, errMsg, func(ctx context.Context,
		q ledgerTx) error {

		if err := q.ledger.InsertLedgerEntry(ctx, sendLeg); err != nil {
			return fmt.Errorf("exit send leg: %w", err)
		}

		if err := q.ledger.InsertLedgerEntry(ctx, feeLeg); err != nil {
			return fmt.Errorf("exit fee leg: %w", err)
		}

		var proceedsErr error
		proceedsLeg.WhenSome(func(leg LedgerEntry) {
			proceedsErr = q.ledger.InsertLedgerEntry(ctx, leg)
		})
		if proceedsErr != nil {
			return fmt.Errorf("exit proceeds leg: %w", proceedsErr)
		}

		return nil
	})
}

const ledgerIdempotencyVersion = "ledger:v1:"

const (
	operationSend           = "send"
	operationRoundRefresh   = "round_refresh"
	operationUnilateralExit = "unilateral_exit"
	legSend                 = "send"
	legSpend                = "spend"
	legFee                  = "fee"
	legProceeds             = "proceeds"
)

// outpointIdempotencyPayload returns the stable natural identity shared by
// outpoint-scoped operations. Refresh and exit handlers always store it under
// a versioned operation-and-leg prefix.
func outpointIdempotencyPayload(hash [32]byte, index uint32) []byte {
	out := make([]byte, 32+4)
	copy(out[:32], hash[:])
	out[32] = byte(index >> 24)
	out[33] = byte(index >> 16)
	out[34] = byte(index >> 8)
	out[35] = byte(index)

	return out
}

// ledgerIdempotencyKey scopes a natural identity by a versioned business
// operation and ledger leg. The textual prefix is stable and human-readable
// while the payload remains opaque binary data.
func ledgerIdempotencyKey(operation, leg string, payload []byte) []byte {
	prefix := ledgerIdempotencyVersion + operation + ":" + leg + ":"
	key := make([]byte, 0, len(prefix)+len(payload))
	key = append(key, prefix...)

	return append(key, payload...)
}

// refreshSendIdempotencyKey derives the identity of the synthetic send leg
// paired with a replacement VTXO created by a refresh round.
func refreshSendIdempotencyKey(hash [32]byte, index uint32) []byte {
	return ledgerIdempotencyKey(
		operationRoundRefresh, legSend,
		outpointIdempotencyPayload(hash, index),
	)
}

// RefreshSendIdempotencyKey exposes refresh-send key derivation to migration
// code that rewrites legacy outpoint-only ledger identities.
func RefreshSendIdempotencyKey(hash [32]byte, index uint32) []byte {
	return refreshSendIdempotencyKey(hash, index)
}

// exitSendIdempotencyKey derives the unilateral exit's net-value send leg.
func exitSendIdempotencyKey(hash [32]byte, index uint32) []byte {
	return ledgerIdempotencyKey(
		operationUnilateralExit, legSend,
		outpointIdempotencyPayload(hash, index),
	)
}

// ExitSendIdempotencyKey exposes exit-send key derivation to migration code.
func ExitSendIdempotencyKey(hash [32]byte, index uint32) []byte {
	return exitSendIdempotencyKey(hash, index)
}

// exitProceedsIdempotencyKey derives the leg that lands an own-wallet
// exit's net value on wallet_balance. It is distinct from the send leg so the
// send leg's dedup identity stays fixed whatever the destination flag says.
func exitProceedsIdempotencyKey(hash [32]byte, index uint32) []byte {
	return ledgerIdempotencyKey(
		operationUnilateralExit, legProceeds,
		outpointIdempotencyPayload(hash, index),
	)
}

// ExitProceedsIdempotencyKey exposes the proceeds-leg identity to the
// accounting invariant checker, which pairs an operation's legs by key.
func ExitProceedsIdempotencyKey(hash [32]byte, index uint32) []byte {
	return exitProceedsIdempotencyKey(hash, index)
}

// exitFeeIdempotencyKey derives the unilateral exit's on-chain fee leg.
func exitFeeIdempotencyKey(hash [32]byte, index uint32) []byte {
	return ledgerIdempotencyKey(
		operationUnilateralExit, legFee,
		outpointIdempotencyPayload(hash, index),
	)
}

// ExitFeeIdempotencyKey exposes the fee-leg identity to read-side consumers
// and migration code.
func ExitFeeIdempotencyKey(hash [32]byte, index uint32) []byte {
	return exitFeeIdempotencyKey(hash, index)
}

// ExitIdempotencyKey returns the unilateral exit fee identity used by the
// confirmed-cost read path.
func ExitIdempotencyKey(hash [32]byte, index uint32) []byte {
	return exitFeeIdempotencyKey(hash, index)
}

// handleUTXOCreated records a new wallet UTXO in two places:
//
//  1. The wallet_utxo_log audit trail via UTXOAuditStore, tagged
//     with the caller-supplied classification.
//  2. The double-entry ledger as a deposit leg "debit
//     wallet_balance, credit opening_balance". opening_balance
//     is an equity account acting as the source of funds. This
//     leg is what balances the matching "debit vtxo_balance,
//     credit wallet_balance" leg that SourceRoundBoarding writes
//     when the same wallet UTXO is later consumed by a round;
//     without this deposit leg wallet_balance would drift negative
//     on every boarding.
//
// Both inserts join the outer durable-actor transaction via
// actor.TxFromContext / db.TransactionExecutor.ExecTx, so a crash
// between them rolls back both together with the mailbox ack.
// The ledger leg uses an outpoint-derived idempotency key so a
// replayed UTXOCreatedMsg dedupes silently via the partial unique
// index idx_client_ledger_idempotent_key.
//
// UTXOAuditStore is optional: when nil, both the audit entry and
// the ledger entry are skipped (the actor is in "log-only" mode).
// This mirrors the pre-existing behavior; callers wanting the
// double-entry row must wire the audit store.
//
// Non-positive amounts are rejected up front with ErrInvalidMessage
// so a malformed TLV dead-letters instead of hitting the SQL
// CHECK (amount_sat > 0) and driving an infinite nack-and-retry
// loop. A zero/negative on-chain UTXO is impossible in practice
// (wire enforces MaxSatoshi bounds on tx outputs) but the guard
// closes the last corruption gap on the TLV decode path.
func (a *LedgerActor) handleUTXOCreated(ctx context.Context,
	msg *UTXOCreatedMsg, ax actor.Exec[ledgerTx]) fn.Result[LedgerResp] {

	const errMsg = "Failed to handle UTXO created"

	a.log.InfoS(ctx, "Recording UTXO created",
		slog.String(
			"outpoint", fmt.Sprintf("%x:%d", msg.OutpointHash,
				msg.OutpointIndex),
		),
		slog.Int64("amount_sat", msg.AmountSat),
		slog.Uint64("block_height",
			uint64(msg.BlockHeight)),
		slog.String("classification",
			msg.Classification),
	)

	// UTXOAuditStore is optional: in log-only mode there is nothing to
	// persist, so consume the message without opening a Commit.
	if a.cfg.UTXOAuditStore == nil {
		return fn.Ok[LedgerResp](nil)
	}

	if msg.AmountSat <= 0 {
		return a.fail(
			ctx, errMsg, fmt.Errorf("%w: UTXOCreatedMsg "+
				"amount_sat must be positive (got %d)",
				ErrInvalidMessage, msg.AmountSat),
		)
	}

	now := a.clk.Now().Unix()
	chainVout := int32(msg.OutpointIndex)
	confirmationHeight := int32(msg.BlockHeight)

	audit := UTXOAuditEntry{
		OutpointHash:  msg.OutpointHash[:],
		OutpointIndex: int32(msg.OutpointIndex),
		AmountSat:     msg.AmountSat,
		Event:         "created",
		BlockHeight:   int32(msg.BlockHeight),
		ClassifiedAs:  msg.Classification,
		CreatedAt:     now,
	}

	creditAccount := AccountOpeningBalance
	if msg.Classification == ClassificationBoardingSweepReturn {
		creditAccount = AccountWalletClearing
	}

	entry := LedgerEntry{
		DebitAccount:  AccountWalletBalance,
		CreditAccount: creditAccount,
		AmountSat:     msg.AmountSat,
		EventType:     EventWalletUTXOCreated,
		Description: fmt.Sprintf(
			"wallet UTXO confirmed at %x:%d "+
				"(classification %s) at height %d",
			msg.OutpointHash, msg.OutpointIndex,
			msg.Classification, msg.BlockHeight,
		),
		CreatedAt: now,
		IdempotencyKey: walletUTXOIdempotencyKey(
			msg.OutpointHash, msg.OutpointIndex,
		),
		ChainTxid:          msg.OutpointHash[:],
		ChainVout:          &chainVout,
		ConfirmationHeight: &confirmationHeight,
	}

	outpoint := wire.OutPoint{
		Hash:  chainhash.Hash(msg.OutpointHash),
		Index: msg.OutpointIndex,
	}

	// The audit row, the credit leg, and any reversal this outpoint turns
	// out to owe all commit together in one lease-fenced transaction.
	return a.commit(ctx, ax, errMsg, func(ctx context.Context,
		q ledgerTx) error {

		if err := q.audit.InsertUTXOAuditEntry(ctx, audit); err != nil {
			return err
		}

		// Exit and leave proceeds book no credit leg here: the
		// ExitCostMsg or VTXOSentMsg proceeds leg already credited
		// wallet_balance for exactly this value when the sweep or the
		// round confirmed, and a second leg would count the same coins
		// twice. Recycled change is not in that position -- no earlier
		// message described it -- so it books its credit like any
		// other wallet UTXO.
		if !creditAlreadyBooked(msg.Classification) {
			err := q.ledger.InsertLedgerEntry(ctx, entry)
			if err != nil {
				return err
			}
		}

		if msg.Classification == ClassificationDeposit {
			return a.bookDepositFunding(ctx, q, msg, now)
		}

		if IsOwnWalletProceeds(msg.Classification) {
			return a.reverseIfDepositFunded(
				ctx, q, outpoint, msg.AmountSat, now,
			)
		}

		return nil
	})
}

// creditAlreadyBooked reports whether an earlier ledger message already
// credited wallet_balance for the value of a UTXO with this classification,
// so handleUTXOCreated must not credit it again.
//
// It is deliberately narrower than IsOwnWalletProceeds (ledger/actor.go).
// Every member of that family names coins the ledger will have credited by
// the time the story ends, but only some of them were credited by an earlier
// message. Recycled change has no such producer and books its own credit
// here; boarding sweep returns never reach handleUTXOCreated at all, because
// their credit rides BoardingSweepConfirmedMsg. A new classification must be
// considered against both predicates: they answer different questions.
func creditAlreadyBooked(classification string) bool {
	switch classification {
	case ClassificationExitProceeds, ClassificationLeaveProceeds:
		return true

	default:
		return false
	}
}

// bookDepositFunding records the previous outpoints a boarding deposit's
// funding transaction spent, and reverses the wallet_balance credit of every
// one that is already a recorded own-wallet proceeds UTXO.
//
// The funding-input index is written for every input, recognised or not,
// because recognition can arrive later: the proceeds message describing one of
// these coins may still be in flight. The index carries no amount and no
// accounting meaning, which is what lets it name a stranger's coin safely.
// The audit row and the reversing leg are written only for inputs this ledger
// actually credited, so every deposit_funding audit row has a real amount and
// a real leg beside it.
func (a *LedgerActor) bookDepositFunding(ctx context.Context, q ledgerTx,
	msg *UTXOCreatedMsg, now int64) error {

	deposit := wire.OutPoint{
		Hash:  chainhash.Hash(msg.OutpointHash),
		Index: msg.OutpointIndex,
	}

	for _, input := range msg.FundingInputs {
		err := q.audit.InsertDepositFundingInput(
			ctx, input, deposit, now,
		)
		if err != nil {
			return fmt.Errorf("record funding input %v: %w", input,
				err)
		}

		created, found, err := q.audit.LookupCreatedUTXO(ctx, input)
		if err != nil {
			return fmt.Errorf("look up funding input %v: %w", input,
				err)
		}
		if !found || !IsOwnWalletProceeds(created.ClassifiedAs) {
			continue
		}

		err = a.bookProceedsReversal(
			ctx, q, input, created.AmountSat,
			int32(msg.BlockHeight), now,
		)
		if err != nil {
			return err
		}

		a.log.InfoS(ctx, "Reversing recycled own-wallet proceeds "+
			"credit for boarding deposit",
			slog.String("outpoint", input.String()),
			slog.String(
				"proceeds_classification", created.ClassifiedAs,
			),
			slog.Int64("amount_sat", created.AmountSat),
		)
	}

	return nil
}

// reverseIfDepositFunded is the mirror of bookDepositFunding: an own-wallet
// proceeds row arriving after the deposit it funded finds the deposit's
// funding-input index already written and books the same reversing leg from
// this side.
//
// Whichever of the two messages commits second performs the reversal, and the
// leg must be byte-identical either way: the durable mailbox may redeliver
// either message after both have committed, and a replay whose payload differs
// from the persisted row is an idempotency conflict, not a no-op. The deposit
// side stamps the leg with the deposit's confirmation height, which is the
// height the proceeds were actually spent at, so this side reads that height
// back from the deposit's own audit row rather than using the proceeds'
// creation height. A funding transaction paying several boarding addresses
// records one deposit per output, all at the same height, so the first is as
// good as any. Neither side reads the other's in-flight state: each reads
// only what the other has already committed.
func (a *LedgerActor) reverseIfDepositFunded(ctx context.Context, q ledgerTx,
	outpoint wire.OutPoint, amountSat int64, now int64) error {

	deposits, err := q.audit.DepositsFundedByInput(ctx, outpoint)
	if err != nil {
		return fmt.Errorf("look up deposit funding for %v: %w",
			outpoint, err)
	}
	if len(deposits) == 0 {
		return nil
	}

	deposit, found, err := q.audit.LookupCreatedUTXO(ctx, deposits[0])
	if err != nil {
		return fmt.Errorf("look up deposit %v: %w", deposits[0], err)
	}
	if !found {

		// The funding index and the deposit's audit row commit in one
		// transaction, so an index entry without its deposit is a
		// broken invariant rather than a race to wait out.
		return fmt.Errorf("deposit %v funded by %v has no audit row",
			deposits[0], outpoint)
	}

	if err := a.bookProceedsReversal(
		ctx, q, outpoint, amountSat, deposit.BlockHeight, now,
	); err != nil {
		return err
	}

	a.log.InfoS(ctx, "Reversing own-wallet proceeds credit for a "+
		"boarding deposit already booked",
		slog.String("outpoint", outpoint.String()),
		slog.String("deposit", deposits[0].String()),
		slog.Int64("amount_sat", amountSat),
	)

	return nil
}

// bookProceedsReversal writes the audit row and the ledger leg that undo a
// wallet_balance credit the ledger already made for an outpoint now funding a
// boarding deposit.
//
// Debiting opening_balance mirrors that deposit's own credit, so the pair
// nets to nothing and the recycled coins are counted once. The key is
// namespaced by classification because the boarding sweep's input leg keys on
// the bare outpoint and the two can name the same coin; the dedup tuple
// already differs by accounts, so without the namespace two legs for one
// outpoint would both persist and each would look like a valid replay target
// for the other's message.
func (a *LedgerActor) bookProceedsReversal(ctx context.Context, q ledgerTx,
	outpoint wire.OutPoint, amountSat int64, blockHeight int32,
	now int64) error {

	hash := [32]byte(outpoint.Hash)
	chainVout := int32(outpoint.Index)
	confirmationHeight := blockHeight

	err := q.audit.InsertUTXOAuditEntry(ctx, UTXOAuditEntry{
		OutpointHash:  hash[:],
		OutpointIndex: chainVout,
		AmountSat:     amountSat,
		Event:         "spent",
		BlockHeight:   blockHeight,
		ClassifiedAs:  ClassificationDepositFunding,
		CreatedAt:     now,
	})
	if err != nil {
		return fmt.Errorf("audit funding spend %v: %w", outpoint, err)
	}

	return q.ledger.InsertLedgerEntry(ctx, LedgerEntry{
		DebitAccount:  AccountOpeningBalance,
		CreditAccount: AccountWalletBalance,
		AmountSat:     amountSat,
		EventType:     EventWalletUTXOSpent,
		Description: fmt.Sprintf(
			"wallet UTXO spent at %v (classification %s) at "+
				"height %d", outpoint,
			ClassificationDepositFunding, blockHeight,
		),
		CreatedAt: now,
		IdempotencyKey: classifiedUTXOIdempotencyKey(
			ClassificationDepositFunding, hash, outpoint.Index,
		),
		ChainTxid:          hash[:],
		ChainVout:          &chainVout,
		ConfirmationHeight: &confirmationHeight,
	})
}

// walletUTXOIdempotencyKey derives the outpoint-scoped dedup key
// used on the wallet UTXO deposit ledger leg. Same encoding as
// exitIdempotencyKey but kept as a distinct helper so a future
// change to one scheme (e.g. collision domain split) doesn't
// silently affect the other.
func walletUTXOIdempotencyKey(hash [32]byte, index uint32) []byte {
	return outpointIdempotencyPayload(hash, index)
}

// handleUTXOSpent records a spent wallet UTXO in the audit log.
// The classification is provided by the sending subsystem (e.g.
// round actor classifies as "round_funding").
func (a *LedgerActor) handleUTXOSpent(ctx context.Context, msg *UTXOSpentMsg,
	ax actor.Exec[ledgerTx]) fn.Result[LedgerResp] {

	const errMsg = "Failed to handle UTXO spent"

	a.log.InfoS(ctx, "Recording UTXO spent",
		slog.String(
			"outpoint", fmt.Sprintf("%x:%d", msg.OutpointHash,
				msg.OutpointIndex),
		),
		slog.Int64("amount_sat", msg.AmountSat),
		slog.Uint64("block_height",
			uint64(msg.BlockHeight)),
		slog.String("classification",
			msg.Classification),
	)

	// UTXOAuditStore is optional: in log-only mode there is nothing to
	// persist, so consume the message without opening a Commit.
	if a.cfg.UTXOAuditStore == nil {
		return fn.Ok[LedgerResp](nil)
	}

	now := a.clk.Now().Unix()
	audit := UTXOAuditEntry{
		OutpointHash:  msg.OutpointHash[:],
		OutpointIndex: int32(msg.OutpointIndex),
		AmountSat:     msg.AmountSat,
		Event:         "spent",
		BlockHeight:   int32(msg.BlockHeight),
		ClassifiedAs:  msg.Classification,
		CreatedAt:     now,
	}

	// Only the boarding-sweep-input classification books a double-entry
	// ledger leg (debit wallet_clearing, credit wallet_balance). All other
	// classifications are audit-only, and wallet_utxo_log has no positivity
	// CHECK, so the non-positive guard is scoped to the ledger-emitting
	// branch. Rejecting unconditionally would turn an audit-only spend with
	// a zero amount into a durable-mailbox poison-pill.
	var entry *LedgerEntry
	if msg.Classification == ClassificationBoardingSweepInput {
		if msg.AmountSat <= 0 {
			return a.fail(
				ctx, errMsg, fmt.Errorf("%w: UTXOSpentMsg "+
					"amount_sat must be positive (got %d)",
					ErrInvalidMessage, msg.AmountSat),
			)
		}

		chainVout := int32(msg.OutpointIndex)
		confirmationHeight := int32(msg.BlockHeight)
		entry = &LedgerEntry{
			DebitAccount:  AccountWalletClearing,
			CreditAccount: AccountWalletBalance,
			AmountSat:     msg.AmountSat,
			EventType:     EventWalletUTXOSpent,
			Description: fmt.Sprintf(
				"wallet UTXO spent at %x:%d "+
					"(classification %s) at height %d",
				msg.OutpointHash, msg.OutpointIndex,
				msg.Classification, msg.BlockHeight,
			),
			CreatedAt: now,
			IdempotencyKey: walletUTXOIdempotencyKey(
				msg.OutpointHash, msg.OutpointIndex,
			),
			ChainTxid:          msg.OutpointHash[:],
			ChainVout:          &chainVout,
			ConfirmationHeight: &confirmationHeight,
		}
	}

	return a.commit(ctx, ax, errMsg, func(ctx context.Context,
		q ledgerTx) error {

		if err := q.audit.InsertUTXOAuditEntry(ctx, audit); err != nil {
			return err
		}

		if entry == nil {
			return nil
		}

		return q.ledger.InsertLedgerEntry(ctx, *entry)
	})
}

// classifiedUTXOIdempotencyKey derives an outpoint-scoped dedup key that is
// namespaced by classification. One outpoint can be the subject of two
// different wallet-spend legs -- a boarding sweep consuming it, and a boarding
// deposit funded by it -- and those legs book different accounts. The bare
// outpoint key is reserved for the historical boarding-sweep leg, so every
// later classification takes a namespaced key rather than colliding with it.
func classifiedUTXOIdempotencyKey(classification string, hash [32]byte,
	index uint32) []byte {

	return ledgerIdempotencyKey(
		classification, legSpend,
		outpointIdempotencyPayload(hash, index),
	)
}

// handleBoardingSweepConfirmed books every leg of a confirmed boarding
// sweep inside a single Commit so the wallet_clearing account is updated
// atomically: the fee leg, one audit + clearing-debit leg per spent input,
// and the destination leg (an external transfer out, or a wallet-return
// deposit). Either the whole clearing set lands or none of it does, so a
// partial failure can never strand value in wallet_clearing.
//
// The per-leg idempotency keys match the historical single-message keys
// (sweep txid for the fee, outpoint for each input, txid vout 0 for a
// wallet return, "wallet-sweep:"+txid for an external transfer) so a replay
// — including an in-flight message straddling an upgrade — dedups cleanly.
func (a *LedgerActor) handleBoardingSweepConfirmed(ctx context.Context,
	msg *BoardingSweepConfirmedMsg,
	ax actor.Exec[ledgerTx]) fn.Result[LedgerResp] {

	const errMsg = "Failed to handle boarding sweep confirmed"

	// Validate the aggregate and per-input amounts up front so a
	// malformed message dead-letters cleanly instead of hitting a SQL
	// CHECK partway through the commit.
	if msg.ChainCostSat <= 0 {
		return a.fail(
			ctx, errMsg, fmt.Errorf("%w: "+
				"BoardingSweepConfirmedMsg chain_cost_sat "+
				"must be positive (got %d)", ErrInvalidMessage,
				msg.ChainCostSat),
		)
	}
	if msg.DestinationSat <= 0 {
		return a.fail(
			ctx, errMsg, fmt.Errorf("%w: "+
				"BoardingSweepConfirmedMsg destination_sat "+
				"must be positive (got %d)", ErrInvalidMessage,
				msg.DestinationSat),
		)
	}
	if len(msg.Inputs) == 0 {
		return a.fail(
			ctx, errMsg, fmt.Errorf("%w: "+
				"BoardingSweepConfirmedMsg must carry at "+
				"least one input", ErrInvalidMessage),
		)
	}
	for _, in := range msg.Inputs {
		if in.AmountSat <= 0 {
			return a.fail(
				ctx, errMsg, fmt.Errorf("%w: "+
					"BoardingSweepConfirmedMsg input %s "+
					"amount_sat must be positive (got %d)",
					ErrInvalidMessage, in.Outpoint,
					in.AmountSat),
			)
		}
	}

	a.log.InfoS(ctx, "Recording boarding sweep confirmed",
		slog.String("txid", fmt.Sprintf("%x", msg.Txid)),
		slog.Int64("chain_cost_sat", msg.ChainCostSat),
		slog.Int("num_inputs", len(msg.Inputs)),
		slog.Int64("destination_sat", msg.DestinationSat),
		slog.Bool("destination_external", msg.DestinationExternal),
		slog.Uint64("block_height", uint64(msg.BlockHeight)),
	)

	// Build every leg up front so the commit closure stays a thin,
	// shallow insert sequence and the whole set is booked atomically.
	legs := a.boardingSweepLegs(msg)
	now := a.clk.Now().Unix()

	return a.commit(ctx, ax, errMsg, func(ctx context.Context,
		q ledgerTx) error {

		for _, leg := range legs {
			if leg.hasAudit && q.audit != nil {
				err := q.audit.InsertUTXOAuditEntry(
					ctx, leg.audit,
				)
				if err != nil {
					return err
				}
			}

			if err := q.ledger.InsertLedgerEntry(
				ctx, leg.entry,
			); err != nil {
				return err
			}
		}

		// A wallet-return output is own-wallet proceeds like any
		// other, so if the boarding deposit that spent it was booked
		// first, this is the side that owes the reversing leg. The
		// deposit-first order is handled symmetrically by
		// bookDepositFunding, which reads the audit row written just
		// above.
		if msg.DestinationExternal || q.audit == nil {
			return nil
		}

		returnPoint := wire.OutPoint{
			Hash:  chainhash.Hash(msg.Txid),
			Index: 0,
		}

		return a.reverseIfDepositFunded(
			ctx, q, returnPoint, msg.DestinationSat, now,
		)
	})
}

// sweepLeg pairs an optional UTXO audit row with the double-entry ledger row
// that a single boarding-sweep clearing leg writes. hasAudit is false for
// the fee and external-transfer legs, which touch only the ledger.
type sweepLeg struct {
	hasAudit bool
	audit    UTXOAuditEntry
	entry    LedgerEntry
}

// boardingSweepLegs builds the ordered set of ledger (and audit) legs a
// confirmed boarding sweep books: the fee leg, one audit + clearing-debit
// leg per spent input, and the destination leg (an external transfer out,
// or a wallet-return deposit with its audit row). Building the set outside
// the commit keeps the transaction body shallow and lets the handler insert
// every leg in one Commit.
func (a *LedgerActor) boardingSweepLegs(
	msg *BoardingSweepConfirmedMsg) []sweepLeg {

	now := a.clk.Now().Unix()
	confirmationHeight := int32(msg.BlockHeight)

	legs := make([]sweepLeg, 0, len(msg.Inputs)+2)

	// Fee leg: debit onchain_fees, credit wallet_clearing.
	legs = append(legs, sweepLeg{
		entry: LedgerEntry{
			DebitAccount:   AccountOnchainFees,
			CreditAccount:  AccountWalletClearing,
			AmountSat:      msg.ChainCostSat,
			EventType:      EventBoardingSweepFeePaid,
			Description:    "boarding-sweep on-chain cost",
			CreatedAt:      now,
			IdempotencyKey: append([]byte(nil), msg.Txid[:]...),
		},
	})

	// Per-input audit row + wallet_clearing debit leg.
	for _, in := range msg.Inputs {
		hash := [32]byte(in.Outpoint.Hash)
		chainVout := int32(in.Outpoint.Index)

		audit := UTXOAuditEntry{
			OutpointHash:  in.Outpoint.Hash[:],
			OutpointIndex: int32(in.Outpoint.Index),
			AmountSat:     in.AmountSat,
			Event:         "spent",
			BlockHeight:   int32(msg.BlockHeight),
			ClassifiedAs:  ClassificationBoardingSweepInput,
			CreatedAt:     now,
		}

		entry := LedgerEntry{
			DebitAccount:  AccountWalletClearing,
			CreditAccount: AccountWalletBalance,
			AmountSat:     in.AmountSat,
			EventType:     EventWalletUTXOSpent,
			Description: fmt.Sprintf(
				"wallet UTXO spent at %x:%d "+
					"(classification %s) at height %d",
				in.Outpoint.Hash, in.Outpoint.Index,
				ClassificationBoardingSweepInput,
				msg.BlockHeight,
			),
			CreatedAt: now,
			IdempotencyKey: walletUTXOIdempotencyKey(
				hash, in.Outpoint.Index,
			),
			ChainTxid:          in.Outpoint.Hash[:],
			ChainVout:          &chainVout,
			ConfirmationHeight: &confirmationHeight,
		}

		legs = append(legs, sweepLeg{
			hasAudit: true,
			audit:    audit,
			entry:    entry,
		})
	}

	// External destination: the funds left the wallet, so settle the
	// value to transfers_out and close the clearing account.
	if msg.DestinationExternal {
		legs = append(legs, sweepLeg{
			entry: LedgerEntry{
				DebitAccount:  AccountTransfersOut,
				CreditAccount: AccountWalletClearing,
				AmountSat:     msg.DestinationSat,
				EventType:     EventWalletSweepTransfer,
				Description: fmt.Sprintf(
					"wallet sweep external transfer for "+
						"txid %x at height %d",
					msg.Txid, msg.BlockHeight,
				),
				CreatedAt: now,
				IdempotencyKey: append(
					[]byte("wallet-sweep:"), msg.Txid[:]...,
				),
				ChainTxid:          msg.Txid[:],
				ConfirmationHeight: &confirmationHeight,
			},
		})

		return legs
	}

	// Wallet-return destination: the swept value re-enters the wallet as
	// a new UTXO at vout 0. Record the audit "created" row and the
	// wallet_balance deposit that closes clearing.
	returnVout := int32(0)
	returnAudit := UTXOAuditEntry{
		OutpointHash:  msg.Txid[:],
		OutpointIndex: 0,
		AmountSat:     msg.DestinationSat,
		Event:         "created",
		BlockHeight:   int32(msg.BlockHeight),
		ClassifiedAs:  ClassificationBoardingSweepReturn,
		CreatedAt:     now,
	}

	returnEntry := LedgerEntry{
		DebitAccount:  AccountWalletBalance,
		CreditAccount: AccountWalletClearing,
		AmountSat:     msg.DestinationSat,
		EventType:     EventWalletUTXOCreated,
		Description: fmt.Sprintf(
			"wallet UTXO confirmed at %x:%d (classification %s) "+
				"at height %d",
			msg.Txid, 0, ClassificationBoardingSweepReturn,
			msg.BlockHeight,
		),
		CreatedAt: now,
		IdempotencyKey: walletUTXOIdempotencyKey(
			msg.Txid, 0,
		),
		ChainTxid:          msg.Txid[:],
		ChainVout:          &returnVout,
		ConfirmationHeight: &confirmationHeight,
	}

	legs = append(legs, sweepLeg{
		hasAudit: true,
		audit:    returnAudit,
		entry:    returnEntry,
	})

	return legs
}
