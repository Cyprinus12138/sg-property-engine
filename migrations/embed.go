// Package migrations embeds the SQL schema migrations so every binary
// built from this module carries the schema it expects. Apply them with
// db.RunMigrations (or the golang-migrate CLI against this directory).
package migrations

import "embed"

// FS exposes the *.sql migration files to golang-migrate's iofs source.
//
//go:embed *.sql
var FS embed.FS
