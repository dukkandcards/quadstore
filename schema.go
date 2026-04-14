package quadstore

import (
	"database/sql"
	"fmt"
)

// currentSchemaVersion is the schema the library writes.
// Increment and add a migration step when the schema changes.
const currentSchemaVersion = 2

// migrate creates or upgrades the schema to currentSchemaVersion.
// Idempotent forward. Fails loudly on downgrade.
//
// Schema metadata (version etc.) lives in a dedicated `meta` table,
// NOT as quads — storing it as quads would pollute the user's view
// of their data (Stats, Match, Shape, Path would all see it).
func migrate(db *sql.DB) error {
	// Base schema — quads + meta (always present).
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS quads (
			subject   TEXT NOT NULL,
			predicate TEXT NOT NULL,
			object    TEXT NOT NULL,
			label     TEXT NOT NULL DEFAULT '',
			UNIQUE(subject, predicate, object, label)
		);
		CREATE INDEX IF NOT EXISTS idx_spo ON quads(subject, predicate, object);
		CREATE INDEX IF NOT EXISTS idx_pos ON quads(predicate, object, subject);
		CREATE INDEX IF NOT EXISTS idx_osp ON quads(object, subject, predicate);
		CREATE INDEX IF NOT EXISTS idx_lsp ON quads(label, subject, predicate);

		CREATE TABLE IF NOT EXISTS meta (
			key   TEXT PRIMARY KEY,
			value TEXT NOT NULL
		);
	`); err != nil {
		return fmt.Errorf("quadstore: create base schema: %w", err)
	}

	version, err := readSchemaVersion(db)
	if err != nil {
		return err
	}
	if version > currentSchemaVersion {
		return fmt.Errorf(
			"quadstore: database schema version %d is newer than library version %d; refusing to downgrade",
			version, currentSchemaVersion,
		)
	}

	// v1 → v2: commits + commit_ops journal tables.
	if version < 2 {
		if _, err := db.Exec(`
			CREATE TABLE IF NOT EXISTS commits (
				id         TEXT PRIMARY KEY,
				created_at INTEGER NOT NULL,
				label      TEXT NOT NULL DEFAULT '',
				metadata   TEXT
			);
			CREATE INDEX IF NOT EXISTS idx_commits_created ON commits(created_at);

			CREATE TABLE IF NOT EXISTS commit_ops (
				commit_id TEXT NOT NULL REFERENCES commits(id),
				op        TEXT NOT NULL CHECK (op IN ('add', 'remove')),
				subject   TEXT NOT NULL,
				predicate TEXT NOT NULL,
				object    TEXT NOT NULL,
				label     TEXT NOT NULL DEFAULT ''
			);
			CREATE INDEX IF NOT EXISTS idx_commit_ops_commit ON commit_ops(commit_id);
			CREATE INDEX IF NOT EXISTS idx_commit_ops_quad ON commit_ops(subject, predicate, object, label);
		`); err != nil {
			return fmt.Errorf("quadstore: migrate v1→v2: %w", err)
		}
		if err := writeSchemaVersion(db, 2); err != nil {
			return err
		}
	}

	return nil
}

// readSchemaVersion returns the schema version from the meta table,
// or 0 if not present (legacy v1 DB without the marker).
func readSchemaVersion(db *sql.DB) (int, error) {
	var s string
	err := db.QueryRow(`SELECT value FROM meta WHERE key = ?`, "schema_version").Scan(&s)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("quadstore: read schema version: %w", err)
	}
	var v int
	if _, err := fmt.Sscanf(s, "%d", &v); err != nil {
		return 0, fmt.Errorf("quadstore: parse schema version %q: %w", s, err)
	}
	return v, nil
}

// writeSchemaVersion upserts the schema version in the meta table.
func writeSchemaVersion(db *sql.DB, v int) error {
	_, err := db.Exec(
		`INSERT INTO meta (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		"schema_version", fmt.Sprintf("%d", v),
	)
	if err != nil {
		return fmt.Errorf("quadstore: write schema version: %w", err)
	}
	return nil
}
