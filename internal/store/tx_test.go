package store

import (
	"context"
	"errors"
	"expvar"
	"testing"

	"github.com/google/uuid"
)

func TestRetryOnConflict(t *testing.T) {
	ctx := context.Background()
	boom := errors.New("boom")

	cases := []struct {
		name      string
		results   []error // returned by successive attempts
		wantErr   error
		wantCalls int
	}{
		{"succeeds first try", []error{nil}, nil, 1},
		{"succeeds after conflicts", []error{ErrVersionConflict, ErrVersionConflict, nil}, nil, 3},
		{"exhausted", []error{ErrVersionConflict, ErrVersionConflict, ErrVersionConflict}, ErrRetriesExhausted, 3},
		{"other error returns immediately", []error{boom}, boom, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			calls := 0
			err := retryOnConflict(ctx, 3, nil, "test", func() error {
				err := c.results[calls]
				calls++
				return err
			})
			if !errors.Is(err, c.wantErr) {
				t.Errorf("err = %v, want %v", err, c.wantErr)
			}
			if calls != c.wantCalls {
				t.Errorf("calls = %d, want %d", calls, c.wantCalls)
			}
		})
	}
}

func TestRetryOnConflictCounts(t *testing.T) {
	stats := new(expvar.Map).Init()
	results := []error{ErrVersionConflict, nil, ErrVersionConflict, ErrVersionConflict}
	calls := 0
	try := func() error { err := results[calls]; calls++; return err }

	_ = retryOnConflict(context.Background(), 2, stats, "accept", try) // conflict, then success
	_ = retryOnConflict(context.Background(), 2, stats, "accept", try) // two conflicts, exhausted

	for key, want := range map[string]int64{"accept.attempts": 4, "accept.conflicts": 3, "accept.exhausted": 1} {
		got := int64(0)
		if v, ok := stats.Get(key).(*expvar.Int); ok {
			got = v.Value()
		}
		if got != want {
			t.Errorf("%s = %d, want %d", key, got, want)
		}
	}
}

func TestAscending(t *testing.T) {
	a := uuid.MustParse("00000000-0000-7000-8000-000000000001")
	b := uuid.MustParse("00000000-0000-7000-8000-000000000002")
	c := uuid.MustParse("ffffffff-0000-7000-8000-000000000000")

	in := []uuid.UUID{c, a, b}
	got := Ascending(in...)
	if got[0] != a || got[1] != b || got[2] != c {
		t.Errorf("Ascending = %v", got)
	}
	if in[0] != c {
		t.Error("Ascending mutated its input")
	}
}
