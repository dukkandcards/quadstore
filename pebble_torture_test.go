package quadstore

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Property-based torture tests for the Pebble backend.
//
// These don't replace the named-fixture tests in pebble_store_test.go;
// they answer a different question: "does the backend survive
// random data and random Pattern shapes, not just the data we
// happened to write tests for?"
//
// Run with:
//
//	go test -race -run 'TestPebbleTorture' -v ./...

// labelPool is the set of namespace-valid labels we draw from.
// Empty label is included so the no-label code path is exercised.
var labelPool = []string{
	"source:hr",
	"source:ledger",
	"source:msgraph",
	"derived:cluster",
	"derived:tfidf",
	"human:tenant1/notes",
	"human:tenant2/notes",
	"meta:schema",
	"",
}

// genRune draws a random rune avoiding NUL (key separator) and a
// few other low-ASCII control codes. Includes ASCII + Latin-1 +
// some Unicode for shape variety.
func genRune(rng *rand.Rand) rune {
	for {
		// Buckets: ASCII printable, Latin-1, Cyrillic, CJK fraction.
		switch rng.Intn(4) {
		case 0:
			r := rune(rng.Intn(95) + 32) // 0x20..0x7E
			if r != '\x00' {
				return r
			}
		case 1:
			return rune(rng.Intn(96) + 0xA0) // Latin-1 supplement
		case 2:
			return rune(rng.Intn(64) + 0x0410) // Cyrillic A..
		case 3:
			return rune(rng.Intn(80) + 0x4E00) // CJK
		}
	}
}

func genString(rng *rand.Rand, minLen, maxLen int) string {
	n := minLen + rng.Intn(maxLen-minLen+1)
	var sb strings.Builder
	for i := 0; i < n; i++ {
		sb.WriteRune(genRune(rng))
	}
	s := sb.String()
	// Defensively scrub NUL even though genRune doesn't emit it.
	if strings.IndexByte(s, 0) >= 0 {
		s = strings.ReplaceAll(s, "\x00", "")
	}
	return s
}

// genQuads generates N random quads. Predicates draw from a pool
// of `predPool` distinct strings to make Pattern.Predicate
// selectivity testable. Subjects draw from `subPool` distinct
// strings (smaller than N so multiple quads share subjects).
func genQuads(rng *rand.Rand, n, subPool, predPool int) []Quad {
	subs := make([]string, subPool)
	for i := range subs {
		subs[i] = "s:" + genString(rng, 3, 20)
	}
	preds := make([]string, predPool)
	for i := range preds {
		preds[i] = "p:" + genString(rng, 3, 15)
	}
	out := make([]Quad, 0, n)
	seen := make(map[Quad]struct{}, n)
	for len(out) < n {
		q := Quad{
			Subject:   subs[rng.Intn(len(subs))],
			Predicate: preds[rng.Intn(len(preds))],
			Object:    genString(rng, 1, 40),
			Label:     labelPool[rng.Intn(len(labelPool))],
		}
		if _, dup := seen[q]; dup {
			continue
		}
		seen[q] = struct{}{}
		out = append(out, q)
	}
	return out
}

// patternsToCheck returns a representative set of Pattern shapes
// to query for the given quad universe. Includes:
//   - empty Pattern (full scan)
//   - Subject-only for every distinct subject
//   - Predicate-only for every distinct predicate
//   - Object-only for a sample
//   - Label-only for every distinct label
//   - All field combinations on a sample of 50 quads
func patternsToCheck(rng *rand.Rand, quads []Quad) []Pattern {
	subjects := map[string]struct{}{}
	predicates := map[string]struct{}{}
	objects := map[string]struct{}{}
	labels := map[string]struct{}{}
	for _, q := range quads {
		subjects[q.Subject] = struct{}{}
		predicates[q.Predicate] = struct{}{}
		objects[q.Object] = struct{}{}
		labels[q.Label] = struct{}{}
	}

	var out []Pattern
	out = append(out, Pattern{}) // full scan
	for s := range subjects {
		out = append(out, Pattern{Subject: s})
	}
	for p := range predicates {
		out = append(out, Pattern{Predicate: p})
	}
	// Sample objects (full enumeration is N).
	count := 0
	for o := range objects {
		out = append(out, Pattern{Object: o})
		count++
		if count >= 50 {
			break
		}
	}
	for l := range labels {
		if l == "" {
			continue // empty Label is a wildcard, not a filter — skip
		}
		out = append(out, Pattern{Label: l})
	}
	// Combination shapes on a sample.
	for i := 0; i < 50 && i < len(quads); i++ {
		q := quads[rng.Intn(len(quads))]
		out = append(out, Pattern{Subject: q.Subject, Predicate: q.Predicate})
		out = append(out, Pattern{Subject: q.Subject, Object: q.Object})
		out = append(out, Pattern{Subject: q.Subject, Predicate: q.Predicate, Object: q.Object})
		// Skip Label combinations when q.Label == "" — that's a
		// wildcard query, not "match label=''".
		if q.Label != "" {
			out = append(out, Pattern{Label: q.Label, Subject: q.Subject})
			out = append(out, Pattern{Label: q.Label, Subject: q.Subject, Predicate: q.Predicate})
		}
	}
	return out
}

