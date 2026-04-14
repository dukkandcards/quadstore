package quadstore

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

func TestSchema_MigrateV1ToV2(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "legacy.db")

	// Create a v1-shaped DB directly (quads only, no meta/commits/commit_ops).
	db, err := sql.Open("sqlite", path+"?_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
		CREATE TABLE quads (
			subject   TEXT NOT NULL,
			predicate TEXT NOT NULL,
			object    TEXT NOT NULL,
			label     TEXT NOT NULL DEFAULT '',
			UNIQUE(subject, predicate, object, label)
		);
		INSERT INTO quads (subject, predicate, object, label) VALUES ('legacy', 'p', 'o', 'reference');
	`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	// Open via library — should migrate to v2 in place.
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open legacy v1 DB: %v", err)
	}
	defer s.Close()

	v, err := readSchemaVersion(s.db)
	if err != nil {
		t.Fatal(err)
	}
	if v != 2 {
		t.Errorf("expected version 2 after migration, got %d", v)
	}

	// commits + commit_ops tables exist.
	var name string
	if err := s.db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name='commits'`).Scan(&name); err != nil {
		t.Errorf("commits table missing: %v", err)
	}
	if err := s.db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name='commit_ops'`).Scan(&name); err != nil {
		t.Errorf("commit_ops table missing: %v", err)
	}

	// Legacy quad preserved and user-visible (not counted toward library meta).
	n, _, _ := s.Stats()
	if n != 1 {
		t.Errorf("expected legacy quad preserved (1), got Stats count %d", n)
	}

	// Legacy "reference" label still queryable via Match.
	it := s.Match("", "", "", "reference")
	count := 0
	for it.Next() {
		count++
	}
	it.Close()
	if count != 1 {
		t.Errorf("expected legacy reference-labeled quad still matchable, got %d", count)
	}
}

func TestSchema_DowngradeRefused(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "future.db")

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	// Poke a fake-future version into meta.
	if err := writeSchemaVersion(s.db, 99); err != nil {
		t.Fatal(err)
	}
	s.Close()

	_, err = Open(path)
	if err == nil {
		t.Error("expected error opening future-version DB")
	}
}

func TestSchema_MetaNotVisibleToUser(t *testing.T) {
	// Meta table holds schema_version; must NOT leak into quads views.
	s := tempStore(t)

	// Fresh store has schema_version=2 in meta, 0 quads visible.
	n, _, err := s.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("fresh store should have 0 user quads, got %d (meta leaked?)", n)
	}

	shape, err := s.Shape()
	if err != nil {
		t.Fatal(err)
	}
	if shape.NodeCount != 0 || len(shape.Edges) != 0 || len(shape.Predicates) != 0 {
		t.Errorf("fresh store Shape should be empty, got %+v", shape)
	}
}
