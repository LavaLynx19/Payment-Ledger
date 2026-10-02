//go:build tigerbeetle

package tbengine

import (
	"fmt"

	tb "github.com/tigerbeetle/tigerbeetle-go"

	"payment-ledger/internal/checker"
)

// Audit checks the ledger in TigerBeetle the way the Postgres checker does,
// with the invariants that apply to TigerBeetle's model (A§9.7), plus the
// harness's acknowledged-Transfer check when acks is non-nil.
func (e *Engine) Audit(acks []checker.Ack) ([]checker.Result, error) {
	balanced := checker.Result{Check: checker.Check{Invariant: 1, Name: "ledger balanced (Σ debits = Σ credits, posted and pending)"}}
	wallets := checker.Result{Check: checker.Check{Invariant: 3, Name: "wallets never overdrawn (TigerBeetle-enforced; verified)"}}
	recv := checker.Result{Check: checker.Check{Invariant: 7, Name: "receivables never overpaid"}}
	control := checker.Result{Check: checker.Check{Invariant: 8, Name: "control accounts net to zero"}}

	var dp, dpend, cp, cpend uint64
	filter := tb.QueryFilter{Ledger: ledgerID, Limit: 8189}
	for {
		accts, err := e.c.QueryAccounts(filter)
		if err != nil {
			return nil, fmt.Errorf("query accounts: %w", err)
		}
		for _, a := range accts {
			dp += uint64(amountOf(a.DebitsPosted))
			dpend += uint64(amountOf(a.DebitsPending))
			cp += uint64(amountOf(a.CreditsPosted))
			cpend += uint64(amountOf(a.CreditsPending))
			id := fromTB(a.ID)
			switch {
			case a.Code == codeWallet && available(a) < 0:
				wallets.Violations++
				wallets.Samples = appendSample(wallets.Samples, fmt.Sprintf("wallet %s available %d", id, available(a)))
			case a.Code == codeReceivable && posted(a) < 0:
				recv.Violations++
				recv.Samples = appendSample(recv.Samples, fmt.Sprintf("receivable %s owed %d", id, posted(a)))
			case a.Code == codeControl && (amountOf(a.DebitsPosted) != amountOf(a.CreditsPosted) || amountOf(a.DebitsPending) != 0):
				control.Violations++
				control.Samples = appendSample(control.Samples, fmt.Sprintf("control %s debits %d credits %d", id, amountOf(a.DebitsPosted), amountOf(a.CreditsPosted)))
			}
		}
		if len(accts) < int(filter.Limit) {
			break
		}
		filter.TimestampMin = accts[len(accts)-1].Timestamp + 1
	}
	if dp != cp || dpend != cpend {
		balanced.Violations++
		balanced.Samples = append(balanced.Samples, fmt.Sprintf("posted %d/%d pending %d/%d", dp, cp, dpend, cpend))
	}
	results := []checker.Result{balanced, wallets, recv, control}
	if acks == nil {
		return results, nil
	}

	ackResult := checker.Result{Check: checker.AckCheck}
	for start := 0; start < len(acks); start += 8189 {
		batch := acks[start:min(start+8189, len(acks))]
		ids := make([]tb.Uint128, len(batch))
		for i, a := range batch {
			ids[i] = toTB(a.TransferID)
		}
		found, err := e.c.LookupTransfers(ids)
		if err != nil {
			return nil, fmt.Errorf("lookup acks: %w", err)
		}
		exists := make(map[[16]byte]bool, len(found))
		for _, t := range found {
			exists[t.ID.Bytes()] = true
		}
		for _, a := range batch {
			status := ""
			if exists[[16]byte(a.TransferID)] {
				status = "posted" // single-phase: a Transfer exists only once posted
			}
			checker.JudgeAck(&ackResult, a, status, keyID(a.Key))
		}
	}
	return append(results, ackResult), nil
}

func appendSample(s []string, v string) []string {
	if len(s) < 5 {
		return append(s, v)
	}
	return s
}
