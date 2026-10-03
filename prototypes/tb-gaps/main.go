// PROTOTYPE, wipe me. Throwaway code that answers two Rung 4 questions
// against a real TigerBeetle (an HTML simulation would assume the answers):
//
//  1. Can "an open Receivable blocks the debtor's debits" be enforced
//     atomically? Design: link the real debit with a balancing_credit from a
//     zero-balance control account D into the receivable R. That moves
//     exactly what's owed, D can't go negative, so the chain fails iff owed > 0.
//  2. Can a Reversal's shortfall be computed atomically? Design: link
//     C→A X, then B→C balancing_debit X (moves r = what B can cover), then
//     R→C balancing_credit X (capped so C nets to zero, moves X - r).
package main

import (
	"fmt"
	"log"
	"os"

	tb "github.com/tigerbeetle/tigerbeetle-go"
)

const ledger = 1

var (
	client   tb.Client
	failures int
)

func main() {
	var err error
	client, err = tb.NewClient(tb.ToUint128(0), []string{envOr("TB_ADDRESS", "3033")})
	if err != nil {
		log.Fatal(err)
	}
	defer client.Close()

	receivableBlock()
	reversalShortfall()

	if failures > 0 {
		fmt.Printf("\nVERDICT: %d expectation(s) failed\n", failures)
		os.Exit(1)
	}
	fmt.Println("\nVERDICT: both designs work atomically in TigerBeetle")
}

// ---- prototype 1: receivable blocks debits ----

func receivableBlock() {
	fmt.Println("=== 1. Open Receivable blocks the debtor's debits (atomic linked chain)")
	f := account("funding", tb.AccountFlags{})
	w := account("wallet W", tb.AccountFlags{DebitsMustNotExceedCredits: true})
	x := account("payee X", tb.AccountFlags{DebitsMustNotExceedCredits: true})
	r := account("receivable R", tb.AccountFlags{CreditsMustNotExceedDebits: true}) // owed = debits - credits
	d := account("control D", tb.AccountFlags{DebitsMustNotExceedCredits: true})    // never credited

	submit("fund W 100", plain(f, w, 100))
	guarded := func(amount uint64) []tb.Transfer {
		return []tb.Transfer{
			transfer(w, x, tb.ToUint128(amount), tb.TransferFlags{Linked: true}),
			transfer(d, r, tb.AmountMax, tb.TransferFlags{BalancingCredit: true}),
		}
	}

	expect(ok(submit("a) guarded debit 30 while owing 0", guarded(30))), "guarded debit succeeds when nothing is owed")
	submit("open receivable: R owes 40", plain(r, x, 40))
	before := lookup(w)
	expect(!ok(submit("b) guarded debit 10 while owing 40", guarded(10))), "guarded debit is rejected while owing")
	expect(lookup(w).DebitsPosted == before.DebitsPosted, "rejected chain left W untouched (atomic)")
	expect(ok(submit("c) repayment 25 (unguarded)", plain(w, r, 25))), "repayment allowed while owing")
	expect(!ok(submit("   overpay 20 (owes 15)", plain(w, r, 20))), "overpay rejected by credits_must_not_exceed_debits")
	expect(ok(submit("d) repayment 15", plain(w, r, 15))), "final repayment clears the debt")
	expect(ok(submit("   guarded debit 10 after paying off", guarded(10))), "guarded debit allowed again once owed is 0")
	expect(lookup(d).DebitsPosted == tb.ToUint128(0), "control D never moved")
	show(w, x, r, d)
}

// ---- prototype 2: reversal shortfall ----

