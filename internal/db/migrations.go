package db

import "embed"

// migrationsFS embeds the goose migration files so the binary carries its own
// schema and applies it at startup — no separate migration step or binary
// needed in the runtime image.
//
//go:embed migrations/*.sql
var migrationsFS embed.FS

const migrationsDir = "migrations"
