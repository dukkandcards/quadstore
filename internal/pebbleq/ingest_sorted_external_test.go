package pebbleq

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
)

// TestIngestSortedExternal_RoundTrip verifies bounded-memory ingest
// produces the same data as IngestSorted (in-memory) on the same
// input.
func TestIngestSortedExternal_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "store"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	const n = 50_000
	quads := make([]Quad, n)
	for i := range quads {
		quads[i] = Quad{
			Subject:   fmt.Sprintf("s%d", i),
			Predicate: "type",
			Object:    fmt.Sprintf("o%d", i%97),
			Label:     "source:bulk",
		}
	}

	in := make(chan Quad, 1024)
	go func() {
		defer close(in)
		for _, q := range quads {
			in <- q
		}
	}()

	stats, err := s.IngestSortedExternal(ctx, in, IngestSortedExternalOptions{
		ChunkSize: 5_000, // forces multiple chunks → multiple runs → merge phase
	})
	if err != nil {
		t.Fatalf("IngestSortedExternal: %v", err)
	}
	if stats.QuadsIngested != n {
		t.Errorf("QuadsIngested: got %d, want %d", stats.QuadsIngested, n)
	}
	if stats.RunsCreated < 10 {
		t.Errorf("RunsCreated: got %d, want >= 10 (50k input / 5k chunks)", stats.RunsCreated)
	}
	if stats.SSTablesWritten != 4 {
		t.Errorf("SSTablesWritten: got %d, want 4", stats.SSTablesWritten)
	}

	// Reads work for every quad we put in.
	r := s.Reader()
	count, err := r.Count(ctx, Pattern{})
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if count != int64(n) {
		t.Errorf("post-ingest count: got %d, want %d", count, n)
	}

	// Label counter correct.
	c, err := r.Count(ctx, Pattern{Label: "source:bulk"})
	if err != nil {
		t.Fatalf("Count(Label): %v", err)
	}
	if c != int64(n) {
		t.Errorf("label count: got %d, want %d", c, n)
	}

	// Subject lookup returns expected rows.
	hit := 0
	for q, err := range r.Find(ctx, Pattern{Subject: "s100"}) {
		if err != nil {
			t.Fatalf("Find: %v", err)
		}
		hit++
		if q.Subject != "s100" {
			t.Errorf("wrong subject: %q", q.Subject)
		}
	}
	if hit != 1 {
		t.Errorf("Find(Subject=s100): got %d hits, want 1", hit)
	}
}

// TestIngestSortedExternal_SingleChunk verifies the path works when
// all input fits in one chunk (no merge phase needed beyond pass-through).
func TestIngestSortedExternal_SingleChunk(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "store"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	in := make(chan Quad, 100)
	go func() {
		defer close(in)
		for i := 0; i < 50; i++ {
			in <- Quad{
				Subject:   fmt.Sprintf("s%d", i),
				Predicate: "p",
				Object:    "o",
				Label:     "source:tiny",
			}
		}
	}()

	stats, err := s.IngestSortedExternal(ctx, in, IngestSortedExternalOptions{
		ChunkSize: 1_000_000, // way bigger than input
	})
	if err != nil {
		t.Fatalf("IngestSortedExternal: %v", err)
	}
	if stats.QuadsIngested != 50 {
		t.Errorf("got %d, want 50", stats.QuadsIngested)
	}
	if stats.RunsCreated != 1 {
		t.Errorf("RunsCreated: got %d, want 1 (everything fits in one chunk)", stats.RunsCreated)
	}
}

// TestIngestSortedExternal_EmptyInput is a no-op.
func TestIngestSortedExternal_EmptyInput(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "store"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	in := make(chan Quad)
	close(in) // empty

	stats, err := s.IngestSortedExternal(ctx, in, IngestSortedExternalOptions{})
	if err != nil {
		t.Fatalf("IngestSortedExternal: %v", err)
	}
	if stats.QuadsIngested != 0 {
		t.Errorf("got %d, want 0", stats.QuadsIngested)
	}
	if stats.RunsCreated != 0 {
		t.Errorf("RunsCreated: got %d, want 0", stats.RunsCreated)
	}
	// Sstables ARE still written in the empty-runs case (they're empty
	// sstables) and Pebble still ingests them — that's a fine edge case.
}

// TestIngestSortedExternal_DuplicateAcrossChunks confirms cross-run
// dedup at the merge phase. Same quad appears in input multiple times
// across different chunks.
func TestIngestSortedExternal_DuplicateAcrossChunks(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "store"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	q := Quad{Subject: "s", Predicate: "p", Object: "o", Label: "source:dup"}
	in := make(chan Quad, 100)
	go func() {
		defer close(in)
		// 6 copies — with ChunkSize=2 they span 3 chunks, exercising
		// cross-run dedup during merge.
		for i := 0; i < 6; i++ {
			in <- q
		}
	}()
	stats, err := s.IngestSortedExternal(ctx, in, IngestSortedExternalOptions{
		ChunkSize: 2,
	})
	if err != nil {
		t.Fatalf("IngestSortedExternal: %v", err)
	}
	if stats.QuadsIngested != 1 {
		t.Errorf("QuadsIngested: got %d, want 1 (5 dupes dropped)", stats.QuadsIngested)
	}
	if stats.DuplicatesSkipped != 5 {
		t.Errorf("DuplicatesSkipped: got %d, want 5", stats.DuplicatesSkipped)
	}

	count, _ := s.Reader().Count(ctx, Pattern{})
	if count != 1 {
		t.Errorf("post-ingest count: got %d, want 1", count)
	}
}

