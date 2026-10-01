// Command api runs the ledger API server and its operational subcommands.
package main

import (
	"context"
	"errors"
	"expvar"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"payment-ledger/internal/api"
	"payment-ledger/internal/env"
	"payment-ledger/internal/failpoint"
	"payment-ledger/internal/ledger"
	"payment-ledger/internal/metrics"
	"payment-ledger/internal/store"
)

const usage = "usage: api <serve|migrate>"

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	switch os.Args[1] {
	case "serve":
		runServe()
	case "migrate":
		runMigrate()
	default:
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
}

func runServe() {
	tokens := api.ParseTokens(env.Must("LEDGER_SERVICE_TOKENS"))
	if len(tokens) == 0 {
		log.Fatal("LEDGER_SERVICE_TOKENS has no tokens")
	}
	addr := env.Or("LISTEN_ADDR", ":8080")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	shards, err := store.OpenShards(ctx, env.ShardURLs())
	if err != nil {
		log.Fatalf("api: %v", err)
	}
	defer shards.Close()
	// Temporary until cross-shard Accept and Capture land (PLAN P5.5–P5.6):
	// ledger operations still run on shard 0 only.
	if shards.N() > 1 {
		log.Fatalf("%s: %d shards configured; multi-shard ledger operations arrive in P5.5–P5.6", "api", shards.N())
	}
	db := shards.Pool(0)
	fp, err := failpoint.Parse(env.Or("FAILPOINTS", ""))
	if err != nil {
		log.Fatalf("api: FAILPOINTS: %v", err)
	}
	log.Printf("api: failpoints: %s", fp)
	cas := expvar.NewMap("cas")
	l, err := ledger.New(ctx, db, ledger.Config{
		HoldTTL:     env.Duration("HOLD_TTL", 30*time.Second),
		CASAttempts: env.Int("CAS_ATTEMPTS", 10),
		Failpoints:  fp,
		CASStats:    cas,
	})
	if err != nil {
		log.Fatalf("api: %v", err)
	}
	logged := make(chan struct{})
	go func() { defer close(logged); metrics.LogEvery(ctx, "cas", cas, 10*time.Second) }()
	defer func() { <-logged }() // final totals are logged before exit

	mux := http.NewServeMux()
	mux.Handle("/", api.NewHandler(api.NewService(l), tokens))
	mux.Handle("/debug/vars", expvar.Handler())

	// HTTP/1.1 for Connect JSON clients, plaintext HTTP/2 (h2c) for gRPC clients like k6.
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true)

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		Protocols:         protocols,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	log.Printf("api: listening on %s (HTTP/1.1 + h2c)", addr)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("api: %v", err)
	}
}

func runMigrate() {
	for i, url := range env.ShardURLs() {
		results, err := store.Migrate(context.Background(), url)
		if err != nil {
			log.Fatalf("migrate: shard %d: %v", i, err)
		}
		if len(results) == 0 {
			log.Printf("migrate: shard %d: schema already current", i)
		}
		for _, r := range results {
			log.Printf("migrate: shard %d: applied %s in %s", i, r.Source.Path, r.Duration)
		}
	}
}
