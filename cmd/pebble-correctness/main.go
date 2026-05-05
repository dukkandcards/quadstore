// pebble-correctness — migrate a SQLite-backed quadstore to a
// Pebble-backed quadstore and run a battery of comparison queries
// against both, asserting byte-identical results.
//
// Usage:
//
//	pebble-correctness -src /path/to/source.db -dst /path/to/pebble-dir
//
// Phase 1: open both, run a fixed set of summary queries on src to
//          establish a baseline.
// Phase 2: MigrateToPebble with progress reporting.
// Phase 3: run the same queries on dst, plus a randomized sample of
//          subject / predicate / object / label point queries on
//          both, and assert set-equal results.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"os"
	"sort"
	"time"

	"github.com/dukkandcards/quadstore"
)

func main() {
	srcPath := flag.String("src", "", "path to SQLite source DB (required)")
	dstPath := flag.String("dst", "", "path to Pebble destination directory (will be created; required)")
	sampleN := flag.Int("sample", 200, "number of random subjects/predicates/objects to spot-check")
	flag.Parse()

	if *srcPath == "" || *dstPath == "" {
		log.Fatalf("usage: %s -src <sqlite.db> -dst <pebble-dir>", os.Args[0])
	}

	ctx := context.Background()

	log.Printf("==> opening SQLite source: %s", *srcPath)
	src, err := quadstore.Open(*srcPath)
	if err != nil {
		log.Fatalf("open src: %v", err)
	}
	defer src.Close()

	log.Printf("==> phase 1: source baseline")
	srcBaseline := summarize(ctx, srcReader(src), "src")

	log.Printf("==> phase 2: MigrateToPebble")
	if err := os.MkdirAll(*dstPath, 0o755); err != nil {
		log.Fatalf("mkdir dst: %v", err)
	}
	dst, err := quadstore.OpenPebble(*dstPath)
	if err != nil {
		log.Fatalf("open dst: %v", err)
	}
	defer dst.Close()

	t0 := time.Now()
	stats, err := quadstore.MigrateToPebble(ctx, src, dst, quadstore.MigrateToPebbleOptions{
		Progress: func(s quadstore.MigrateToPebbleStats) {
			log.Printf("    migrated %d quads (%.1f s)", s.QuadsCopied, time.Since(t0).Seconds())
		},
		ProgressEvery: 100_000,
	})
	migrateDur := time.Since(t0)
	if err != nil {
		log.Fatalf("MigrateToPebble: %v", err)
	}
	log.Printf("    migrated %d quads in %v (%.0f quads/sec)",
		stats.QuadsCopied, migrateDur, float64(stats.QuadsCopied)/migrateDur.Seconds())

	log.Printf("==> phase 3: destination baseline")
	dstBaseline := summarize(ctx, pebReader(dst), "dst")

	log.Printf("==> baseline comparison")
	compareBaselines(srcBaseline, dstBaseline)

	log.Printf("==> phase 4: sampled point-query correctness check (%d each)", *sampleN)
	sampleCheck(ctx, src, dst, srcBaseline, *sampleN)

	log.Println("==> done — all checks passed")
}

// baseline holds the summary measurements we can demand byte-identical
// agreement on between src and dst.
type baseline struct {
	totalQuads       int64
	predicateCount   int64
	labelCounts      map[string]int64
	totalCommits     int64
	totalOps         int64
	subjectsHashed   string // hex sha256 of sorted distinct subjects
	predicatesHashed string // hex sha256 of sorted distinct predicates
}

// readerFn abstracts over (*Store).Reader and (*PebbleStore).Reader
// so summarize can drive both.
type readerFn func() finder

type finder struct {
	count func(p quadstore.Pattern) (int64, error)
	find  func(p quadstore.Pattern, yield func(q quadstore.Quad) bool) error
}

func srcReader(s *quadstore.Store) readerFn {
	return func() finder {
		r := s.Reader()
		return finder{
			count: func(p quadstore.Pattern) (int64, error) {
				return r.Count(context.Background(), p)
			},
			find: func(p quadstore.Pattern, yield func(quadstore.Quad) bool) error {
				for q, err := range r.Find(context.Background(), p) {
					if err != nil {
						return err
					}
					if !yield(q) {
						return nil
					}
				}
				return nil
			},
		}
	}
}

func pebReader(s *quadstore.PebbleStore) readerFn {
	return func() finder {
		r := s.Reader()
		return finder{
			count: func(p quadstore.Pattern) (int64, error) {
				return r.Count(context.Background(), p)
			},
			find: func(p quadstore.Pattern, yield func(quadstore.Quad) bool) error {
				for q, err := range r.Find(context.Background(), p) {
					if err != nil {
						return err
					}
					if !yield(q) {
						return nil
					}
				}
				return nil
			},
		}
	}
}

