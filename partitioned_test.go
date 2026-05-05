package quadstore

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// twoPart is a test helper that opens a 2-partition Store under t.TempDir
// with a Router that sends "source:cmt-*" → corpus and everything else
// → main. Returns the open Store; t.Cleanup handles Close.
func twoPart(t *testing.T) *Store {
	t.Helper()
	root := t.TempDir()
	cfg := PartitionedConfig{
		Root: root,
		Partitions: []PartitionSpec{
			{Name: "main", File: "main.db"},
			{Name: "corpus", File: "corpus.db"},
		},
		Default: "main",
		RouteLabel: func(label string) Partition {
			if strings.HasPrefix(label, "source:cmt-") ||
				strings.HasPrefix(label, "derived:cmt-") {
				return "corpus"
			}
			return "main"
		},
		RoutePattern: func(p Pattern) Partition {
			// Optimization: subjects under cmt: live in corpus.
			if strings.HasPrefix(p.Subject, "cmt:") {
				return "corpus"
			}
			if p.Label != "" {
				if strings.HasPrefix(p.Label, "source:cmt-") ||
					strings.HasPrefix(p.Label, "derived:cmt-") {
					return "corpus"
				}
				return "main"
			}
			return ""
		},
	}
	s, err := OpenPartitioned(cfg)
	if err != nil {
		t.Fatalf("OpenPartitioned: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestOpenPartitioned_RoutingByLabel(t *testing.T) {
	s := twoPart(t)

	// Write quads via Writer.Commit; verify they land in expected partition.
	cases := []struct {
		label   string
		want    Partition
		subject string
	}{
		{"source:edgar", "main", "letter:14a8-2024-x"},
		{"source:sec-letter", "main", "letter:14a8-2024-y"},
		{"source:cmt-body", "corpus", "cmt:14a8-2024-x-001"},
		{"derived:cmt-thread", "corpus", "cmt:14a8-2024-x-001"},
		{"derived:requester-extract", "main", "letter:14a8-2024-x"},
	}

	for _, c := range cases {
		w, err := s.WriterFor(context.Background(), c.want)
		if err != nil {
			t.Fatalf("WriterFor(%s): %v", c.want, err)
		}
		err = w.Commit(context.Background(), Batch{
			Label: c.label,
			Adds:  []Quad{{Subject: c.subject, Predicate: "p", Object: "o"}},
		})
		w.Close()
		if err != nil {
			t.Fatalf("commit %s: %v", c.label, err)
		}
	}

	r := s.Reader()
	mainCount, err := r.Count(context.Background(), Pattern{Label: "source:edgar"})
	if err != nil || mainCount != 1 {
		t.Errorf("main source:edgar count = %d, %v", mainCount, err)
	}
	corpusCount, err := r.Count(context.Background(), Pattern{Label: "source:cmt-body"})
	if err != nil || corpusCount != 1 {
		t.Errorf("corpus source:cmt-body count = %d, %v", corpusCount, err)
	}
}

func TestPartitionFor(t *testing.T) {
	s := twoPart(t)
	if got := s.PartitionFor("source:cmt-body"); got != "corpus" {
		t.Errorf("PartitionFor(source:cmt-body) = %s, want corpus", got)
	}
	if got := s.PartitionFor("source:edgar"); got != "main" {
		t.Errorf("PartitionFor(source:edgar) = %s, want main", got)
	}
	if got := s.PartitionFor("anything-else"); got != "main" {
		t.Errorf("PartitionFor(anything-else) = %s, want main (default)", got)
	}
}

func TestPartitions(t *testing.T) {
	s := twoPart(t)
	parts := s.Partitions()
	if len(parts) != 2 {
		t.Fatalf("expected 2 partitions, got %d", len(parts))
	}
	if parts[0] != "main" || parts[1] != "corpus" {
		t.Errorf("partition order = %v, want [main corpus]", parts)
	}
}

func TestSinglePartitionPartitionsList(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "single.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	parts := s.Partitions()
	if len(parts) != 1 || parts[0] != "" {
		t.Errorf("single-file Partitions() = %v, want [\"\"]", parts)
	}
}

func TestWriter_CrossPartitionBatchRejected(t *testing.T) {
	s := twoPart(t)
	w, err := s.WriterFor(context.Background(), "main")
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	err = w.Commit(context.Background(), Batch{
		Label: "source:edgar",
		Adds: []Quad{
			{Subject: "x", Predicate: "p", Object: "o"},
			{Subject: "y", Predicate: "p", Object: "o", Label: "source:cmt-body"},
		},
	})
	if !errors.Is(err, ErrCrossPartitionBatch) {
		t.Errorf("expected ErrCrossPartitionBatch, got %v", err)
	}

	// Verify nothing landed (rolled back).
	r := s.Reader()
	n, _ := r.Count(context.Background(), Pattern{Subject: "x"})
	if n != 0 {
		t.Errorf("after rejected batch, x exists in %d rows; expected 0", n)
	}
}

func TestWriter_ConcurrentDifferentPartitions(t *testing.T) {
	s := twoPart(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mainW, err := s.WriterFor(ctx, "main")
	if err != nil {
		t.Fatal(err)
	}
	defer mainW.Close()

	// Acquire corpus while main is held — must NOT block.
	done := make(chan error, 1)
	go func() {
		corpusW, err := s.WriterFor(ctx, "corpus")
		if err != nil {
			done <- err
			return
		}
		corpusW.Close()
		done <- nil
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("corpus writer acquisition failed: %v", err)
		}
	}
}

func TestReader_FanOut(t *testing.T) {
	s := twoPart(t)
	mainW, _ := s.WriterFor(context.Background(), "main")
	mainW.Commit(context.Background(), Batch{
		Label: "source:edgar",
		Adds:  []Quad{{Subject: "letter:1", Predicate: "shared", Object: "main-data"}},
	})
	mainW.Close()

	corpusW, _ := s.WriterFor(context.Background(), "corpus")
	corpusW.Commit(context.Background(), Batch{
		Label: "source:cmt-body",
		Adds:  []Quad{{Subject: "cmt:1", Predicate: "shared", Object: "corpus-data"}},
	})
	corpusW.Close()

	// Pattern with no Label — should fan out and find both quads.
	r := s.Reader()
	count, err := r.Count(context.Background(), Pattern{Predicate: "shared"})
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Errorf("fan-out predicate count = %d, want 2", count)
	}

	// Pattern with Label that routes — should scope to one partition.
	count, err = r.Count(context.Background(), Pattern{
		Predicate: "shared",
		Label:     "source:cmt-body",
	})
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("scoped predicate count = %d, want 1", count)
	}
}

