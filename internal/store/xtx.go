package store

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"payment-ledger/internal/shard"
)

// XTx is one logical write over every shard it touches (A§9.2). It begins a
// tx on a shard the first time the path touches it. Commit is a plain local
// commit when only one shard was touched, and two-phase commit otherwise.
type XTx struct {
	ctx    context.Context
	shards *Shards
	txs    map[int]pgx.Tx
	home   uuid.UUID
	stats  Counter
	fail   func(name string) // A§9.4 crash points; nil disables
}

// lockTimeout bounds lock waits once a write spans shards: Postgres can't see
// a deadlock cycle that crosses shards, so the timeout breaks it and RunX
// retries.
const lockTimeout = "SET LOCAL lock_timeout = '5s'"

// commitTimeout bounds the 2PC commit phase, which runs detached from the
// request's cancellation (A§9.4). It matches PREPARE_TIMEOUT's default.
const commitTimeout = 2 * time.Second

// RunX runs fn as one cross-shard write and retries it from scratch on a
// version conflict, a cross-shard lock timeout, or a 2PC the resolver
// aborted, like RunCAS. fail fires the twopc.* crash points (nil disables).
func (s *Shards) RunX(ctx context.Context, attempts int, stats Counter, op string, fail func(string), fn func(*XTx) error) error {
	return retryOnConflict(ctx, attempts, stats, op, func() error {
		if s.admit != nil {
			select {
			case s.admit <- struct{}{}:
				defer func() { <-s.admit }()
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		x := &XTx{ctx: ctx, shards: s, txs: map[int]pgx.Tx{}, stats: stats, fail: fail}
		// Always roll back on the way out, so a panic can't leak open txs
		// holding locks. After a commit or a PREPARE it's a harmless no-op.
		defer x.rollback()
		if err := fn(x); err != nil {
			return asConflict(err)
		}
		return asConflict(x.commit())
	})
}

// asConflict maps a lock timeout (SQLSTATE 55P03) onto ErrVersionConflict so
// the write retries.
func asConflict(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "55P03" {
		return fmt.Errorf("%w: %v", ErrVersionConflict, err)
	}
	return err
}

// On returns shard i's tx, beginning it on first use.
func (x *XTx) On(i int) (pgx.Tx, error) {
	if tx, ok := x.txs[i]; ok {
		return tx, nil
	}
	tx, err := x.shards.pools[i].BeginTx(x.ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return nil, fmt.Errorf("begin on shard %d: %w", i, err)
	}
	x.txs[i] = tx
	if len(x.txs) == 2 { // just became distributed: bound the existing tx too
		for _, t := range x.txs {
			if _, err := t.Exec(x.ctx, lockTimeout); err != nil {
				return nil, err
			}
		}
	} else if len(x.txs) > 2 {
		if _, err := tx.Exec(x.ctx, lockTimeout); err != nil {
			return nil, err
		}
	}
	return tx, nil
}

// For returns the tx of the shard that stores id.
func (x *XTx) For(id uuid.UUID) (pgx.Tx, error) {
	return x.On(shard.Route(id, x.shards.N()))
}

// SetHome names the Transfer this write creates or changes. Its shard holds
// the commit decision, and its id names the 2PC (A§9.2).
func (x *XTx) SetHome(id uuid.UUID) { x.home = id }

func (x *XTx) rollback() {
	for _, tx := range x.txs {
		_ = tx.Rollback(x.ctx)
	}
}

// GID is a fresh 2PC transaction identifier for one attempt of a write whose
// home is id: "x-<home>-<nonce>". The resolver routes it back to the
// decision's shard. The nonce matters because a retried write keeps its
// Transfer id, and its earlier attempt's gid may already be decided "abort".
func GID(home uuid.UUID) string {
	return "x-" + home.String() + "-" + uuid.NewString()[:8]
}

// HomeOf parses a GID back to its home id, or reports false for a foreign gid.
func HomeOf(gid string) (uuid.UUID, bool) {
	if len(gid) < 2+36 || gid[:2] != "x-" {
		return uuid.Nil, false
	}
	id, err := uuid.Parse(gid[2 : 2+36])
	return id, err == nil
}

func (x *XTx) hit(name string) {
	if x.fail != nil {
		x.fail(name)
	}
}

func (x *XTx) commit() error {
	switch len(x.txs) {
	case 0:
		return nil
	case 1:
		for _, tx := range x.txs {
			return tx.Commit(x.ctx)
		}
	}
	if x.home == uuid.Nil {
		x.rollback()
		return errors.New("cross-shard write has no home Transfer")
	}
	gid := GID(x.home)
	// From the first PREPARE on, a client deadline must not strand a
	// prepared tx holding locks: the rest runs detached, bounded (A§9.4).
	ctx, cancel := context.WithTimeout(context.WithoutCancel(x.ctx), commitTimeout)
	defer cancel()
	order := make([]int, 0, len(x.txs))
	for i := range x.txs {
		order = append(order, i)
	}
	slices.Sort(order)

	// Phase 1: prepare every participant in shard order.
	var prepared []int
	for _, i := range order {
		tx := x.txs[i]
		if _, err := tx.Exec(ctx, "PREPARE TRANSACTION '"+gid+"'"); err != nil {
			x.abort(ctx, gid, prepared, order)
			return fmt.Errorf("prepare on shard %d: %w", i, err)
		}
		// The session no longer has an open tx, so this sends a server-side
		// no-op ROLLBACK and only returns the connection to the pool.
		_ = tx.Rollback(ctx)
		prepared = append(prepared, i)
		if len(prepared) == 1 {
			x.hit("twopc.after_first_prepare")
		}
	}
	x.hit("twopc.after_all_prepared")

	// The commit point. The decision row arbitrates against the resolver
	// (A§9.4): if its 'abort' got there first, this write must not commit.
	home := x.shards.For(x.home)
	tag, err := home.Exec(ctx,
		`INSERT INTO decisions (gid, outcome) VALUES ($1, 'commit') ON CONFLICT (gid) DO NOTHING`, gid)
	if err != nil {
		x.abort(ctx, gid, prepared, nil)
		return fmt.Errorf("record decision: %w", err)
	}
	if tag.RowsAffected() == 0 {
		x.abort(ctx, gid, prepared, nil)
		count(x.stats, "twopc.aborts")
		return fmt.Errorf("%w: resolver aborted 2PC %s first", ErrVersionConflict, gid)
	}
	x.hit("twopc.after_decision")

	// Phase 2. A failure here leaves a decided gid for the resolver to finish.
	done := true
	for n, i := range order {
		if _, err := x.shards.pools[i].Exec(ctx, "COMMIT PREPARED '"+gid+"'"); err != nil {
			done = false
		}
		if n == 0 {
			x.hit("twopc.after_first_commit")
		}
	}
	count(x.stats, "twopc.commits")
	if done {
		_, _ = home.Exec(ctx, `DELETE FROM decisions WHERE gid = $1`, gid)
	}
	return nil
}

// abort undoes a write that hasn't reached its decision: prepared shards are
// rolled back by gid, and shards still open are rolled back directly.
func (x *XTx) abort(ctx context.Context, gid string, prepared, order []int) {
	for _, i := range prepared {
		_, _ = x.shards.pools[i].Exec(ctx, "ROLLBACK PREPARED '"+gid+"'")
	}
	for _, i := range order {
		if !slices.Contains(prepared, i) {
			_ = x.txs[i].Rollback(ctx)
		}
	}
}
