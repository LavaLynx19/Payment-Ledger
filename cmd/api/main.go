// Command api runs the ledger API server and its operational subcommands.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"payment-ledger/internal/api"
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
	tokens := api.ParseTokens(mustEnv("LEDGER_SERVICE_TOKENS"))
	if len(tokens) == 0 {
		log.Fatal("LEDGER_SERVICE_TOKENS has no tokens")
	}
	addr := envOr("LISTEN_ADDR", ":8080")

	// HTTP/1.1 for Connect JSON clients, plaintext HTTP/2 (h2c) for gRPC clients like k6.
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true)

	srv := &http.Server{
		Addr:              addr,
		Handler:           api.NewHandler(&api.Service{}, tokens),
		Protocols:         protocols,
		ReadHeaderTimeout: 5 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
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
	results, err := store.Migrate(context.Background(), mustEnv("DATABASE_URL"))
	if err != nil {
		log.Fatalf("migrate: %v", err)
	}
	if len(results) == 0 {
		log.Print("migrate: schema already current")
	}
	for _, r := range results {
		log.Printf("migrate: applied %s in %s", r.Source.Path, r.Duration)
	}
}

func mustEnv(name string) string {
	v := os.Getenv(name)
	if v == "" {
		log.Fatalf("%s is not set", name)
	}
	return v
}

func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}
