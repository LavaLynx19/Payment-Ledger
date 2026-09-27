// Command checker verifies the ledger invariants (A§4). With --once it runs a
// single pass and exits non-zero on any violation. Otherwise it repeats every
// --interval and logs each pass.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"payment-ledger/internal/checker"
	"payment-ledger/internal/env"
	"payment-ledger/internal/store"
)

func main() {
	once := flag.Bool("once", false, "run one pass and exit non-zero on violations")
	interval := flag.Duration("interval", 10*time.Second, "time between passes when not --once")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := store.Open(ctx, env.Must("DATABASE_URL"))
	if err != nil {
		log.Fatalf("checker: %v", err)
	}
	defer db.Close()

	for {
		results, err := checker.Snapshot(ctx, db)
		if err != nil {
			log.Fatalf("checker: %v", err)
		}
		report(results)
		if *once {
			if !checker.Clean(results) {
				os.Exit(1)
			}
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(*interval):
		}
	}
}

func report(results []checker.Result) {
	for _, r := range results {
		status := "ok  "
		if r.Violations > 0 {
			status = "FAIL"
		}
		fmt.Printf("%s invariant %d: %s (%d violations)\n", status, r.Invariant, r.Name, r.Violations)
		for _, s := range r.Samples {
			fmt.Printf("       %s\n", s)
		}
	}
	if checker.Clean(results) {
		fmt.Println("checker: CLEAN")
	} else {
		fmt.Println("checker: VIOLATIONS FOUND")
	}
}