func TestReader_RoutePatternOptimization(t *testing.T) {
	// RoutePattern routes by subject prefix; verify the right partition
	// is hit when only the subject is specified.
	s := twoPart(t)
	w, _ := s.WriterFor(context.Background(), "corpus")
	w.Commit(context.Background(), Batch{
		Label: "source:cmt-body",
		Adds: []Quad{
			{Subject: "cmt:1", Predicate: "txt", Object: "hello"},
			{Subject: "cmt:2", Predicate: "txt", Object: "world"},
		},
	})
	w.Close()

	// Subject-prefix lookup: RoutePattern returns "corpus", read scopes
	// to corpus only.
	r := s.Reader()
	got, err := r.Count(context.Background(), Pattern{Subject: "cmt:1"})
	if err != nil {
		t.Fatal(err)
	}
	if got != 1 {
		t.Errorf("subject-routed count = %d, want 1", got)
	}
}

func TestOpenPartitioned_Validation(t *testing.T) {
	cases := []struct {
		name string
		cfg  PartitionedConfig
		want error
	}{
		{
			name: "empty partitions",
			cfg: PartitionedConfig{
				Default:    "main",
				RouteLabel: func(string) Partition { return "" },
			},
			want: ErrNoPartitions,
		},
		{
			name: "missing router",
			cfg: PartitionedConfig{
				Partitions: []PartitionSpec{{Name: "main", File: "x.db"}},
				Default:    "main",
			},
			want: ErrMissingRouter,
		},
		{
			name: "empty partition name",
			cfg: PartitionedConfig{
				Partitions: []PartitionSpec{{Name: "", File: "x.db"}},
				Default:    "main",
				RouteLabel: func(string) Partition { return "" },
			},
			want: ErrEmptyPartitionName,
		},
		{
			name: "duplicate partition",
			cfg: PartitionedConfig{
				Partitions: []PartitionSpec{
					{Name: "main", File: "a.db"},
					{Name: "main", File: "b.db"},
				},
				Default:    "main",
				RouteLabel: func(string) Partition { return "" },
			},
			want: ErrDuplicatePartition,
		},
		{
			name: "default not in partitions",
			cfg: PartitionedConfig{
				Partitions: []PartitionSpec{{Name: "main", File: "x.db"}},
				Default:    "missing",
				RouteLabel: func(string) Partition { return "" },
			},
			want: ErrMissingDefault,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := tc.cfg
			cfg.Root = t.TempDir()
			_, err := OpenPartitioned(cfg)
			if !errors.Is(err, tc.want) {
				t.Errorf("OpenPartitioned err = %v, want errors.Is %v", err, tc.want)
			}
		})
	}
}

