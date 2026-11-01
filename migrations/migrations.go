// Package migrations embeds the SQL migrations of the journal database (PLT-11).
//
// A migration is a file NNNN_<name>.sql. The journal applies the files that
// schema_migrations does not list yet, in the order of their names, each one
// together with its version in one implicit transaction. So a file holds plain
// statements without BEGIN and COMMIT and is never changed once released.
package migrations

import "embed"

// FS holds the migration files.
//
//go:embed *.sql
var FS embed.FS
