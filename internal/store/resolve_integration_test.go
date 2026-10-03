package store

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

type crash struct{ point string }

// crashAt returns a fail hook that panics at point, like a coordinator dying.
func crashAt(point string) func(string) {
	return func(name string) {
		if name == point {
			panic(crash{point})
		}
	}
}

func pair(t *testing.T, s *Shards) (uuid.UUID, uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	a, err := CreateWalletsOn(ctx, s.Pool(0), 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	b, err := CreateWalletsOn(ctx, s.Pool(1), 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	return a[0], b[0]
}

// A coordinator crash at each 2PC step is finished by the resolver: rolled
// back before the decision, committed after it, with nothing left over.
func TestResolverFinishesCrashedCoordinators(t *testing.T) {
	s, _ := twoShards(t)
	ctx := context.Background()
	for _, c := range []struct {
		point     string
		committed bool
	}{
		{"twopc.after_first_prepare", false},
		{"twopc.after_all_prepared", false},
		{"twopc.after_decision", true},
		{"twopc.after_first_commit", true},
	} {
		t.Run(c.point, func(t *testing.T) {
			a, b := pair(t, s)
			func() {
				defer func() {
					if r, ok := recover().(crash); !ok || r.point != c.point {
						t.Fatalf("expected a crash at %s", c.point)
					}
				}()
				_ = s.RunX(ctx, 1, nil, "test", crashAt(c.point), func(x *XTx) error {
					x.SetHome(a)
					if err := bump(ctx, x, a); err != nil {
						return err
					}
					return bump(ctx, x, b)
				})
			}()

			if _, _, err := s.Resolve(ctx, 0, nil); err != nil {
				t.Fatal(err)
			}
			want := int64(0)
			if c.committed {
				want = 1
			}
			if va, vb := versionOf(t, s, a), versionOf(t, s, b); va != want || vb != want {
				t.Errorf("after resolve: versions %d/%d, want %d/%d", va, vb, want, want)
			}
			if _, _, err := s.Resolve(ctx, 0, nil); err != nil { // clean up abort markers
				t.Fatal(err)
			}
			noLeftovers(t, s)
		})
	}
}

// The race the decision row arbitrates (A§9.4): a resolver overtakes a slow
// coordinator between prepare and decision. Its abort must win, the
// coordinator must roll back and retry, and the write must land exactly once.
func TestResolverAbortBeatsSlowCoordinator(t *testing.T) {
	s, _ := twoShards(t)
	ctx := context.Background()
	a, b := pair(t, s)

	stats := map[string]int64{}
	overtaken := false
	slow := func(name string) {
		if name == "twopc.after_all_prepared" && !overtaken {
			overtaken = true
			if _, rolledBack, err := s.Resolve(ctx, 0, nil); err != nil || rolledBack != 2 {
				t.Errorf("resolver: rolledBack=%d err=%v, want 2 shards rolled back", rolledBack, err)
			}
		}
	}
	err := s.RunX(ctx, 3, counterFunc(func(k string, d int64) { stats[k] += d }), "test", slow, func(x *XTx) error {
		x.SetHome(a)
		if err := bump(ctx, x, a); err != nil {
			return err
		}
		return bump(ctx, x, b)
	})
	if err != nil {
		t.Fatal(err)
	}
	if va, vb := versionOf(t, s, a), versionOf(t, s, b); va != 1 || vb != 1 {
		t.Errorf("versions %d/%d, want 1/1 (landed exactly once)", va, vb)
	}
	if stats["twopc.aborts"] != 1 || stats["test.attempts"] != 2 || stats["twopc.commits"] != 1 {
		t.Errorf("counters %v, want 1 abort, 2 attempts, 1 commit", stats)
	}
	if _, _, err := s.Resolve(ctx, 0, nil); err != nil { // clean up the abort marker
		t.Fatal(err)
	}
	noLeftovers(t, s)
}

type counterFunc func(string, int64)

func (f counterFunc) Add(key string, delta int64) { f(key, delta) }
