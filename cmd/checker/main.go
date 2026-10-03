// Command checker verifies the ledger invariants (A§4, A§9.6) on every
// shard, or on TigerBeetle when LEDGER_ENGINE=tigerbeetle. With --acks it
// also checks that every Transfer a client was told succeeded exists, is
// posted, and still owns its key. With --once it runs a single pass and exits
// non-zero on any violation; otherwise it repeats every --interval.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
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
	acksPath := flag.String("acks", "", `file of "<key> <transfer id>" lines to verify ("-" for stdin)`)
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	acks, err := readAcks(*acksPath)
	if err != nil {
		log.Fatalf("checker: %v", err)
	}

	pass := postgresPass
	if env.Or("LEDGER_ENGINE", "postgres") == "tigerbeetle" {
		pass = tigerbeetlePass
	}
	for {
		results, err := pass(ctx, acks)
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

func postgresPass(ctx context.Context, acks []checker.Ack) ([]checker.Result, error) {
	shards, err := store.OpenShards(ctx, env.ShardURLs())
	if err != nil {
		return nil, err
	}
	defer shards.Close()
	results, err := checker.Snapshot(ctx, shards)
	if err != nil || acks == nil {
		return results, err
	}
	qs := make([]store.Querier, shards.N())
	for i := range qs {
		qs[i] = shards.Pool(i)
	}
	r, err := checker.VerifyAcks(ctx, qs, acks)
	return append(results, r), err
}

func readAcks(path string) ([]checker.Ack, error) {
	if path == "" {
		return nil, nil
	}
	var in io.Reader = os.Stdin
	if path != "-" {
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		in = f
	}
	acks, err := checker.ParseAcks(in)
	if acks == nil {
		acks = []checker.Ack{} // asked for, even if empty: still report the check
	}
	return acks, err
}

func report(results []checker.Result) {
	for _, r := range results {
		status := "ok  "
		if r.Violations > 0 {
			status = "FAIL"
		}
		label := fmt.Sprintf("invariant %d", r.Invariant)
		if r.Invariant == 0 {
			label = "harness"
		}
		fmt.Printf("%s %s: %s (%d violations)\n", status, label, r.Name, r.Violations)
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