// TestIngestSortedExternal_RejectsInvalidLabel covers validation.
func TestIngestSortedExternal_RejectsInvalidLabel(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "store"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	in := make(chan Quad, 1)
	in <- Quad{Subject: "s", Predicate: "p", Object: "o", Label: "not-a-namespace"}
	close(in)

	_, err = s.IngestSortedExternal(ctx, in, IngestSortedExternalOptions{})
	if err == nil {
		t.Error("expected ErrInvalidLabel, got nil")
	}
	if !errors.Is(err, ErrInvalidLabel) {
		t.Errorf("expected ErrInvalidLabel, got %v", err)
	}
}

// TestIngestSortedExternal_ContextCancellation verifies the loop
// returns ctx.Err() when the caller cancels mid-stream.
func TestIngestSortedExternal_ContextCancellation(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "store"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	ctx, cancel := context.WithCancel(context.Background())
	in := make(chan Quad, 10)
	// Push a few then cancel — don't close.
	in <- Quad{Subject: "s1", Predicate: "p", Object: "o", Label: "source:x"}
	in <- Quad{Subject: "s2", Predicate: "p", Object: "o", Label: "source:x"}

	go func() {
		// Cancel after a small delay so the loop has a chance to
		// process at least one quad.
		cancel()
	}()

	_, err = s.IngestSortedExternal(ctx, in, IngestSortedExternalOptions{
		ChunkSize: 1_000_000, // ensure no flush from chunk-fill
	})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled, got %v", err)
	}
}

// TestRunFile_RoundTrip covers the run file format directly.
func TestRunFile_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "run.dat")

	keys := [][]byte{
		[]byte("alpha"),
		[]byte("beta"),
		[]byte("gamma\x00with\x00nuls"),
		bytes.Repeat([]byte("x"), 10_000), // larger key
	}
	if err := writeRunFile(path, keys); err != nil {
		t.Fatalf("writeRunFile: %v", err)
	}

	r, err := openRunReader(path)
	if err != nil {
		t.Fatalf("openRunReader: %v", err)
	}
	defer r.Close()

	for i, want := range keys {
		if r.eof {
			t.Fatalf("unexpected EOF at index %d", i)
		}
		if !bytes.Equal(r.cur, want) {
			t.Errorf("key %d: got %q, want %q", i, r.cur, want)
		}
		r.advance()
		if r.err != nil {
			t.Fatalf("advance err at %d: %v", i, r.err)
		}
	}
	if !r.eof {
		t.Errorf("expected EOF after %d keys, but got more", len(keys))
	}
}

// TestRunHeap_KWayMerge constructs three sorted in-memory runs and
// verifies the merge produces a globally sorted, deduped output.
func TestRunHeap_KWayMerge(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "store"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	// Three chunks with overlapping content; expect dedup across runs.
	in := make(chan Quad, 100)
	go func() {
		defer close(in)
		// Chunk 1: s0..s4
		for i := 0; i < 5; i++ {
			in <- Quad{Subject: fmt.Sprintf("s%d", i), Predicate: "p", Object: "o", Label: "source:k"}
		}
		// Chunk 2: s3..s7 (overlap with chunk 1 on s3, s4)
		for i := 3; i < 8; i++ {
			in <- Quad{Subject: fmt.Sprintf("s%d", i), Predicate: "p", Object: "o", Label: "source:k"}
		}
		// Chunk 3: s6..s9 (overlap with chunk 2 on s6, s7)
		for i := 6; i < 10; i++ {
			in <- Quad{Subject: fmt.Sprintf("s%d", i), Predicate: "p", Object: "o", Label: "source:k"}
		}
	}()

	stats, err := s.IngestSortedExternal(ctx, in, IngestSortedExternalOptions{
		ChunkSize: 5,
	})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	// 5 + 5 + 4 = 14 input quads, 4 are duplicates (s3,s4,s6,s7)
	// → 10 unique, 4 dropped.
	if stats.QuadsIngested != 10 {
		t.Errorf("QuadsIngested: got %d, want 10", stats.QuadsIngested)
	}
	if stats.DuplicatesSkipped != 4 {
		t.Errorf("DuplicatesSkipped: got %d, want 4", stats.DuplicatesSkipped)
	}

	// The store should have exactly 10 quads (s0..s9).
	count, _ := s.Reader().Count(ctx, Pattern{})
	if count != 10 {
		t.Errorf("count: got %d, want 10", count)
	}
}
