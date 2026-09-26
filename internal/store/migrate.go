package store

import (
	"context"
	"database/sql"
	"fmt"

	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver goose needs
	"github.com/pressly/goose/v3"

	"payment-ledger/migrations"
)

// Migrate applies all pending migrations. It returns an empty list when the
// schema is already current.
func Migrate(ctx context.Context, dsn string) ([]*goose.MigrationResult, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	defer db.Close()

	p, err := goose.NewProvider(goose.DialectPostgres, db, migrations.FS)
	if err != nil {
		return nil, fmt.Errorf("goose provider: %w", err)
	}
	return p.Up(ctx)
}