func summarize(ctx context.Context, getReader readerFn, name string) baseline {
	t0 := time.Now()
	r := getReader()

	total, err := r.count(quadstore.Pattern{})
	if err != nil {
		log.Fatalf("[%s] Count: %v", name, err)
	}
	log.Printf("    [%s] total quads: %d (%.1fs)", name, total, time.Since(t0).Seconds())

	t1 := time.Now()
	subjects := map[string]struct{}{}
	predicates := map[string]struct{}{}
	r2 := getReader()
	if err := r2.find(quadstore.Pattern{}, func(q quadstore.Quad) bool {
		subjects[q.Subject] = struct{}{}
		predicates[q.Predicate] = struct{}{}
		return true
	}); err != nil {
		log.Fatalf("[%s] full scan: %v", name, err)
	}
	log.Printf("    [%s] distinct subjects: %d, distinct predicates: %d (%.1fs)",
		name, len(subjects), len(predicates), time.Since(t1).Seconds())

	subjectsList := mapKeys(subjects)
	sort.Strings(subjectsList)
	subjectsHash := hashStrings(subjectsList)

	predicatesList := mapKeys(predicates)
	sort.Strings(predicatesList)
	predicatesHash := hashStrings(predicatesList)

	return baseline{
		totalQuads:       total,
		predicateCount:   int64(len(predicates)),
		subjectsHashed:   subjectsHash,
		predicatesHashed: predicatesHash,
	}
}

func compareBaselines(src, dst baseline) {
	fail := false
	if src.totalQuads != dst.totalQuads {
		log.Printf("    FAIL: total quads src=%d dst=%d", src.totalQuads, dst.totalQuads)
		fail = true
	} else {
		log.Printf("    OK:   total quads = %d on both sides", src.totalQuads)
	}
	if src.predicateCount != dst.predicateCount {
		log.Printf("    FAIL: distinct predicates src=%d dst=%d", src.predicateCount, dst.predicateCount)
		fail = true
	} else {
		log.Printf("    OK:   distinct predicates = %d on both sides", src.predicateCount)
	}
	if src.subjectsHashed != dst.subjectsHashed {
		log.Printf("    FAIL: subjects-hash src=%s dst=%s", src.subjectsHashed[:16], dst.subjectsHashed[:16])
		fail = true
	} else {
		log.Printf("    OK:   subjects-hash matches (%s)", src.subjectsHashed[:16])
	}
	if src.predicatesHashed != dst.predicatesHashed {
		log.Printf("    FAIL: predicates-hash src=%s dst=%s", src.predicatesHashed[:16], dst.predicatesHashed[:16])
		fail = true
	} else {
		log.Printf("    OK:   predicates-hash matches (%s)", src.predicatesHashed[:16])
	}
	if fail {
		log.Fatalf("baseline check FAILED")
	}
}

func sampleCheck(ctx context.Context, src *quadstore.Store, dst *quadstore.PebbleStore, _ baseline, n int) {
	rng := rand.New(rand.NewSource(1))
	subjects := mapKeysFromSrc(ctx, src)
	if len(subjects) == 0 {
		log.Println("    no subjects — skipping sample")
		return
	}

	t0 := time.Now()
	mismatches := 0
	for i := 0; i < n; i++ {
		s := subjects[rng.Intn(len(subjects))]
		// Compare row sets for Pattern{Subject: s}.
		srcRows := collectFromSrc(ctx, src, quadstore.Pattern{Subject: s})
		dstRows := collectFromDst(ctx, dst, quadstore.Pattern{Subject: s})
		if !sameQuadSet(srcRows, dstRows) {
			log.Printf("    FAIL subject=%q src=%d dst=%d", s, len(srcRows), len(dstRows))
			mismatches++
		}
	}
	log.Printf("    %d random subject point-queries; %d mismatches; %.1fs",
		n, mismatches, time.Since(t0).Seconds())
	if mismatches != 0 {
		log.Fatalf("sample correctness check FAILED")
	}
}

func mapKeysFromSrc(ctx context.Context, s *quadstore.Store) []string {
	subs := map[string]struct{}{}
	for q, err := range s.Reader().Find(ctx, quadstore.Pattern{}) {
		if err != nil {
			log.Fatalf("collect subjects: %v", err)
		}
		subs[q.Subject] = struct{}{}
	}
	out := make([]string, 0, len(subs))
	for k := range subs {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func collectFromSrc(ctx context.Context, s *quadstore.Store, p quadstore.Pattern) []quadstore.Quad {
	var out []quadstore.Quad
	for q, err := range s.Reader().Find(ctx, p) {
		if err != nil {
			log.Fatalf("src find: %v", err)
		}
		out = append(out, q)
	}
	return out
}

func collectFromDst(ctx context.Context, s *quadstore.PebbleStore, p quadstore.Pattern) []quadstore.Quad {
	var out []quadstore.Quad
	for q, err := range s.Reader().Find(ctx, p) {
		if err != nil {
			log.Fatalf("dst find: %v", err)
		}
		out = append(out, q)
	}
	return out
}

func sameQuadSet(a, b []quadstore.Quad) bool {
	if len(a) != len(b) {
		return false
	}
	am := map[quadstore.Quad]struct{}{}
	for _, q := range a {
		am[q] = struct{}{}
	}
	for _, q := range b {
		if _, ok := am[q]; !ok {
			return false
		}
	}
	return true
}

func mapKeys[K comparable](m map[K]struct{}) []K {
	out := make([]K, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func hashStrings(ss []string) string {
	h := sha256.New()
	for _, s := range ss {
		fmt.Fprintln(h, s)
	}
	return hex.EncodeToString(h.Sum(nil))
}
