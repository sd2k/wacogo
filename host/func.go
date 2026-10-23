package host

import (
	"context"

	"github.com/partite-ai/wacogo/internal/core"
)

// Func is the body of a host-component function. cc carries the
// per-call context (caller memory, resource-table operations); h is
// the bound ComponentInstance, providing access to UserState and
// resource registration. stack carries the canonical-ABI flat
// arguments on entry and receives the flat results on return.
//
// Returning a non-nil error traps the calling wasm component with the
// error message. A Func is invoked serially.
type Func func(ctx context.Context, cc *CallContext, h *ComponentInstance, stack []uint64) error

// GoFunc is the body of a host-component function declared with
// AddGoFunction: it receives the lifted arguments and returns the
// results, with no canonical-ABI handling of its own. Returning an error
// traps the caller; for an async call, an error wrapping
// context.Canceled after ctx is cancelled reports the call as cancelled.
type GoFunc func(ctx context.Context, args []core.Val) ([]core.Val, error)
