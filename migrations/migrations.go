// Package migrations embeds the SQL migration files so the routegate binary
// can apply them itself (ADR-0001 §2.3: no external CLI dependency).
package migrations

import (
	"embed"
)

//go:embed *.sql
var FS embed.FS

// Filenames are recorded verbatim in schema_migrations.version, a
// VARCHAR(32) column — keep every migration FILENAME (including .sql) at 32
// characters or fewer, or the version insert fails after the DDL has already
// applied (implicit commit) and the next start crash-loops on the re-run.
// Since MySQL has no ADD COLUMN IF NOT EXISTS, write schema-changing
// migrations idempotently (see 0012_ai_reasoning.sql for the guarded
// prepared-statement pattern).

// Names is exported for tooling/tests that enumerate available migrations.
func Names() ([]string, error) {
	entries, err := FS.ReadDir(".")
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() {
			names = append(names, e.Name())
		}
	}
	return names, nil
}
