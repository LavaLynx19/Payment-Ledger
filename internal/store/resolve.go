package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Resolve finishes in-doubt 2PC writes on every shard (A§9.4). A prepared
// gid whose home decision says commit is committed. One that says abort, or
// has no decision and has been prepared longer than timeout, is rolled back,
// after recording 'abort' so a slow coordinator can't commit it afterwards.
// Decisions older than timeout whose gid is no longer prepared anywhere are
// deleted.
func (s *Shards) Resolve(ctx context.Context, timeout time.Duration, stats Counter) (committed, rolledBack int, err error) {
	type inDoubt struct {
		shard int
		gid   string
		stale bool
	}
	var found []inDoubt
	stillPrepared := map[string]bool{}
	for i, pool := range s.pools {
		rows, err := pool.Query(ctx,
			`SELECT gid, prepared < clock_timestamp() - $1::bigint * interval '1 microsecond'
			 FROM pg_prepared_xacts WHERE database = current_database() AND gid LIKE 'x-%'`,
			timeout.Microseconds())
		if err != nil {
			return committed, rolledBack, fmt.Errorf("shard %d: list prepared: %w", i, err)
		}
		for rows.Next() {
			d := inDoubt{shard: i}
			if err := rows.Scan(&d.gid, &d.stale); err != nil {
				rows.Close()
				return committed, rolledBack, err
			}
			found = append(found, d)
			stillPrepared[d.gid] = true
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return committed, rolledBack, err
		}
	}

	for _, d := range found {
		home, ok := HomeOf(d.gid)
		if !ok {
			continue
		}
		hp := s.For(home)
		outcome, err := decisionOf(ctx, hp, d.gid)
		if err != nil {
			return committed, rolledBack, err
		}
		if outcome == "" {
			if !d.stale {
				continue // the coordinator may still be working on it
			}
			if _, err := hp.Exec(ctx,
				`INSERT INTO decisions (gid, outcome) VALUES ($1, 'abort') ON CONFLICT (gid) DO NOTHING`, d.gid); err != nil {
				return committed, rolledBack, fmt.Errorf("record abort: %w", err)
			}
			if outcome, err = decisionOf(ctx, hp, d.gid); err != nil { // a commit may have won the race
				return committed, rolledBack, err
			}
		}
		verb, stat := "ROLLBACK PREPARED", "resolve.rolled_back"
		if outcome == "commit" {
			verb, stat = "COMMIT PREPARED", "resolve.committed"
		}
		if _, err := s.pools[d.shard].Exec(ctx, verb+" '"+d.gid+"'"); err != nil {
			if notPrepared(err) {
				continue // the coordinator finished it first
			}
			return committed, rolledBack, fmt.Errorf("shard %d: %s %s: %w", d.shard, verb, d.gid, err)
		}
		count(stats, stat)
		if outcome == "commit" {
			committed++
		} else {
			rolledBack++
		}
	}

	for i, pool := range s.pools {
		// Non-nil on purpose: a nil slice is sent as NULL, and NOT (gid = ANY(NULL))
		// is NULL, which would delete nothing whenever nothing is prepared.
		active := []string{}
		for gid := range stillPrepared {
			active = append(active, gid)
		}
		if _, err := pool.Exec(ctx,
			`DELETE FROM decisions
			 WHERE decided_at < clock_timestamp() - $1::bigint * interval '1 microsecond'
			   AND NOT (gid = ANY($2))`, timeout.Microseconds(), active); err != nil {
			return committed, rolledBack, fmt.Errorf("shard %d: clean decisions: %w", i, err)
		}
	}
	return committed, rolledBack, nil
}

// decisionOf is a gid's recorded outcome, or "" if there is none.
func decisionOf(ctx context.Context, q Querier, gid string) (string, error) {
	var outcome string
	err := q.QueryRow(ctx, `SELECT outcome FROM decisions WHERE gid = $1`, gid).Scan(&outcome)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read decision: %w", err)
	}
	return outcome, nil
}

// notPrepared reports Postgres's "prepared transaction does not exist"
// (SQLSTATE 42704).
func notPrepared(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "42704"
}
