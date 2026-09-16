package waved

import (
	"context"
	"log/slog"

	btcaddr "github.com/btcsuite/btcd/address/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/db"
	"github.com/lightninglabs/wavelength/wallet"
)

// ownedWalletScriptRegistrar records a backing-wallet script the daemon just
// minted. It is the write half of the ownership registry; the read half is
// wallet.OwnedWalletScriptChecker.
//
// The interface exists so the per-backend wallet adapters can register a
// script without depending on the db package, and so a test adapter can be
// constructed with no registry at all.
type ownedWalletScriptRegistrar interface {
	// RecordOwnedWalletScript registers a minted pkScript under a source
	// naming the mint site.
	RecordOwnedWalletScript(ctx context.Context, pkScript []byte,
		source string) error
}

// ownedWalletScriptRegistry is both halves at once, which is what Server
// hands out: the same store answers the mint side and the lookup side.
type ownedWalletScriptRegistry interface {
	ownedWalletScriptRegistrar
	wallet.OwnedWalletScriptChecker
}

// ownedWalletScriptStore adapts the db-backed registry to both halves of the
// contract. It is the only implementation in production.
type ownedWalletScriptStore struct {
	store *db.OwnedWalletScriptStore
	log   btclog.Logger
}

var _ ownedWalletScriptRegistry = (*ownedWalletScriptStore)(nil)

// ownedWalletScripts returns the daemon's owned-script registry, or a nil
// interface when the database is not open yet.
//
// The nil is a real nil rather than an interface wrapping a nil pointer, so
// every caller's plain nil check works and no method ever runs on an absent
// store. A nil registry means "not known to be ours" for every script, which
// is the conservative accounting answer and the behaviour that predates the
// registry, so callers need no special case beyond that one check.
func (s *Server) ownedWalletScripts() ownedWalletScriptRegistry {
	if s.db == nil {
		return nil
	}

	dbStore := db.NewStore(
		s.db.DB, s.db.Queries, s.db.Backend(),
		s.subLogger(db.Subsystem),
	)

	return &ownedWalletScriptStore{
		store: db.NewOwnedWalletScriptStore(dbStore),
		log:   s.subLogger(db.Subsystem),
	}
}

// RecordOwnedWalletScript registers a minted pkScript in the durable registry.
func (o *ownedWalletScriptStore) RecordOwnedWalletScript(ctx context.Context,
	pkScript []byte, source string) error {

	return o.store.RecordOwnedWalletScript(ctx, pkScript, source)
}

// IsOwnedWalletScript reports whether a pkScript is in the registry.
func (o *ownedWalletScriptStore) IsOwnedWalletScript(ctx context.Context,
	pkScript []byte) (bool, error) {

	return o.store.IsOwnedWalletScript(ctx, pkScript)
}

// recordOwnedWalletScript registers a freshly minted destination script,
// swallowing registry failures.
//
// Minting a script is a funds-path operation -- a sweep or change output is
// about to pay it -- while the registry entry only decides how the ledger
// later classifies that payment. Failing the mint to protect an accounting
// flag would trade a real loss for a bookkeeping one. A lost entry degrades
// exactly one way: the destination reads as foreign, and the value is booked
// as having left rather than as an internal transfer.
func recordOwnedWalletScript(ctx context.Context,
	registrar ownedWalletScriptRegistrar, pkScript []byte) {

	recordMintedScript(
		ctx, registrar, pkScript, db.OwnedWalletScriptSourceSweep,
		"Failed to record minted wallet script; payments to it "+
			"will book as outflows",
	)
}

// recordOwnedWalletAddress registers a freshly minted receive address by its
// pkScript. NewWalletAddress hands back an address string rather than a
// script, so the conversion happens here.
func (s *Server) recordOwnedWalletAddress(ctx context.Context,
	addr btcaddr.Address) {

	registrar := s.ownedWalletScripts()
	if registrar == nil {
		return
	}

	const failMsg = "Failed to record minted wallet address; payments " +
		"to it will book as outflows"

	pkScript, err := txscript.PayToAddrScript(addr)
	if err != nil {
		logRegistryMiss(ctx, registrar, failMsg, err)

		return
	}

	recordMintedScript(
		ctx, registrar, pkScript, db.OwnedWalletScriptSourceReceive,
		failMsg,
	)
}

// recordMintedScript is the shared body of the two mint sites: it writes the
// registry entry on a context detached from whatever cancelled the mint, and
// reports a failure rather than propagating it.
func recordMintedScript(ctx context.Context,
	registrar ownedWalletScriptRegistrar, pkScript []byte, source,
	failMsg string) {

	if registrar == nil || len(pkScript) == 0 {
		return
	}

	// The mint's own context may already be unwinding by the time the
	// script is handed back, and the registry write is independent of
	// whatever cancelled it.
	recordCtx := context.WithoutCancel(ctx)

	err := registrar.RecordOwnedWalletScript(recordCtx, pkScript, source)
	if err != nil {
		logRegistryMiss(recordCtx, registrar, failMsg, err)
	}
}

// logRegistryMiss reports a lost registry write through the registrar's own
// logger when it has one. A test registrar without a logger stays silent.
func logRegistryMiss(ctx context.Context, registrar ownedWalletScriptRegistrar,
	msg string, err error) {

	store, ok := registrar.(*ownedWalletScriptStore)
	if !ok || store.log == nil {
		return
	}

	store.log.InfoS(ctx, msg, slog.String("error", err.Error()))
}
