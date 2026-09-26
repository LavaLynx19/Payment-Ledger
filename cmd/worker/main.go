// Command worker captures PENDING Transfers (A§5 Capture). Each goroutine
// claims Holds with FOR UPDATE SKIP LOCKED, so any number of goroutines and
// processes can run side by side.
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
	l, err := ledger.New(ctx, db, ledger.Config{CASAttempts: env.Int("CAS_ATTEMPTS", 10)})
	if err != nil {
		log.Fatalf("worker: %v", err)
	}

	concurrency := env.Int("WORKER_CONCURRENCY", 4)
	poll := env.Duration("WORKER_POLL", 20*time.Millisecond)
	log.Printf("worker: %d capture loops, idle poll %s", concurrency, poll)

	var wg sync.WaitGroup
	for range concurrency {
		wg.Go(func() { captureLoop(ctx, l, poll) })
	}
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
		select {
		case <-ctx.Done():
		case <-time.After(poll):
		}
	}
}