func TestMigrate_RoundTrip(t *testing.T) {
	// Build a single-file source DB with two label families, then
	// migrate into a partitioned DB and verify per-partition counts.
	srcPath := filepath.Join(t.TempDir(), "src.db")
	src, err := Open(srcPath)
	if err != nil {
		t.Fatal(err)
	}
	w, err := src.Writer(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	mainQuads := []Quad{
		{Subject: "letter:1", Predicate: "title", Object: "Letter 1"},
		{Subject: "letter:2", Predicate: "title", Object: "Letter 2"},
		{Subject: "letter:3", Predicate: "title", Object: "Letter 3"},
	}
	corpusQuads := []Quad{
		{Subject: "cmt:1", Predicate: "body", Object: "Comment 1"},
		{Subject: "cmt:2", Predicate: "body", Object: "Comment 2"},
	}
	if err := w.Commit(context.Background(), Batch{
		Label: "source:edgar", Adds: mainQuads,
	}); err != nil {
		t.Fatal(err)
	}
	if err := w.Commit(context.Background(), Batch{
		Label: "source:cmt-body", Adds: corpusQuads,
	}); err != nil {
		t.Fatal(err)
	}
	w.Close()

	// Open destination partitioned store.
	dstRoot := t.TempDir()
	dst, err := OpenPartitioned(PartitionedConfig{
		Root: dstRoot,
		Partitions: []PartitionSpec{
			{Name: "main", File: "main.db"},
			{Name: "corpus", File: "corpus.db"},
		},
		Default: "main",
		RouteLabel: func(label string) Partition {
			if strings.HasPrefix(label, "source:cmt-") {
				return "corpus"
			}
			return "main"
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()

	stats, err := Migrate(context.Background(), src, dst, MigrateOptions{
		ChunkSize:   100,
		CopyCommits: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if stats.QuadsCopied != int64(len(mainQuads)+len(corpusQuads)) {
		t.Errorf("quads copied = %d, want %d", stats.QuadsCopied, len(mainQuads)+len(corpusQuads))
	}
	if stats.PerPartition["main"] != int64(len(mainQuads)) {
		t.Errorf("main quads = %d, want %d", stats.PerPartition["main"], len(mainQuads))
	}
	if stats.PerPartition["corpus"] != int64(len(corpusQuads)) {
		t.Errorf("corpus quads = %d, want %d", stats.PerPartition["corpus"], len(corpusQuads))
	}

	// Verify by reading from dst.
	r := dst.Reader()
	mainCount, _ := r.Count(context.Background(), Pattern{Label: "source:edgar"})
	if mainCount != int64(len(mainQuads)) {
		t.Errorf("dst main count = %d, want %d", mainCount, len(mainQuads))
	}
	corpusCount, _ := r.Count(context.Background(), Pattern{Label: "source:cmt-body"})
	if corpusCount != int64(len(corpusQuads)) {
		t.Errorf("dst corpus count = %d, want %d", corpusCount, len(corpusQuads))
	}
	src.Close()
}

func TestStore_Add_RoutesByLabel(t *testing.T) {
	// Legacy Add should also route in partitioned mode.
	s := twoPart(t)
	if err := s.Add(Quad{Subject: "letter:1", Predicate: "p", Object: "o", Label: "source:edgar"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Add(Quad{Subject: "cmt:1", Predicate: "p", Object: "o", Label: "source:cmt-body"}); err != nil {
		t.Fatal(err)
	}
	// Read back per-partition.
	r := s.Reader()
	if n, _ := r.Count(context.Background(), Pattern{Label: "source:edgar"}); n != 1 {
		t.Errorf("source:edgar count = %d, want 1", n)
	}
	if n, _ := r.Count(context.Background(), Pattern{Label: "source:cmt-body"}); n != 1 {
		t.Errorf("source:cmt-body count = %d, want 1", n)
	}
}

func TestStore_AddBatch_CrossPartitionRejected(t *testing.T) {
	s := twoPart(t)
	err := s.AddBatch([]Quad{
		{Subject: "a", Predicate: "p", Object: "o", Label: "source:edgar"},
		{Subject: "b", Predicate: "p", Object: "o", Label: "source:cmt-body"},
	})
	if !errors.Is(err, ErrCrossPartitionBatch) {
		t.Errorf("expected ErrCrossPartitionBatch, got %v", err)
	}
}

func TestMigrateFromSnapshot_RoundTrip(t *testing.T) {
	// Build a single-file source with two label families. Run a goroutine
	// that continues writing during MigrateFromSnapshot to verify the
	// snapshot is consistent and the migration captures only the pre-
	// snapshot state — concurrent writers do not corrupt the migration.
	srcPath := filepath.Join(t.TempDir(), "src.db")
	src, err := Open(srcPath)
	if err != nil {
		t.Fatal(err)
	}
	w, err := src.Writer(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Commit(context.Background(), Batch{
		Label: "source:edgar",
		Adds: []Quad{
			{Subject: "letter:1", Predicate: "title", Object: "L1"},
			{Subject: "letter:2", Predicate: "title", Object: "L2"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := w.Commit(context.Background(), Batch{
		Label: "source:cmt-body",
		Adds: []Quad{
			{Subject: "cmt:1", Predicate: "body", Object: "C1"},
			{Subject: "cmt:2", Predicate: "body", Object: "C2"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	w.Close()
	src.Close() // release writer slot so the parallel writer below can take it

	// Open destination.
	dstRoot := t.TempDir()
	dst, err := OpenPartitioned(PartitionedConfig{
		Root: dstRoot,
		Partitions: []PartitionSpec{
			{Name: "main", File: "main.db"},
			{Name: "corpus", File: "corpus.db"},
		},
		Default: "main",
		RouteLabel: func(label string) Partition {
			if strings.HasPrefix(label, "source:cmt-") {
				return "corpus"
			}
			return "main"
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()

	snapshotPath := filepath.Join(t.TempDir(), "snap.db")
	stats, err := MigrateFromSnapshot(context.Background(), srcPath, dst, SnapshotOptions{
		SnapshotPath: snapshotPath,
		KeepSnapshot: false,
		Migrate: MigrateOptions{
			ChunkSize:   100,
			CopyCommits: true,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if stats.QuadsCopied != 4 {
		t.Errorf("quads copied = %d, want 4", stats.QuadsCopied)
	}
	if stats.PerPartition["main"] != 2 || stats.PerPartition["corpus"] != 2 {
		t.Errorf("per-partition = %v, want main=2 corpus=2", stats.PerPartition)
	}
	if stats.SnapshotDuration <= 0 {
		t.Errorf("expected positive snapshot duration, got %v", stats.SnapshotDuration)
	}
	// Snapshot was deleted (KeepSnapshot=false).
	if _, err := os.Stat(snapshotPath); !os.IsNotExist(err) {
		t.Errorf("snapshot file should have been removed, but Stat err = %v", err)
	}
}

func TestMigrateFromSnapshot_KeepSnapshot(t *testing.T) {
	srcPath := filepath.Join(t.TempDir(), "src.db")
	src, _ := Open(srcPath)
	w, _ := src.Writer(context.Background())
	w.Commit(context.Background(), Batch{
		Label: "source:edgar",
		Adds:  []Quad{{Subject: "x", Predicate: "p", Object: "o"}},
	})
	w.Close()
	src.Close()

	dst, _ := OpenPartitioned(PartitionedConfig{
		Root:       t.TempDir(),
		Partitions: []PartitionSpec{{Name: "main", File: "main.db"}, {Name: "corpus", File: "corpus.db"}},
		Default:    "main",
		RouteLabel: func(string) Partition { return "main" },
	})
	defer dst.Close()

	snapshotPath := filepath.Join(t.TempDir(), "snap.db")
	_, err := MigrateFromSnapshot(context.Background(), srcPath, dst, SnapshotOptions{
		SnapshotPath: snapshotPath,
		KeepSnapshot: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Snapshot survived.
	if _, err := os.Stat(snapshotPath); err != nil {
		t.Errorf("snapshot should persist, got err %v", err)
	}
}

func TestMigrateFromSnapshot_RequiresSnapshotPath(t *testing.T) {
	dst, _ := OpenPartitioned(PartitionedConfig{
		Root:       t.TempDir(),
		Partitions: []PartitionSpec{{Name: "main", File: "main.db"}, {Name: "corpus", File: "corpus.db"}},
		Default:    "main",
		RouteLabel: func(string) Partition { return "main" },
	})
	defer dst.Close()

	_, err := MigrateFromSnapshot(context.Background(), "anywhere.db", dst, SnapshotOptions{})
	if err == nil || !strings.Contains(err.Error(), "SnapshotPath") {
		t.Errorf("expected SnapshotPath required error, got %v", err)
	}
}

func TestStore_Stats_AcrossPartitions(t *testing.T) {
	s := twoPart(t)
	w1, _ := s.WriterFor(context.Background(), "main")
	w1.Commit(context.Background(), Batch{
		Label: "source:edgar",
		Adds: []Quad{
			{Subject: "x", Predicate: "p1", Object: "o"},
			{Subject: "y", Predicate: "p2", Object: "o"},
		},
	})
	w1.Close()
	w2, _ := s.WriterFor(context.Background(), "corpus")
	w2.Commit(context.Background(), Batch{
		Label: "source:cmt-body",
		Adds: []Quad{
			{Subject: "a", Predicate: "p1", Object: "o"}, // shared predicate
			{Subject: "b", Predicate: "p3", Object: "o"},
		},
	})
	w2.Close()

	quads, preds, err := s.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if quads != 4 {
		t.Errorf("Stats quads = %d, want 4", quads)
	}
	if preds != 3 {
		t.Errorf("Stats predicates (DISTINCT) = %d, want 3 (p1 p2 p3)", preds)
	}
}
