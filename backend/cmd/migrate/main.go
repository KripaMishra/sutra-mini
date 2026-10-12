// Command migrate applies the embedded schema migrations and, with -seed, the
// seed data. Safe to run repeatedly: applied versions are tracked in
// schema_migrations and the seed is idempotent.
package main

import (
	"context"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/KripaMishra/sutra-mini/backend/internal/config"
	"github.com/KripaMishra/sutra-mini/backend/internal/db"
	"github.com/KripaMishra/sutra-mini/backend/migrations"
)

func main() {
	seed := flag.Bool("seed", false, "apply the seed data after migrating")
	check := flag.Bool("check", false, "print table row counts and exit")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	if cfg.DatabaseURL == "" {
		log.Fatal("DATABASE_URL is not set — copy .env.example to .env and fill it in")
	}

	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("database: %v", err)
	}
	defer pool.Close()

	if err := ensureMigrationsTable(ctx, pool); err != nil {
		log.Fatalf("migrations table: %v", err)
	}
	applied, err := applyMigrations(ctx, pool)
	if err != nil {
		log.Fatalf("migrations: %v", err)
	}
	fmt.Printf("migrations: %d applied\n", applied)

	if *seed {
		if err := applySeed(ctx, pool); err != nil {
			log.Fatalf("seed: %v", err)
		}
		fmt.Println("seed: applied")
	}

	if *check {
		if err := checkCounts(ctx, pool); err != nil {
			log.Fatalf("check: %v", err)
		}
	}
}

func ensureMigrationsTable(ctx context.Context, pool *pgxpool.Pool) error {
	_, err := pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    text PRIMARY KEY,
		applied_at timestamptz NOT NULL DEFAULT now()
	)`)
	return err
}

func applyMigrations(ctx context.Context, pool *pgxpool.Pool) (int, error) {
	entries, err := fs.ReadDir(migrations.Files, ".")
	if err != nil {
		return 0, err
	}
	var names []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".sql") || name[0] < '0' || name[0] > '9' {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)

	applied := 0
	for _, name := range names {
		sqlBytes, err := migrations.Files.ReadFile(name)
		if err != nil {
			return applied, err
		}
		done, err := applyMigration(ctx, pool, name, string(sqlBytes))
		if err != nil {
			return applied, fmt.Errorf("%s: %w", name, err)
		}
		if done {
			applied++
			fmt.Printf("applied %s\n", name)
		}
	}
	return applied, nil
}

func applyMigration(ctx context.Context, pool *pgxpool.Pool, version, sqlText string) (bool, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)`, version).Scan(&exists); err != nil {
		return false, err
	}
	if exists {
		return false, nil
	}

	if err := execSQL(ctx, tx, sqlText); err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES ($1)`, version); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}

func applySeed(ctx context.Context, pool *pgxpool.Pool) error {
	sqlBytes, err := migrations.Files.ReadFile("seed.sql")
	if err != nil {
		return err
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := execSQL(ctx, tx, string(sqlBytes)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// execSQL runs a file that may contain several statements. The simple
// protocol lets the server parse the batch as a whole.
func execSQL(ctx context.Context, tx pgx.Tx, sqlText string) error {
	mrr := tx.Conn().PgConn().Exec(ctx, sqlText)
	if _, err := mrr.ReadAll(); err != nil {
		return err
	}
	return nil
}

func checkCounts(ctx context.Context, pool *pgxpool.Pool) error {
	for _, table := range []string{"skus", "parties", "jobwork_issues", "jobwork_receipts", "vouchers"} {
		var n int64
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&n); err != nil {
			return err
		}
		fmt.Printf("%-18s %d\n", table+":", n)
	}
	return nil
}
