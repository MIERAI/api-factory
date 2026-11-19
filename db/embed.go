// Package db: the versioned migrations (goose, db/migrations) and the demo data (db/seed), embedded in cmd/migrate.
// db/schema is the current schema for the factory (sqlc, types); CI keeps both identical (scripts/ci/schema-drift.sh).
package db

import "embed"

//go:embed migrations/*.sql
var Migrations embed.FS

//go:embed seed/*.sql
var Seed embed.FS