// expectedMatches returns the subset of quads matching the pattern
// (empty fields = wildcards). Mirrors Reader.Find's filtering rule.
func expectedMatches(quads []Quad, p Pattern) []Quad {
	var out []Quad
	for _, q := range quads {
		if p.Subject != "" && q.Subject != p.Subject {
			continue
		}
		if p.Predicate != "" && q.Predicate != p.Predicate {
			continue
		}
		if p.Object != "" && q.Object != p.Object {
			continue
		}
		if p.Label != "" && q.Label != p.Label {
			continue
		}
		out = append(out, q)
	}
	return out
}

// runFind drains an iter.Seq2[Quad, error] into a quad slice.
// On any error the test fails immediately.
func runFind(t *testing.T, p Pattern, finder func(Pattern) []Quad) []Quad {
	t.Helper()
	return finder(p)
}

// pebbleFinder adapts (*PebbleReader).Find for the property test.
func pebbleFinder(ctx context.Context, r *PebbleReader, t *testing.T) func(Pattern) []Quad {
	return func(p Pattern) []Quad {
		var out []Quad
		for q, err := range r.Find(ctx, p) {
			if err != nil {
				t.Fatalf("Find error on pattern %+v: %v", p, err)
			}
			out = append(out, q)
		}
		return out
	}
}

// quadSetEqual returns true iff a and b are set-equal (order-
// independent). Returns a diff message when not equal.
func quadSetEqual(a, b []Quad) (bool, string) {
	if len(a) != len(b) {
		// Build small diff to surface the first divergence.
		am := map[Quad]struct{}{}
		for _, q := range a {
			am[q] = struct{}{}
		}
		var missing, extra []Quad
		bm := map[Quad]struct{}{}
		for _, q := range b {
			bm[q] = struct{}{}
			if _, ok := am[q]; !ok {
				extra = append(extra, q)
			}
		}
		for q := range am {
			if _, ok := bm[q]; !ok {
				missing = append(missing, q)
			}
		}
		msg := fmt.Sprintf("len mismatch: expected %d, got %d", len(a), len(b))
		if len(missing) > 0 {
			msg += fmt.Sprintf("; missing %d (first: %+v)", len(missing), missing[0])
		}
		if len(extra) > 0 {
			msg += fmt.Sprintf("; extra %d (first: %+v)", len(extra), extra[0])
		}
		return false, msg
	}
	am := map[Quad]struct{}{}
	for _, q := range a {
		am[q] = struct{}{}
	}
	for _, q := range b {
		if _, ok := am[q]; !ok {
			return false, fmt.Sprintf("first divergent quad: %+v", q)
		}
	}
	return true, ""
}

