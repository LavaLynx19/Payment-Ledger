// Command worker runs the background loops: capture of PENDING Transfers
// (A§5 Capture) and the sweeper that finalizes expired Holds and purges old
// idempotency keys. Capture goroutines claim Holds with FOR UPDATE SKIP
// LOCKED, so any number of goroutines and processes can run side by side.
package main

import (
	"context"
	"expvar"
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"payment-ledger/internal/env"
	"payment-ledger/internal/failpoint"
	"payment-ledger/internal/ledger"
	"payment-ledger/internal/metrics"
	"payment-ledger/internal/store"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	shards, err := store.OpenShards(ctx, env.ShardURLs())
	if err != nil {
		log.Fatalf("worker: %v", err)
	}
	defer shards.Close()
	// Temporary until cross-shard Accept and Capture land (PLAN P5.5–P5.6):
	// ledger operations still run on shard 0 only.
	if shards.N() > 1 {
		log.Fatalf("%s: %d shards configured; multi-shard ledger operations arrive in P5.5–P5.6", "worker", shards.N())
	}
	fp, err := failpoint.Parse(env.Or("FAILPOINTS", ""))
	if err != nil {
		log.Fatalf("worker: FAILPOINTS: %v", err)
	}
	log.Printf("worker: failpoints: %s", fp)
	cas := expvar.NewMap("cas")
	l, err := ledger.New(ctx, shards, ledger.Config{
		CASAttempts:  env.Int("CAS_ATTEMPTS", 10),
		KeyRetention: env.Duration("IDEMPOTENCY_RETENTION", 24*time.Hour),
		Failpoints:   fp,
		CASStats:     cas,
	})
	if err != nil {
		log.Fatalf("worker: %v", err)
	}

	concurrency := env.Int("WORKER_CONCURRENCY", 4)
	batch := env.Int("WORKER_BATCH", 100)
	poll := env.Duration("WORKER_POLL", 20*time.Millisecond)
	sweepEvery := env.Duration("SWEEP_INTERVAL", time.Second)
	log.Printf("worker: %d capture loops (batch ≤ %d, idle poll %s), sweeper every %s", concurrency, batch, poll, sweepEvery)

	var wg sync.WaitGroup
	for range concurrency {
		wg.Go(func() { captureLoop(ctx, l, batch, poll) })
	}
	wg.Go(func() { sweepLoop(ctx, l, sweepEvery) })
	wg.Go(func() { metrics.LogEvery(ctx, "cas", cas, 10*time.Second) })
	wg.Wait()
}

// captureLoop captures batches back to back while Holds are waiting and
// sleeps for poll when there are none.
func captureLoop(ctx context.Context, l *ledger.Ledger, batch int, poll time.Duration) {
	for ctx.Err() == nil {
		n, err := l.CaptureBatch(ctx, batch)
		if err != nil && ctx.Err() == nil {
			log.Printf("worker: capture: %v", err)
		}
		if n > 0 && err == nil {
			continue
		}
		sleep(ctx, poll)
	}
}

func sweepLoop(ctx context.Context, l *ledger.Ledger, every time.Duration) {
	for ctx.Err() == nil {
		expired, purged, err := l.Sweep(ctx)
		if err != nil && ctx.Err() == nil {
			log.Printf("worker: sweep: %v", err)
		}
		if expired > 0 || purged > 0 {
			log.Printf("worker: sweep expired %d holds, purged %d idempotency keys", expired, purged)
		}
		sleep(ctx, every)
	}
}

func sleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}
