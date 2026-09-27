package ledger

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"payment-ledger/internal/store"
)

func dbNow(t *testing.T, l *Ledger) time.Time {
	t.Helper()
	now, err := store.Now(context.Background(), l.db)
	if err != nil {
		t.Fatal(err)
	}
	return now
}

func TestGetBalanceAt(t *testing.T) {
	l := testLedger(t)
	ctx := context.Background()
	ws := wallets(t, l, 2)

	beforeAny := dbNow(t, l)
	fund(t, l, ws[0], 100)
	afterFund := dbNow(t, l)
	if _, err := l.CreateTransfer(ctx, AcceptRequest{Key: key(), Hash: []byte("h"), SourceID: ws[0], DestID: ws[1], Amount: 30}); err != nil {
		t.Fatal(err)
	}
	drain(t, l)
	afterSpend := dbNow(t, l)

	for name, c := range map[string]struct {
		at   time.Time
		want int64
	}{
		"before any entry": {beforeAny, 0},
		"after top-up":     {afterFund, 100},
		"after transfer":   {afterSpend, 70},
	} {
		got, err := l.GetBalanceAt(ctx, ws[0], c.at)
		if err != nil {
			t.Fatal(err)
		}
		if got != c.want {
			t.Errorf("%s: balance = %d, want %d", name, got, c.want)
		}
	}
}

func TestListEntriesPages(t *testing.T) {
	l := testLedger(t)
	ctx := context.Background()
	w := wallets(t, l, 1)[0]
	for range 5 {
		fund(t, l, w, 10) // 5 credit Entries
	}

	var versions []int64
	var after int64
	for pages := 0; ; pages++ {
		if pages > 5 {
			t.Fatal("pagination never ended")
		}
		page, more, err := l.ListEntries(ctx, w, nil, nil, after, 2)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range page {
			versions = append(versions, e.AccountVersion)
		}
		if !more {
			break
		}
		after = page[len(page)-1].AccountVersion
	}
	if len(versions) != 5 {
		t.Fatalf("got %d entries across pages, want 5", len(versions))
	}
	for i := 1; i < len(versions); i++ {
		if versions[i] <= versions[i-1] {
			t.Errorf("versions not increasing: %v", versions)
		}
	}

	future := dbNow(t, l).Add(time.Hour)
	if page, _, err := l.ListEntries(ctx, w, &future, nil, 0, 10); err != nil || len(page) != 0 {
		t.Errorf("from=future returned %d entries, err %v; want 0", len(page), err)
	}
}

func TestListReceivables(t *testing.T) {
	l := testLedger(t)
	ctx := context.Background()
	_, debtor, id := paid(t, l, 100)
	spend(t, l, debtor, 100)
	reverse(t, l, id) // debtor owes 100

	find := func(minAge time.Duration) *store.Receivable {
		after := uuid.Nil
		for {
			page, more, err := l.ListReceivables(ctx, minAge, after, 50)
			if err != nil {
				t.Fatal(err)
			}
			for i := range page {
				if page[i].DebtorWalletID == debtor {
					return &page[i]
				}
			}
			if !more {
				return nil
			}
			after = page[len(page)-1].DebtorWalletID
		}
	}

	// Listed before capture: the shortfall counts from Accept.
	r := find(0)
	if r == nil || r.Owed != 100 {
		t.Fatalf("receivable = %+v, want debtor owing 100", r)
	}
	if find(time.Hour) != nil {
		t.Error("min_age=1h listed a receivable opened just now")
	}
	drain(t, l)
	if r := find(0); r == nil || r.Owed != 100 {
		t.Errorf("after capture receivable = %+v, want owed 100", r)
	}
}
