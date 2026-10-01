// Command worker runs the background loops: capture of PENDING Transfers
// (A§5 Capture) and the sweeper that finalizes expired Holds and purges old
// idempotency keys. Capture goroutines claim Holds with FOR UPDATE SKIP
// LOCKED, so any number of goroutines and processes can run side by side.
package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"payment-ledger/internal/env"
	"payment-ledger/internal/failpoint"
	"payment-ledger/internal/ledger"
	"payment-ledger/internal/store"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := store.Open(ctx, env.Must("DATABASE_URL"))
	if err != nil {
		log.Fatalf("worker: %v", err)
	}
	defer db.Close()
	fp, err := failpoint.Parse(env.Or("FAILPOINTS", ""))
	if err != nil {
		log.Fatalf("worker: FAILPOINTS: %v", err)
	}
	log.Printf("worker: failpoints: %s", fp)
	l, err := ledger.New(ctx, db, ledger.Config{
		CASAttempts:  env.Int("CAS_ATTEMPTS", 10),
		KeyRetention: env.Duration("IDEMPOTENCY_RETENTION", 24*time.Hour),
		Failpoints:   fp,
	})
	if err != nil {
		log.Fatalf("worker: %v", err)
	}

	concurrency := env.Int("WORKER_CONCURRENCY", 4)
	poll := env.Duration("WORKER_POLL", 20*time.Millisecond)
	sweepEvery := env.Duration("SWEEP_INTERVAL", time.Second)
	log.Printf("worker: %d capture loops (idle poll %s), sweeper every %s", concurrency, poll, sweepEvery)

	var wg sync.WaitGroup
	for range concurrency {
		wg.Go(func() { captureLoop(ctx, l, poll) })
	}
	wg.Go(func() { sweepLoop(ctx, l, sweepEvery) })
	wg.Wait()
}

// captureLoop captures back to back while Holds are waiting and sleeps for
// poll when there are none.
func captureLoop(ctx context.Context, l *ledger.Ledger, poll time.Duration) {
	for ctx.Err() == nil {
		found, err := l.CaptureNext(ctx)
		if err != nil && ctx.Err() == nil {
			log.Printf("worker: capture: %v", err)
		}
		if found && err == nil {
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
