package store

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"payment-ledger/internal/shard"
)

// Shards holds one pool per Postgres shard, in shard-index order (A§9.1).
type Shards struct {
	pools []*pgxpool.Pool
}

// OpenShards connects a pool to every shard URL.
func OpenShards(ctx context.Context, urls []string) (*Shards, error) {
	if len(urls) == 0 || len(urls) > shard.Max {
		return nil, fmt.Errorf("need 1 to %d shard URLs, got %d", shard.Max, len(urls))
	}
	s := &Shards{}
	for i, u := range urls {
		pool, err := Open(ctx, u)
		if err != nil {
			s.Close()
			return nil, fmt.Errorf("shard %d: %w", i, err)
		}
		s.pools = append(s.pools, pool)
	}
	return s, nil
}

// MigrateAll applies pending migrations to every shard.
func MigrateAll(ctx context.Context, urls []string) error {
	for i, u := range urls {
		if _, err := Migrate(ctx, u); err != nil {
			return fmt.Errorf("shard %d: %w", i, err)
		}
	}
	return nil
}

func (s *Shards) N() int { return len(s.pools) }

// Pool is shard i's pool.
func (s *Shards) Pool(i int) *pgxpool.Pool { return s.pools[i] }

// For is the pool of the shard that stores id.
func (s *Shards) For(id uuid.UUID) *pgxpool.Pool { return s.pools[shard.Route(id, len(s.pools))] }

func (s *Shards) Close() {
	for _, p := range s.pools {
		p.Close()
	}
}

// CreateWallets inserts n empty Wallets spread round-robin across shards,
// each minted with its shard's bits.
func (s *Shards) CreateWallets(ctx context.Context, n int) ([]uuid.UUID, error) {
	ids := make([]uuid.UUID, 0, n)
	for i := range s.N() {
		count := n / s.N()
		if i < n%s.N() {
			count++
		}
		created, err := CreateWalletsOn(ctx, s.pools[i], i, count)
		if err != nil {
			return nil, fmt.Errorf("shard %d: %w", i, err)
		}
		ids = append(ids, created...)
	}
	return ids, nil
}
