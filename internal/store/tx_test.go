package store

import (
	"context"
	"errors"
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
			err := retryOnConflict(ctx, 3, func() error {
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
