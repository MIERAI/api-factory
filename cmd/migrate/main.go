// migrate: versioned schema migrations (goose) on the main database and the shards, before the API starts
// (init container of the api-factory Deployment, so a re-created database gets its tables without a deploy).
//
//	migrate [up|status]       connection: PGHOST / PGPORT / PGUSER / PGPASSWORD (libpq names)
//	MIGRATE_DATABASES          databases, comma-separated (default factory,shard_a,shard_b; missing ones are created)
//	SEED=demo                  then load db/seed/demo-<db>.sql (idempotent; dev only)
//
// A Postgres advisory lock per database: pods starting together run each migration once. Each migration runs in
// its own transaction; the log has the duration of every migration (the migration rehearsal reads it).
package main

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"

	"apifactory/db"
)

func main() {
	cmd := "up"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	if err := run(context.Background(), cmd); err != nil {
		fmt.Fprintln(os.Stderr, "✗", err)
		os.Exit(1)
	}
}

func dsn(database string) string {
	u := url.URL{Scheme: "postgres", Host: env("PGHOST", "localhost") + ":" + env("PGPORT", "5432"), Path: "/" + database,
		User: url.UserPassword(env("PGUSER", "factory"), os.Getenv("PGPASSWORD")), RawQuery: "sslmode=" + env("PGSSLMODE", "disable")}
	return u.String()
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func run(ctx context.Context, cmd string) error {
	dbs := strings.Split(env("MIGRATE_DATABASES", "factory,shard_a,shard_b"), ",")
	if err := ensureDatabases(ctx, dbs); err != nil {
		return err
	}
	migrations, err := fs.Sub(db.Migrations, "migrations")
	if err != nil {
		return err
	}
	for _, name := range dbs {
		if err := migrate(ctx, name, cmd, migrations); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	if cmd == "up" && os.Getenv("SEED") == "demo" {
		for _, name := range dbs {
			if err := seed(ctx, name); err != nil {
				return fmt.Errorf("seed %s: %w", name, err)
			}
		}
	}
	return nil
}

// ensureDatabases: the shards are separate databases in the same cluster (created on first start).
func ensureDatabases(ctx context.Context, dbs []string) error {
	conn, err := open(ctx, dbs[0])
	if err != nil {
		return err
	}
	defer conn.Close()
	for _, name := range dbs[1:] {
		var exists bool
		if err := conn.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)", name).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			if _, err := conn.ExecContext(ctx, "CREATE DATABASE "+quoteIdent(name)); err != nil && !strings.Contains(err.Error(), "already exists") {
				return fmt.Errorf("create database %s: %w", name, err)
			}
			fmt.Printf("✓ database %s created\n", name)
		}
	}
	return nil
}

func open(ctx context.Context, name string) (*sql.DB, error) {
	conn, err := sql.Open("pgx", dsn(name))
	if err != nil {
		return nil, err
	}
	for i := 0; ; i++ { // the database may still be starting (first pod after a re-created cluster)
		if err = conn.PingContext(ctx); err == nil {
			return conn, nil
		}
		if i == 60 {
			return nil, fmt.Errorf("connect %s: %w", name, err)
		}
		time.Sleep(2 * time.Second)
	}
}

func migrate(ctx context.Context, name, cmd string, migrations fs.FS) error {
	conn, err := open(ctx, name)
	if err != nil {
		return err
	}
	defer conn.Close()
	locker, err := lock.NewPostgresSessionLocker()
	if err != nil {
		return err
	}
	p, err := goose.NewProvider(goose.DialectPostgres, conn, migrations, goose.WithSessionLocker(locker))
	if err != nil {
		return err
	}
	switch cmd {
	case "status":
		st, err := p.Status(ctx)
		if err != nil {
			return err
		}
		for _, s := range st {
			fmt.Printf("%s  %05d %-40s %s\n", name, s.Source.Version, s.Source.Path, s.State)
		}
		return nil
	case "up":
		t0 := time.Now()
		res, err := p.Up(ctx)
		for _, r := range res {
			fmt.Printf("%s  applied %05d %-40s %6dms\n", name, r.Source.Version, r.Source.Path, r.Duration.Milliseconds())
		}
		if err != nil {
			return err
		}
		v, err := p.GetDBVersion(ctx)
		if err != nil {
			return err
		}
		fmt.Printf("✓ %s at version %d (%d applied, %s)\n", name, v, len(res), time.Since(t0).Round(time.Millisecond))
		return nil
	}
	return fmt.Errorf("unknown command %q (up | status)", cmd)
}

func seed(ctx context.Context, name string) error {
	b, err := db.Seed.ReadFile("seed/demo-" + name + ".sql")
	if err != nil {
		return nil // no demo data for this database
	}
	conn, err := open(ctx, name)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, string(b)); err != nil {
		return err
	}
	fmt.Printf("✓ seed %s\n", name)
	return nil
}

func quoteIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }
