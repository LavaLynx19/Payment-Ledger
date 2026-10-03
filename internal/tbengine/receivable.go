//go:build tigerbeetle

package tbengine

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	tb "github.com/tigerbeetle/tigerbeetle-go"

	"payment-ledger/internal/ledger"
	"payment-ledger/internal/store"
)

// owed is what wallet still owes on its receivable account.
func (e *Engine) owed(wallet uuid.UUID) (int64, error) {
	r := receivableID(wallet)
	accts, err := e.lookupAccounts(r)
	if err != nil {
		return 0, err
	}
	return posted(accts[r]), nil
}

// Repay credits the debtor's receivable. It skips the receivable guard, and
// TigerBeetle itself refuses an overpayment (credits_must_not_exceed_debits).
func (e *Engine) Repay(_ context.Context, key string, _ []byte, wallet uuid.UUID, amount int64) (store.Transfer, error) {
	recv := receivableID(wallet)
	if _, _, err := e.prepare(ledger.TypeRepayment, wallet, recv, amount); err != nil {
		return store.Transfer{}, err
	}
	id := keyID(key)
	st, err := e.submit(tb.Transfer{
		ID: toTB(id), DebitAccountID: toTB(wallet), CreditAccountID: toTB(recv),
		Amount: u128(amount), Ledger: ledgerID, Code: typeCodes[ledger.TypeRepayment],
	})
	if err != nil {
		return store.Transfer{}, err
	}
	if st[0] == tb.TransferExceedsDebits {
		owed, err := e.owed(wallet)
		if err != nil {
			return store.Transfer{}, err
		}
		if owed == 0 {
			return store.Transfer{}, invalid("wallet_id has no receivable to repay.")
		}
		return store.Transfer{}, invalid(fmt.Sprintf("amount must be at most %d, the amount still owed.", owed))
	}
	if err := e.outcome(st[0], wallet, amount); err != nil {
		return store.Transfer{}, err
	}
	return e.GetTransfer(context.Background(), id)
}

// Reversal chain ids derive from the reversed Transfer, so TigerBeetle itself
// allows at most one reversal (A§9.7).
func revPayID(orig uuid.UUID) uuid.UUID  { return derive("rev-pay", orig.String()) }
func revBackID(orig uuid.UUID) uuid.UUID { return derive("rev-back", orig.String()) }
func revRecvID(orig uuid.UUID) uuid.UUID { return derive("rev-recv", orig.String()) }

// ReverseTransfer sends a posted P2P Transfer's money back in one linked
// chain through control C (prototypes/tb-gaps): C→payer X; recipient→C
// balancing_debit X (moves r, what the recipient can cover); receivable→C
// balancing_credit X (moves X - r, so C nets to zero). A second reversal, with
// any key, replays the first (A§9.7 parity limit).
func (e *Engine) ReverseTransfer(ctx context.Context, _ string, _ []byte, transferID uuid.UUID) (*store.Transfer, *store.Transfer, error) {
	orig, err := e.GetTransfer(ctx, transferID)
	if err != nil {
		return nil, nil, err
	}
	switch {
	case orig.Type != ledger.TypeP2P:
		return nil, nil, &ledger.NotReversibleError{Msg: "Only P2P transfers can be reversed."}
	case orig.Status != "posted":
		return nil, nil, &ledger.NotReversibleError{Msg: "Only posted transfers can be reversed; this one is " + orig.Status + "."}
	}
	x := orig.Amount
	if _, h, err := e.holdState(ctx, transferID); err == nil && h.CapturedAmount != nil {
		x = *h.CapturedAmount // a captured Hold reverses what was posted
	}
	payer, recipient := orig.SourceID, orig.DestID
	ud := toTB(orig.ID) // reverses_id
	st, err := e.submit(
		tb.Transfer{ID: toTB(revPayID(orig.ID)), DebitAccountID: toTB(e.revC), CreditAccountID: toTB(payer),
			Amount: u128(x), Ledger: ledgerID, Code: typeCodes[ledger.TypeReversal], UserData128: ud,
			Flags: tb.TransferFlags{Linked: true}.ToUint16()},
		tb.Transfer{ID: toTB(revBackID(orig.ID)), DebitAccountID: toTB(recipient), CreditAccountID: toTB(e.revC),
			Amount: u128(x), Ledger: ledgerID, Code: typeCodes[ledger.TypeReversal], UserData128: ud,
			Flags: tb.TransferFlags{Linked: true, BalancingDebit: true}.ToUint16()},
		tb.Transfer{ID: toTB(revRecvID(orig.ID)), DebitAccountID: toTB(receivableID(recipient)), CreditAccountID: toTB(e.revC),
			Amount: u128(x), Ledger: ledgerID, Code: typeCodes[ledger.TypeReceivable], UserData128: ud,
			Flags: tb.TransferFlags{BalancingCredit: true}.ToUint16()},
	)
	if err != nil {
		return nil, nil, err
	}
	if st[0] != tb.TransferCreated && st[0] != tb.TransferExists {
		return nil, nil, fmt.Errorf("tigerbeetle reversal chain: %v", st)
	}
	return e.reversalsOf(orig)
}