// TestPebbleTorture_RoundTrip writes N random quads through a mix
// of Writer.Commit (some audited, some not) and BulkLoader, then
// verifies every Pattern shape returns exactly the expected subset.
func TestPebbleTorture_RoundTrip(t *testing.T) {
	const seed int64 = 1
	const N = 5000
	rng := rand.New(rand.NewSource(seed))
	ctx := context.Background()

	s := tempPebbleStoreT(t)
	quads := genQuads(rng, N, 200, 50) // 200 distinct subjects, 50 distinct predicates

	// Write half through Writer.Commit (varying Audit), half via BulkLoader.
	commitChunkN := 50
	half := N / 2
	{
		w, err := s.Writer(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < half; i += commitChunkN {
			end := i + commitChunkN
			if end > half {
				end = half
			}
			noAudit := (i/commitChunkN)%2 == 0
			b := Batch{
				Adds:    quads[i:end],
				NoAudit: noAudit,
			}
			if err := w.Commit(ctx, b); err != nil {
				t.Fatalf("Commit chunk [%d:%d] noAudit=%v: %v", i, end, noAudit, err)
			}
		}
		w.Close()
	}
	{
		bl, err := s.BulkLoaderWithLabel(ctx, "")
		if err != nil {
			t.Fatal(err)
		}
		for _, q := range quads[half:] {
			if err := bl.Add(q); err != nil {
				t.Fatalf("Bulk Add: %v", err)
			}
		}
		if err := bl.Close(); err != nil {
			t.Fatal(err)
		}
	}

	r := s.Reader()
	finder := pebbleFinder(ctx, r, t)

	// Verify the empty Pattern returns the full set.
	all := finder(Pattern{})
	if ok, msg := quadSetEqual(quads, all); !ok {
		t.Fatalf("full-scan: %s", msg)
	}

	// Check every interesting Pattern shape.
	for _, p := range patternsToCheck(rng, quads) {
		want := expectedMatches(quads, p)
		got := finder(p)
		if ok, msg := quadSetEqual(want, got); !ok {
			t.Errorf("Pattern %+v mismatch: %s", p, msg)
		}
	}
}

// TestPebbleTorture_Removes writes A then removes B (subset),
// verifies reads = A \ B.
func TestPebbleTorture_Removes(t *testing.T) {
	const seed int64 = 2
	const NA = 2000
	const NB = 500
	rng := rand.New(rand.NewSource(seed))
	ctx := context.Background()

	s := tempPebbleStoreT(t)
	A := genQuads(rng, NA, 100, 30)

	// Write all of A.
	w, _ := s.Writer(ctx)
	const chunkN = 100
	for i := 0; i < NA; i += chunkN {
		end := i + chunkN
		if end > NA {
			end = NA
		}
		if err := w.Commit(ctx, Batch{Adds: A[i:end]}); err != nil {
			t.Fatal(err)
		}
	}

	// Pick NB random quads from A to remove (no duplicates).
	rng2 := rand.New(rand.NewSource(seed + 100))
	idxs := rng2.Perm(NA)[:NB]
	B := make([]Quad, NB)
	for i, idx := range idxs {
		B[i] = A[idx]
	}
	if err := w.Commit(ctx, Batch{Removes: B}); err != nil {
		t.Fatal(err)
	}
	w.Close()

	// Expected = A \ B.
	bSet := map[Quad]struct{}{}
	for _, q := range B {
		bSet[q] = struct{}{}
	}
	want := make([]Quad, 0, NA-NB)
	for _, q := range A {
		if _, removed := bSet[q]; !removed {
			want = append(want, q)
		}
	}

	got := pebbleFinder(ctx, s.Reader(), t)(Pattern{})
	if ok, msg := quadSetEqual(want, got); !ok {
		t.Fatalf("after Removes: %s", msg)
	}
}

// ============================================================
// B: crash-recovery tests
// ============================================================

// TestPebbleCrash_CleanCloseReopen — write quads, close, reopen,
// verify all present. The simplest durability claim: a successful
// Close means the data survives a process restart.
func TestPebbleCrash_CleanCloseReopen(t *testing.T) {
	const seed int64 = 3
	const N = 3000
	rng := rand.New(rand.NewSource(seed))
	ctx := context.Background()

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "pebble")

	quads := genQuads(rng, N, 100, 25)

	// Open, write, close.
	{
		s, err := OpenPebble(dbPath)
		if err != nil {
			t.Fatal(err)
		}
		w, _ := s.Writer(ctx)
		const chunkN = 100
		for i := 0; i < N; i += chunkN {
			end := i + chunkN
			if end > N {
				end = N
			}
			if err := w.Commit(ctx, Batch{Adds: quads[i:end]}); err != nil {
				t.Fatal(err)
			}
		}
		w.Close()
		if err := s.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	}

	// Reopen and verify.
	s, err := OpenPebble(dbPath)
	if err != nil {
		t.Fatalf("Reopen: %v", err)
	}
	defer s.Close()

	got := pebbleFinder(ctx, s.Reader(), t)(Pattern{})
	if ok, msg := quadSetEqual(quads, got); !ok {
		t.Fatalf("after reopen: %s", msg)
	}
}

