// Package migrations embeds the SQL schema migrations and seed data so the
// migrate command can run from a single binary with no loose files.
package migrations

import "embed"

// Files holds every .sql file in this directory: the numbered migrations and
// seed.sql.
//
//go:embed *.sql
var Files embed.FS