// reversalsOf reports orig's reversal chain the way the Postgres ledger does:
// a reversal recipient → payer for r and a receivable → payer for X - r,
// either nil when its amount is 0.
func (e *Engine) reversalsOf(orig store.Transfer) (*store.Transfer, *store.Transfer, error) {
	ts, err := e.c.LookupTransfers([]tb.Uint128{toTB(revBackID(orig.ID)), toTB(revRecvID(orig.ID))})
	if err != nil {
		return nil, nil, fmt.Errorf("lookup reversal: %w", err)
	}
	var reversal, receivable *store.Transfer
	for _, t := range ts {
		if amountOf(t.Amount) == 0 {
			continue
		}
		v, err := e.view(t)
		if err != nil {
			return nil, nil, err
		}
		if fromTB(t.ID) == revBackID(orig.ID) {
			reversal = &v
		} else {
			receivable = &v
		}
	}
	return reversal, receivable, nil
}

// view converts a TigerBeetle transfer. Reversal legs route through control
// C, so they're shown as Postgres records them: paid to the original payer,
// with reverses_id from user_data_128.
func (e *Engine) view(t tb.Transfer) (store.Transfer, error) {
	v := transferView(t)
	if fromTB(t.CreditAccountID) != e.revC {
		return v, nil
	}
	orig := fromTB(t.UserData128)
	v.ReversesID = &orig
	ts, err := e.c.LookupTransfers([]tb.Uint128{t.UserData128})
	if err != nil {
		return store.Transfer{}, fmt.Errorf("lookup reversed transfer: %w", err)
	}
	if len(ts) == 1 {
		v.DestID = fromTB(ts[0].DebitAccountID) // the original payer
	}
	return v, nil
}

// ListReceivables pages owing debtors by id. TigerBeetle can't filter on
// balances, so it scans receivable accounts and keeps those owing; opened_at
// is when the current run of non-zero balance began.
func (e *Engine) ListReceivables(_ context.Context, minAge time.Duration, afterDebtor uuid.UUID, limit int) ([]store.Receivable, bool, error) {
	if limit <= 0 {
		limit = 100
	}
	limit = min(limit, ledger.MaxPage)
	var out []store.Receivable
	filter := tb.QueryFilter{Ledger: ledgerID, Code: codeReceivable, Limit: 8189}
	for {
		accts, err := e.c.QueryAccounts(filter)
		if err != nil {
			return nil, false, fmt.Errorf("query receivables: %w", err)
		}
		for _, a := range accts {
			debtor := fromTB(a.UserData128)
			owed := posted(a)
			if owed <= 0 || bytes.Compare(debtor[:], afterDebtor[:]) <= 0 {
				continue
			}
			opened, err := e.openedAt(a)
			if err != nil {
				return nil, false, err
			}
			if minAge > 0 && time.Since(opened) < minAge {
				continue
			}
			out = append(out, store.Receivable{
				DebtorWalletID: debtor, ReceivableAccountID: fromTB(a.ID), Owed: owed, OpenedAt: opened,
			})
		}
		if len(accts) < int(filter.Limit) {
			break
		}
		filter.TimestampMin = accts[len(accts)-1].Timestamp + 1
	}
	slices.SortFunc(out, func(a, b store.Receivable) int { return bytes.Compare(a.DebtorWalletID[:], b.DebtorWalletID[:]) })
	if len(out) > limit {
		return out[:limit], true, nil
	}
	return out, false, nil
}

// openedAt walks the receivable's balance history back to where the current
// run of non-zero balance began.
func (e *Engine) openedAt(a tb.Account) (time.Time, error) {
	bs, err := e.c.GetAccountBalances(tb.AccountFilter{
		AccountID: a.ID, Limit: 8189,
		Flags: tb.AccountFilterFlags{Debits: true, Credits: true, Reversed: true}.ToUint32(),
	})
	if err != nil {
		return time.Time{}, fmt.Errorf("receivable history: %w", err)
	}
	opened := at(a.Timestamp)
	for _, b := range bs {
		if amountOf(b.DebitsPosted)-amountOf(b.CreditsPosted) <= 0 {
			break
		}
		opened = at(b.Timestamp)
	}
	return opened, nil
}
