package checker

import (
	"testing"

	"github.com/google/uuid"

	"payment-ledger/internal/shard"
)

// A saga Transfer whose credit is still in the outbox nets its amount, and
// so does the ledger. That's in flight, not a violation (A§9.5). The same
// imbalance without an outbox row is one.
func TestInFlightSagaCreditIsNotAViolation(t *testing.T) {
	src, _ := shard.NewID(0)
	dst, _ := shard.NewID(1)
	tr, _ := shard.NewID(0)
	captured := int64(30)
	build := func(inFlight bool) *view {
		v := &view{
			n:         2,
			accounts:  []map[uuid.UUID]bool{{src: true}, {dst: true}},
			transfers: []map[uuid.UUID]transfer{{tr: {status: "posted", typ: "p2p", dest: dst, amount: 30}}, {}},
			holds:     map[uuid.UUID]hold{tr: {dest: dst, captured: &captured}},
			nets:      map[uuid.UUID]netSum{tr: {net: 30, entries: 1}}, // debit landed, credit not yet
			keys:      map[string]uuid.UUID{},
			inFlight:  map[uuid.UUID]int64{},
		}
		if inFlight {
			v.inFlight[tr] = 30
		}
		return v
	}
	violations := func(rs []Result) map[string]int {
		m := map[string]int{}
		for _, r := range rs {
			m[r.Name] = r.Violations
		}
		return m
	}

	got := violations(build(true).evaluate())
	for name, n := range got {
		if n != 0 {
			t.Errorf("with the credit in the outbox, %q reported %d violations", name, n)
		}
	}
	got = violations(build(false).evaluate())
	if got["each transfer balanced"] != 1 || got["ledger balanced (Σ debits = Σ credits)"] != 1 {
		t.Errorf("without an outbox row the imbalance must be flagged: %v", got)
	}
}
