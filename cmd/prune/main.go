// Command prune applies a commit-journal retention policy to a
// quadstore. It deletes rows from commit_ops for commits older than the
// cutoff; the commits rows themselves (provenance metadata) are preserved.
// The current-state quads table is unaffected.
//
// Two cutoff modes:
//
//	--older-than 90d    duration relative to now (default, recommended for
//	                    scheduled maintenance)
//	--before 2026-04-20 absolute date (YYYY-MM-DD, UTC); useful for a
//	                    one-time aggressive sweep after a bulk regen
//
// By default the command only reports what *would* be deleted. Pass
// --apply to actually run the DELETE. Pass --vacuum to run VACUUM after
// (reclaims disk space; requires free disk equal to current DB size).
//
// Example:
//
//	prune --db /path/to/db --older-than 90d --apply --vacuum
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"regexp"
	"strconv"
	"time"

	"github.com/dukkandcards/quadstore"
)

func main() {
	dbPath := flag.String("db", "", "quadstore path (required)")
	olderThan := flag.String("older-than", "90d", "prune ops for commits older than this duration from now (e.g., 90d, 24h, 14d)")
	beforeStr := flag.String("before", "", "absolute cutoff (YYYY-MM-DD, UTC); overrides --older-than if set")
	apply := flag.Bool("apply", false, "actually delete (default: dry run)")
	doVacuum := flag.Bool("vacuum", false, "run VACUUM after delete (requires free disk == DB size)")
	flag.Parse()

	if *dbPath == "" {
		flag.Usage()
		os.Exit(2)
	}

	cutoff, err := resolveCutoff(*olderThan, *beforeStr)
	if err != nil {
		log.Fatalf("cutoff: %v", err)
	}

	s, err := quadstore.Open(*dbPath)
	if err != nil {
		log.Fatalf("open: %v", err)
	}
	defer s.Close()

	ctx := context.Background()
	fi, _ := os.Stat(*dbPath)

	pre, err := s.CommitStatsAt(cutoff)
	if err != nil {
		log.Fatalf("stat: %v", err)
	}
	fmt.Printf("db: %s", *dbPath)
	if fi != nil {
		fmt.Printf("  (%.1f MB)", float64(fi.Size())/(1<<20))
	}
	fmt.Println()
	fmt.Printf("cutoff: %s (UTC)\n", cutoff.UTC().Format(time.RFC3339))
	fmt.Printf("commits:    total=%d  older-than-cutoff=%d\n", pre.TotalCommits, pre.OldCommits)
	fmt.Printf("commit_ops: total=%d  eligible=%d\n", pre.TotalOps, pre.OldOps)

	if !*apply {
		fmt.Println("\n(dry run — pass --apply to execute)")
		return
	}

	w, err := s.Writer(ctx)
	if err != nil {
		log.Fatalf("writer: %v", err)
	}
	t0 := time.Now()
	deleted, err := w.PruneOps(ctx, cutoff)
	w.Close()
	if err != nil {
		log.Fatalf("prune: %v", err)
	}
	fmt.Printf("\ndeleted %d commit_ops rows in %s\n", deleted, time.Since(t0).Round(time.Millisecond))

	if *doVacuum {
		fmt.Println("running VACUUM ...")
		t0 := time.Now()
		if err := s.Vacuum(); err != nil {
			log.Fatalf("vacuum: %v", err)
		}
		fi2, _ := os.Stat(*dbPath)
		fmt.Printf("vacuum complete in %s", time.Since(t0).Round(time.Second))
		if fi != nil && fi2 != nil {
			fmt.Printf(" — %.1f MB → %.1f MB (%.1f MB reclaimed)",
				float64(fi.Size())/(1<<20),
				float64(fi2.Size())/(1<<20),
				float64(fi.Size()-fi2.Size())/(1<<20),
			)
		}
		fmt.Println()
	} else {
		fmt.Println("(skipped VACUUM — pass --vacuum to reclaim disk space)")
	}
}

// parseDuration extends stdlib time.ParseDuration with "d" (days).
func parseDuration(s string) (time.Duration, error) {
	re := regexp.MustCompile(`^(\d+)d$`)
	if m := re.FindStringSubmatch(s); m != nil {
		days, err := strconv.Atoi(m[1])
		if err != nil {
			return 0, err
		}
		return time.Duration(days) * 24 * time.Hour, nil
	}
	return time.ParseDuration(s)
}

func resolveCutoff(olderThan, before string) (time.Time, error) {
	if before != "" {
		t, err := time.Parse("2006-01-02", before)
		if err != nil {
			return time.Time{}, fmt.Errorf("parse --before %q: %w", before, err)
		}
		return t, nil
	}
	d, err := parseDuration(olderThan)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse --older-than %q: %w", olderThan, err)
	}
	return time.Now().Add(-d), nil
}
