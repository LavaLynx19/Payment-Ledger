package store

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"

	"payment-ledger/internal/shard"
)

// twoShards opens the shards named by SHARD_URLS and skips unless there are
// at least two (Rung 4 runs bring up postgres-shard1).
func twoShards(t *testing.T) (*Shards, []string) {
	t.Helper()
	var urls []string
	for _, u := range strings.Split(os.Getenv("SHARD_URLS"), ",") {
		if u = strings.TrimSpace(u); u != "" {
			urls = append(urls, u)
		}
	}
	if len(urls) < 2 {
		t.Skip("SHARD_URLS lists fewer than 2 shards; skipping multi-shard test")
	}
	ctx := context.Background()
	if err := MigrateAll(ctx, urls); err != nil {
		t.Fatal(err)
	}
	s, err := OpenShards(ctx, urls)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s, urls
}

func existsOn(t *testing.T, s *Shards, i int, id uuid.UUID) bool {
	t.Helper()
	var ok bool
	if err := s.Pool(i).QueryRow(context.Background(),
		`SELECT EXISTS (SELECT 1 FROM accounts WHERE id = $1)`, id).Scan(&ok); err != nil {
		t.Fatal(err)
	}
	return ok
}

func TestWalletsSpreadAndRouteAcrossShards(t *testing.T) {
	s, _ := twoShards(t)
	ids, err := s.CreateWallets(context.Background(), 5)
	if err != nil {
		t.Fatal(err)
	}
	perShard := map[int]int{}
	for _, id := range ids {
		home := shard.Route(id, s.N())
		perShard[home]++
		if s.For(id) != s.Pool(home) {
			t.Errorf("For(%s) isn't shard %d's pool", id, home)
		}
		for i := range s.N() {
			if got := existsOn(t, s, i, id); got != (i == home) {
				t.Errorf("wallet %s on shard %d: exists=%v, want %v", id, i, got, i == home)
			}
		}
	}
	if perShard[0] != 3 || perShard[1] != 2 {
		t.Errorf("round-robin split = %v, want 3 on shard 0 and 2 on shard 1", perShard)
	}
}

func TestFundingAccountPerShard(t *testing.T) {
	s, _ := twoShards(t)
	ctx := context.Background()
	seen := map[uuid.UUID]bool{}
	for i := range s.N() {
		id, err := EnsureFundingAccountOn(ctx, s.Pool(i), i)
		if err != nil {
			t.Fatal(err)
		}
		again, err := EnsureFundingAccountOn(ctx, s.Pool(i), i)
		if err != nil || again != id {
			t.Errorf("shard %d: second Ensure = %s, %v; want %s", i, again, err, id)
		}
		if shard.Route(id, s.N()) != i || !existsOn(t, s, i, id) {
			t.Errorf("shard %d funding %s routes to %d", i, id, shard.Route(id, s.N()))
		}
		seen[id] = true
	}
	if len(seen) != s.N() {
		t.Errorf("got %d distinct funding accounts, want %d", len(seen), s.N())
	}
}
