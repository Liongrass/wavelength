package tapassets

import (
	"context"
	"errors"
)

// ErrStoreNotFound reports that a journal key has no durable value.
var ErrStoreNotFound = errors.New("asset transition state not found")

// Store persists in-flight attempts and completed transition packages. Store
// must durably replace each value atomically before returning success; Load
// returns ErrStoreNotFound for absent keys. Callers serialize access to each
// workflow; this interface does not provide cross-process coordination.
type Store interface {
	Load(context.Context, string) ([]byte, error)

	Store(context.Context, string, []byte) error
}