func reversalShortfall() {
	fmt.Println("\n=== 2. Reversal of X = 100: B returns r = min(X, available), shortfall X - r to R")
	for _, available := range []uint64{100, 20, 0} {
		fmt.Printf("\n--- B has %d available\n", available)
		f := account("funding", tb.AccountFlags{})
		a := account("payer A", tb.AccountFlags{DebitsMustNotExceedCredits: true})
		b := account("recipient B", tb.AccountFlags{DebitsMustNotExceedCredits: true})
		r := account("receivable R", tb.AccountFlags{CreditsMustNotExceedDebits: true})
		c := account("control C", tb.AccountFlags{})
		if available > 0 {
			submit("fund B", plain(f, b, available))
		}

		const x = 100
		chain := []tb.Transfer{
			transfer(c, a, tb.ToUint128(x), tb.TransferFlags{Linked: true}),                       // A made whole
			transfer(b, c, tb.ToUint128(x), tb.TransferFlags{Linked: true, BalancingDebit: true}), // r from B
			transfer(r, c, tb.ToUint128(x), tb.TransferFlags{BalancingCredit: true}),              // X - r owed
		}
		expect(ok(submit("reversal chain", chain)), "reversal chain commits atomically")

		r0 := min(uint64(x), available)
		expect(net(lookup(a), false) == x, fmt.Sprintf("A credited %d", x))
		expect(net(lookup(b), false) == int64(available-r0), fmt.Sprintf("B returned r = %d", r0))
		expect(net(lookup(r), true) == int64(x-r0), fmt.Sprintf("R owes shortfall %d", x-r0))
		expect(net(lookup(c), false) == 0, "control C nets to zero")
		show(a, b, r, c)
	}
}

// ---- helpers ----

type acct struct {
	name string
	id   tb.Uint128
}

func account(name string, flags tb.AccountFlags) acct {
	a := acct{name, tb.ID()}
	res, err := client.CreateAccounts([]tb.Account{{ID: a.id, Ledger: ledger, Code: 1, Flags: flags.ToUint16()}})
	if err != nil {
		log.Fatal(err)
	}
	for _, r := range res {
		if r.Status != tb.AccountCreated {
			log.Fatalf("create %s: %s", name, r.Status)
		}
	}
	return a
}

func transfer(from, to acct, amount tb.Uint128, flags tb.TransferFlags) tb.Transfer {
	return tb.Transfer{ID: tb.ID(), DebitAccountID: from.id, CreditAccountID: to.id, Amount: amount, Ledger: ledger, Code: 1, Flags: flags.ToUint16()}
}

func plain(from, to acct, amount uint64) []tb.Transfer {
	return []tb.Transfer{transfer(from, to, tb.ToUint128(amount), tb.TransferFlags{})}
}

// submit sends a batch and prints each transfer's result.
func submit(label string, ts []tb.Transfer) []tb.CreateTransferStatus {
	res, err := client.CreateTransfers(ts)
	if err != nil {
		log.Fatal(err)
	}
	statuses := make([]tb.CreateTransferStatus, len(ts))
	for i := range statuses {
		statuses[i] = tb.TransferCreated
	}
	for i, r := range res {
		statuses[i] = r.Status
	}
	fmt.Printf("  %-40s → %v\n", label, statuses)
	return statuses
}

func ok(statuses []tb.CreateTransferStatus) bool {
	for _, s := range statuses {
		if s != tb.TransferCreated {
			return false
		}
	}
	return true
}

func lookup(a acct) tb.Account {
	res, err := client.LookupAccounts([]tb.Uint128{a.id})
	if err != nil || len(res) != 1 {
		log.Fatalf("lookup %s: %v", a.name, err)
	}
	return res[0]
}

// net is credits - debits (posted), or debits - credits for a debit-normal account.
func net(acc tb.Account, debitNormal bool) int64 {
	cr, _ := acc.CreditsPosted.Uint64()
	dr, _ := acc.DebitsPosted.Uint64()
	if debitNormal {
		return int64(dr) - int64(cr)
	}
	return int64(cr) - int64(dr)
}

func show(as ...acct) {
	for _, a := range as {
		acc := lookup(a)
		fmt.Printf("    %-14s debits=%s credits=%s\n", a.name, acc.DebitsPosted, acc.CreditsPosted)
	}
}

func expect(cond bool, what string) {
	mark := "PASS"
	if !cond {
		mark = "FAIL"
		failures++
	}
	fmt.Printf("  [%s] %s\n", mark, what)
}

func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}
