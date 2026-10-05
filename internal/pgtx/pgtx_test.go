package pgtx_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/claudioed/workforce-management/internal/pgtx"
)

// fakeTx is a distinguishable pgx.Tx stand-in; only identity matters here.
type fakeTx struct{ pgx.Tx }

func TestTxFrom_EmptyContextHasNoTransaction(t *testing.T) {
	if tx, ok := pgtx.TxFrom(context.Background()); ok || tx != nil {
		t.Fatalf("TxFrom(background) = %v, %v; want nil, false", tx, ok)
	}
}

func TestWithTx_RoundTripsTheSameTransaction(t *testing.T) {
	want := &fakeTx{}
	ctx := pgtx.WithTx(context.Background(), want)

	got, ok := pgtx.TxFrom(ctx)
	if !ok || got != pgx.Tx(want) {
		t.Fatalf("TxFrom = %v, %v; want the bound transaction", got, ok)
	}
}

func TestWithTx_ChildContextOverridesParent(t *testing.T) {
	outer, inner := &fakeTx{}, &fakeTx{}
	ctx := pgtx.WithTx(pgtx.WithTx(context.Background(), outer), inner)

	got, _ := pgtx.TxFrom(ctx)
	if got != pgx.Tx(inner) {
		t.Fatal("the innermost WithTx must win")
	}
}