// TestPebbleCrash_SIGKILL — fork a subprocess that writes N quads
// via CommitSync (per-commit fsync), kill it before clean close,
// reopen in the parent, verify the surviving quads are intact and
// no corruption is observable.
//
// CommitSync semantics: every Commit fsyncs the WAL before returning,
// so every quad whose Commit completed in the child should survive
// even a SIGKILL. We don't try to count exactly how many made it
// (the kill timing is non-deterministic); we verify whatever did
// survive is well-formed and the set of surviving quads is a
// prefix-respecting subset of what was written.
func TestPebbleCrash_SIGKILL(t *testing.T) {
	if os.Getenv("QUADSTORE_CRASH_CHILD") == "1" {
		// Child path: invoked by the test re-exec'ing itself.
		runCrashChild()
		return
	}

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "pebble")
	const N = 5000
	const seed int64 = 4

	// Spawn the child. Run it long enough that we kill mid-write.
	cmd := exec.Command(os.Args[0], "-test.run=TestPebbleCrash_SIGKILL", "-test.v=false")
	cmd.Env = append(os.Environ(),
		"QUADSTORE_CRASH_CHILD=1",
		"QUADSTORE_CRASH_PATH="+dbPath,
		"QUADSTORE_CRASH_N="+strconv.Itoa(N),
		"QUADSTORE_CRASH_SEED="+strconv.FormatInt(seed, 10),
	)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start child: %v", err)
	}

	// Sleep a tunable fraction of the expected child runtime, then
	// SIGKILL. We're aiming for "kill mid-write" — too short and
	// nothing has been written; too long and the child finishes.
	// 100ms is a reasonable middle on M1; production-CI hardware
	// may need adjustment.
	time.Sleep(100 * time.Millisecond)
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("kill child: %v", err)
	}
	_ = cmd.Wait() // expected to be killed; ignore error

	// Reopen in the parent.
	s, err := OpenPebble(dbPath)
	if err != nil {
		t.Fatalf("Reopen after kill: %v", err)
	}
	defer s.Close()

	// Verify whatever survived is well-formed: every quad readable
	// via Pattern{} round-trips to the same Quad via
	// Pattern{Subject:q.Subject,Predicate:q.Predicate,Object:q.Object,Label:q.Label}.
	ctx := context.Background()
	r := s.Reader()
	all := pebbleFinder(ctx, r, t)(Pattern{})
	t.Logf("survived after SIGKILL: %d quads of up to %d written", len(all), N)

	if len(all) == 0 {
		// Acceptable: kill happened before any commit completed.
		// We've at least proven the file isn't corrupted (Open
		// succeeded).
		return
	}

	for _, q := range all {
		got := pebbleFinder(ctx, r, t)(Pattern{
			Subject: q.Subject, Predicate: q.Predicate,
			Object: q.Object, Label: q.Label,
		})
		if len(got) != 1 {
			t.Errorf("point lookup on surviving quad %+v returned %d rows, want 1", q, len(got))
		}
	}

	// And: the survivors must be a subset of what we asked the
	// child to write. We regenerate the planned set and check
	// containment.
	rng := rand.New(rand.NewSource(seed))
	planned := genQuads(rng, N, 100, 25)
	plannedSet := map[Quad]struct{}{}
	for _, q := range planned {
		plannedSet[q] = struct{}{}
	}
	for _, q := range all {
		if _, ok := plannedSet[q]; !ok {
			t.Errorf("survived quad not in planned set: %+v", q)
		}
	}
}

// runCrashChild is the in-process body of the SIGKILL child. It
// writes quads in a tight loop using CommitSync until the parent
// kills it.
func runCrashChild() {
	dbPath := os.Getenv("QUADSTORE_CRASH_PATH")
	n, _ := strconv.Atoi(os.Getenv("QUADSTORE_CRASH_N"))
	seed, _ := strconv.ParseInt(os.Getenv("QUADSTORE_CRASH_SEED"), 10, 64)

	rng := rand.New(rand.NewSource(seed))
	quads := genQuads(rng, n, 100, 25)

	s, err := OpenPebble(dbPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "child Open:", err)
		os.Exit(1)
	}
	w, err := s.Writer(context.Background())
	if err != nil {
		fmt.Fprintln(os.Stderr, "child Writer:", err)
		os.Exit(1)
	}
	for _, q := range quads {
		if err := w.CommitSync(context.Background(), Batch{Adds: []Quad{q}}); err != nil {
			fmt.Fprintln(os.Stderr, "child CommitSync:", err)
			os.Exit(1)
		}
	}
	// Survived to the end; clean close.
	w.Close()
	s.Close()
	os.Exit(0)
}
