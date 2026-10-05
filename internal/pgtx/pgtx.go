// Package pgtx holds the ONE mechanism this service uses to bind a pgx
// transaction into a context.Context: an unexported key type plus
// WithTx/TxFrom. It exists as its own small package — sibling to, not
// nested inside, internal/adapters/inbound or internal/adapters/outbound
// — so that BOTH sides of a genuinely cross-cutting concern can bind and
// read the SAME transaction-in-context without either one importing the
// other's adapter package (which internal/architecture's fitness tests
// forbid in both directions: inbound must never depend on outbound, and
// outbound must never depend on inbound).
//
// internal/adapters/outbound/postgres (UnitOfWork.Execute, querierFrom,
// beginOrJoin — see that package's unit_of_work.go) is the original owner
// of this mechanism and continues to use it for every repo/publisher
// adapter's own transaction handling.
//
// internal/adapters/inbound/http's idempotency middleware
// (RequireIdempotencyKey, ADR-0027) is the second caller: it begins its OWN
// pgx transaction directly against the pgxpool.Pool it is given, binds it
// into the request's context via pgtx.WithTx, and calls the wrapped handler
// with that context. When the handler's use case later calls
// ports.UnitOfWork.Execute (backed by postgres.UnitOfWork), that Execute
// resolves the SAME transaction via pgtx.TxFrom and JOINS it instead of
// opening a second, invisible-to-each-other one. This is what makes the
// whole request cycle (idempotency bookkeeping, the aggregate Save, the
// outbox insert) commit or roll back together as one atomic unit.
package pgtx

import (
	"context"

	"github.com/jackc/pgx/v5"
)

type txKey struct{}

// WithTx returns a child context carrying tx. Any code that resolves a
// transaction from ctx via TxFrom (directly, or indirectly through the
// postgres package's querierFrom/txFrom/beginOrJoin/UnitOfWork.Execute)
// sees exactly this tx.
func WithTx(ctx context.Context, tx pgx.Tx) context.Context {
	return context.WithValue(ctx, txKey{}, tx)
}

// TxFrom reports the transaction bound to ctx, if any.
func TxFrom(ctx context.Context) (pgx.Tx, bool) {
	tx, ok := ctx.Value(txKey{}).(pgx.Tx)
	return tx, ok
}
