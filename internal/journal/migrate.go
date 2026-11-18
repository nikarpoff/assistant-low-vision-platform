package journal

import (
	"context"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strings"

	"github.com/nikarpoff/assistant-low-vision-platform/internal/logger"
)

const createMigrationsTableQuery = `
	CREATE TABLE IF NOT EXISTS schema_migrations (
		version    TEXT PRIMARY KEY,
		applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
	)
`

const selectMigrationsQuery = `
	SELECT version
	FROM schema_migrations
`

// applyMigrationFormat appends the version record to a migration: without arguments the statements go by the
// simple protocol as one query, and PostgreSQL applies them in one implicit transaction. The version is checked
// by migrationNamePattern, so it is safe as a literal.
const applyMigrationFormat = "%s\n;\nINSERT INTO schema_migrations (version) VALUES ('%s')"

// errMigrationNameFormat is the error for a migration file not named NNNN_<name>.sql.
const errMigrationNameFormat = "bad migration file name %q: want NNNN_<name>.sql"

// migrationNamePattern is the name of a migration file; the group is its version.
var migrationNamePattern = regexp.MustCompile(`^([0-9]{4}_[a-z0-9_]+)\.sql$`)

// migrate applies the migrations of fsys that schema_migrations does not list, in the order of their names.
func migrate(ctx context.Context, conn connection, fsys fs.FS) error {
	if err := conn.Exec(ctx, strings.TrimSpace(createMigrationsTableQuery)); err != nil {
		return fmt.Errorf("failed to create schema_migrations: %w", err)
	}

	applied, err := appliedMigrations(ctx, conn)
	if err != nil {
		return err
	}

	names, err := fs.Glob(fsys, "*.sql")
	if err != nil {
		return fmt.Errorf("failed to list migrations: %w", err)
	}
	sort.Strings(names)

	// check every name before applying anything
	versions := make([]string, len(names))
	for i, name := range names {
		match := migrationNamePattern.FindStringSubmatch(name)
		if match == nil {
			return fmt.Errorf(errMigrationNameFormat, name)
		}
		versions[i] = match[1]
	}

	for i, name := range names {
		version := versions[i]
		if applied[version] {
			continue
		}

		body, err := fs.ReadFile(fsys, name)
		if err != nil {
			return fmt.Errorf("failed to read migration (version=%s): %w", version, err)
		}

		if err := conn.Exec(ctx, fmt.Sprintf(applyMigrationFormat, strings.TrimSpace(string(body)), version)); err != nil {
			return fmt.Errorf("failed to apply migration (version=%s): %w", version, err)
		}

		logger.Info("Journal migration applied: version=%s", version)
	}

	return nil
}

// appliedMigrations returns the versions listed in schema_migrations.
func appliedMigrations(ctx context.Context, conn connection) (map[string]bool, error) {
	rows, err := conn.Query(ctx, strings.TrimSpace(selectMigrationsQuery))
	if err != nil {
		return nil, fmt.Errorf("failed to query schema_migrations: %w", err)
	}
	defer rows.Close()

	applied := map[string]bool{}
	for rows.Next() {
		var version string
		if err := rows.Scan(&version); err != nil {
			return nil, fmt.Errorf("failed to scan migration version: %w", err)
		}
		applied[version] = true
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to iterate migration versions: %w", err)
	}

	return applied, nil
}
