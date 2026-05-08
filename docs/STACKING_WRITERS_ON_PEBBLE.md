# Stacking Writers on quadstore's Pebble Backend

> Application architecture patterns when you're using
> `quadstore.OpenPebble` and need multiple sources writing into the
> same store.

**Audience:** application authors using
[`github.com/dukkandcards/quadstore`](https://github.com/dukkandcards/quadstore).
You've chosen (or are considering) the Pebble backend over the
SQLite backend, and you have multiple cron jobs / writer binaries /
admin tools that all need to write into the store.

**Why this doc exists:** the SQLite backend (`quadstore.Open`) lets
multiple processes share the database file via WAL mode. The Pebble
backend (`quadstore.OpenPebble`) does not — Pebble holds an
exclusive OS file lock on the data directory and refuses a second
opener. **Migrating from SQLite to Pebble forces a writer-architecture
decision that's invisible until you flip the env var.**

**Status:** distilled from production experience operating SecDek
through a SQLite → Pebble cutover, 24-hour rollback, and side-
environment validation in May 2026.

---

## TL;DR

The quadstore backend choice is more than a perf knob. **Pebble
constrains your application's process topology in a way SQLite does
not.** Pick one of three patterns before flipping `SECDEK_DB_BACKEND`
(or whatever your equivalent is):

1. **In-process scheduler** — fold every writer into the server
   binary; goroutines on a Go-side timer. (recommended default)
2. **Admin-endpoint writers** — server exposes auth-gated HTTP
   endpoints; external writers POST batches.
3. **File-feed via spool directory** — writers drop JSON files;
   server consumes them. (best for bulk imports)

Patterns that worked on SQLite (multiple binaries opening the file
via WAL) **do not work** on Pebble. Plan accordingly.

---

## The constraint

`quadstore.OpenPebble(dir)` calls Pebble's `pebble.Open(dir, ...)`,
which acquires an `flock(2)`-style exclusive lock on `<dir>/LOCK`.
The lock survives until the process exits or `(*PebbleStore).Close()`
runs. Any second `OpenPebble` on the same dir from a different
process gets:

```
pebble: resource temporarily unavailable
```

Same with the `quadstore-cli` debugger, ingest tools, etc. — if
they all call `OpenPebble`, the second one fails.

**By contrast**, the SQLite backend (`quadstore.Open`) uses SQLite's
WAL mode, which lets multiple processes coordinate via the WAL file.
SQLite serializes writes internally and lets reads proceed
concurrently. Three or four binaries all calling `quadstore.Open`
on the same path is fine.

This is not a bug in either backend. They're different engines with
different ACID semantics. The lesson is just: **your application's
process topology has to match the backend's lock model**.

---

## Pattern 1: In-process scheduler (recommended)

Fold every recurring writer into the server process. The server
holds the Pebble lock for its lifetime; ingest, periodic refresh,
backups all run as goroutines on a Go-side cron.

```
+--------------------------------------------+
|  cmd/server                                |
|  +------------------------------------+    |
|  | HTTP handlers (reads + occasional  |    |
|  |  writes via shared *quadstore.PebbleStore) |
|  +-----------------+------------------+    |
|                    |                       |
|  +-----------------v------------------+    |
|  | scheduler — goroutine per Job      |    |
|  |  - ingest (every 15 min)           |    |
|  |  - daily refresh (06:00 UTC)       |    |
|  |  - weekly refresh (Sun 03:00 UTC)  |    |
|  +-----------------+------------------+    |
|                    |                       |
|         shared *quadstore.PebbleStore      |
+--------------------------------------------+
```

**Pros:**
- One process, one open. Simplest ownership story; matches the
  embedded-DB ethos.
- Pebble's internal writer mutex serializes goroutines correctly.
  Measured in production: two concurrent writers (one bulk
  ingestion + one fast tick) under realistic load incurred only
  **+16-25% contention overhead** with no deadlock, no corruption.
- Shared in-memory caches (corpus indices, materialized views)
  available to every Job without round-tripping the DB.
- Single Sentry tag, single log stream, single set of metrics.

**Cons:**
- Server process becomes the single failure surface for the
  scheduler. Mitigate via `Restart=on-failure` in systemd; design
  Jobs to be idempotent so a missed/repeated tick is safe.
- Resource limits (`MemoryMax`, CPU quota) span web traffic + bulk
  jobs.

**When to use:** default. ~90% of applications migrating from
SQLite-era multi-binary stacks should use this.

**Skeleton (Go-side):**

```go
// internal/scheduler/scheduler.go
type Job interface {
    Name() string
    Run(ctx context.Context) error
}

type Schedule struct {
    Job     Job
    Cadence Cadence  // implementations: Every, Daily, Weekly
    Timeout time.Duration
}

func (s *Scheduler) Register(sch Schedule) { ... }
func (s *Scheduler) Run(ctx context.Context) error { /* blocks; runs each Job on cadence */ }

// cmd/server/main.go
db, _ := quadstore.OpenPebble(path)
defer db.Close()

sched := scheduler.New(slog.Default(), sentryCapture)
sched.Register(scheduler.Schedule{
    Job:     &ingestJob{DB: db},
    Cadence: scheduler.Every(15 * time.Minute),
    Timeout: 5 * time.Minute,
})
go sched.Run(ctx)

http.ListenAndServeTLS(...)
```

The Job's `Run(ctx)` does ALL the writes via `db.Writer(ctx).Commit(...)`.
No CLI binary opens the store separately.

---

## Pattern 2: Admin-endpoint writers

Keep external writer binaries, but have them route through
authenticated HTTP endpoints on the server. The server holds the
lock and applies the writes; external binaries become thin clients.

```
                              POST /admin/quads (Bearer auth)
+----------+  +----------+  +-------v---------------------------+
| cron 1   |  | cron 2   |  |   cmd/server                      |
| (worker) |->| (worker) |->| (handler executes the write       |
+----------+  +----------+  |   against shared *PebbleStore)    |
                            +-----------------------------------+
                                          |
                                  *quadstore.PebbleStore (one open)
```

The classic shape:

- `POST /admin/quads` — body is a `quadstore.Batch`; server commits.
- `POST /admin/sentinel-filter` — request: candidate subjects;
  response: which are new (server reads via `Reader.Find`).
- `POST /admin/checkpoint?dest=...` — server calls
  `(*PebbleStore).Checkpoint(dest)` while it holds the lock; client
  receives a pointer to a hardlink-backed snapshot dir for backup.

**Pros:**
- Existing writer binaries stay; smaller Go-side refactor than
  Pattern 1.
- Auth boundary is clean (Bearer token + IP allowlist).
- Independent deployment of writers; server can be redeployed
  without touching writers (modulo API compatibility).

**Cons:**
- HTTP body-size limits + serialization cost per batch. Fine for
  thousands-of-rows-per-tick; questionable for millions.
- Two surfaces to keep in sync (writer code + server endpoint).
- Auth-token rotation becomes multi-touchpoint.

**When to use:** when the writer binaries are existing, large, and
you don't want to fold them into the server. Or when writers run
on different machines than the server (rare for embedded-DB apps
but plausible for edge-ingest topologies).

---

## Pattern 3: File-feed via spool directory

Writers produce JSON-Lines (or similar) files into a feed directory.
The server has a goroutine watcher that picks up "ready" files,
applies them via `(*PebbleStore).IngestSortedExternal` or
`Writer.Commit`, then deletes or archives.

```
+----------+   atomic rename            +----------------+
| cron 1   |--->/spool/<uuid>.ready --->| watcher        |
+----------+                            | goroutine      |
+----------+                            | (server proc)  |
| cron 2   |--->/spool/<uuid>.ready --->|                |
+----------+                            +-------+--------+
                                                |
                                    *quadstore.PebbleStore (one open)
```

**Key invariants:**

- Writer writes to `<dir>/<uuid>.tmp`, then `mv` to `<uuid>.ready`
  (atomic on the same filesystem). Crashes mid-write leave only
  `.tmp` files; the watcher ignores those.
- Watcher polls every N seconds. Picks oldest `.ready`, reads,
  applies, deletes on success or moves to `<dir>-archive/` on
  failure.
- Failure mode is captured (the file persists for inspection); the
  next watcher tick continues with the next `.ready`.

**Pros:**
- Fully decoupled writers; no HTTP dependency, no auth surface, no
  body-size limit.
- Bulk-friendly: a multi-million-quad batch is just a bigger file.
- Crash-safe via atomic rename — exactly-once apply semantics for
  the consumer.
- Easy to inspect what's queued (just `ls` the dir).

**Cons:**
- Latency: writes don't materialize until the next watcher tick
  (~30s typical). Unsuitable for low-latency request paths.
- Disk overhead: feed dir grows if processing falls behind.
- Operational complexity: watcher behavior under disk-full,
  permission errors, large files; archive rotation policy.

**When to use:** bulk imports (re-ingesting a corpus, replaying
from a backup, occasional ETL jobs). Lower-priority writers where
~30s latency is fine.

---

## Operational patterns

These work regardless of which write-stacking pattern you pick.

### Backup via Checkpoint

`(*PebbleStore).Checkpoint(destDir)` produces a hardlink-based
snapshot of the live store. Sub-second on tens of GB; the
destination is itself a readable Pebble dir.

The application-level shape (we use this for SecDek):

1. Server exposes `POST /admin/pebble-checkpoint?dest=...`
   (auth-gated). Returns 200 + duration when checkpoint completes.
2. A sibling shell script curls the endpoint, then `tar czf` the
   destination, ships to S3 with sha256 metadata, deletes the
   local destination.
3. Verify side: pull from S3, gunzip + untar, open with
   `quadstore.OpenPebbleReadOnly`, sample-read. Compare quad
   count + label distribution against the source.

**Measured:** a 2.6 GB store checkpointed in 10 ms via the admin
endpoint. Pebble's docs claim sub-second up to tens of GB; our
measurement is consistent.

### Read-only verification

`quadstore.OpenPebbleReadOnly(dir)` opens an existing dir without
acquiring an exclusive write lock. Useful for:

- Verifying a checkpoint or backup snapshot is structurally sound
  (open it, sample-query, count rows).
- Running analytics or exports against a frozen dir without
  disturbing it.

**Note**: `OpenPebbleReadOnly` still acquires SOME lock; you can't
have a read-only opener AND the live writer simultaneously on the
same dir. ReadOnly is for FROZEN dirs (checkpoints, archives), not
for sharing the live store.

**The merger-name SST guard:** every opener of the store —
including read-only — must register the same merger as the writer.
Pebble persists the merger NAME in every SST and rejects opens with
a different merger name:

```
pebble: merger name from file "quadstore.label-count.v1" !=
        merger name from options "pebble.concatenate"
```

`OpenPebbleReadOnly` does the right registration internally. **Do
not** open with raw `pebble.Open(dir, &pebble.Options{ReadOnly: true})`
— you'll get the merger error.

### Crash recovery

Pebble's WAL handles abrupt termination. The last fsynced commit
is preserved; un-fsynced writes are lost (consistent with
single-database guarantees). On re-open, the WAL replays into the
LSM.

**The application-level lesson:** every Job/writer must be
idempotent. A killed mid-tick run will be retried on the next
schedule; that retry must produce the same end state. Per-subject
sentinels (only emit if not already present) are the canonical
shape. quadstore's `INSERT OR IGNORE`-style commit semantics
support this naturally.

---

## SQLite → Pebble migration: gotchas

If you're migrating from quadstore's SQLite backend to Pebble,
expect to fight three things beyond the writer-architecture
question.

### 1. `IngestSortedExternal` scratch dir defaults to `os.TempDir()`

`IngestSortedExternal` writes per-chunk run files to disk for k-way
merge. The default scratch location is whatever `os.TempDir()`
returns — typically `/tmp` on Linux.

**On modern cloud images (Amazon Linux 2023, recent Fedora) `/tmp`
is a tmpfs sized at ~50% of RAM.** A migration of a 30 GB SQLite
DB through `IngestSortedExternal` produces tens of GB of merge runs
that overflow tmpfs and fail with:

```
write /tmp/quadstore-ingest-sorted-external-.../run-N-lsp.dat:
no space left on device
```

We hit this at 7M of 19M quads on a t4g.large with ~3.9 GB tmpfs.

**Workaround:** set `TMPDIR=/path/on/persistent/disk` before
invocation. Re-running with TMPDIR pointed at an EBS volume let
the migration complete cleanly.

**Real fix (filed as quadstore TODO):** expose
`IngestSortedExternalOptions.ScratchDir` so callers don't have to
know about the env override. PR welcome.

### 2. Merger registration is structural, not optional

If your application uses a custom `pebble.Merger` (quadstore
registers `quadstore.label-count.v1` for per-label running totals),
EVERY opener of the store must register the same merger. quadstore's
`OpenPebble` and `OpenPebbleReadOnly` both do this internally — but
if you're tempted to use raw Pebble for a verify tool or analytics
script, you'll trip the SST merger guard.

**The fix:** route all Pebble opens through quadstore's helpers,
even for tooling that "only reads."

### 3. The query layer ports cleanly, mostly

`Reader.Find(Pattern)` honors the same pattern shape across both
backends. ~75% of typical query catalogs port directly. The
exception is anything depending on raw SQL — `SELECT ... GROUP BY`
aggregates, JOINs, etc. — which an embedded KV engine doesn't have.
Port these to in-Go aggregation over streaming reads.

In our case, 197 query patterns ported through 6-8 named templates;
the remainder needed bespoke read loops.

**On-disk size:** Pebble compressed our 32.83 GB SQLite source to
2.57 GB — 12.7× compression — even with the 4-key prefix scheme's
write amplification. Pebble's LSM compresses better than SQLite's
B-tree on quad shapes.

---

## Anti-patterns

### "Just open the dir from each binary"

Worked on SQLite-WAL. Doesn't work on Pebble; second open fails.
Surfaces 24h post-cutover when the daily cron tries to run and
discovers it can't get the lock — by which time you've lost a day
of writes. (We did this. Don't do this.)

### "Stop the server briefly to let the cron write"

Conceptually correct (rotate the lock-holder) but operationally
fragile. A long-running ingest holds the lock for 13+ minutes on
real workloads — meaning the server is unreachable for that
window. Plus you have to coordinate stop/start through systemd,
which gets messy with five separate writer cron jobs.

If you're tempted by this, you've already paid the cost of
designing the writer architecture. Use Pattern 1 instead.

### "Run the cron inside the server's process via signal handler"

We saw this proposed. The idea: server holds the lock; an external
signal (SIGUSR1?) tells it to fire the daily job. **Don't.** You're
inventing a worse version of the in-process scheduler. Use a real
`time.Ticker` + Go goroutine.

### "Use `OpenPebbleReadOnly` for the cron job"

Read-only opens can't write. The cron is a writer. This doesn't
work.

### "Default `os.TempDir()` for the migration tool"

Already covered. Override via TMPDIR. Detection: when migrating,
watch for `no space left on device` errors midway through. The
initial source scan completes fine; the failure is in the merge
phase.

---

## Decision tree

If you're standing up a new quadstore-backed application:

1. **Will it ever have more than one process writing to the store?**
   - No → either backend works; pick based on perf / on-disk size.
   - Yes → keep going.
2. **Are the additional writers existing binaries you don't want
   to fold in?**
   - No → Pattern 1 (in-process scheduler). Default.
   - Yes → keep going.
3. **Do the writers run on the same machine as the server?**
   - Yes → Pattern 3 (file-feed) for bulk; Pattern 2 (admin
     endpoint) for low-latency.
   - No → Pattern 2 (admin endpoint). Pattern 3 needs a shared
     filesystem you probably don't have.

If you're migrating from quadstore SQLite to Pebble, do this
**before flipping the backend env var**, not after. The lock won't
materialize as a problem until the second writer tries to open;
by then you may have already cut over.

---

## What's queued in quadstore itself

These are TODOs in this repo, surfaced by SecDek's experience:

1. **`IngestSortedExternalOptions.ScratchDir`** — make the override
   discoverable instead of requiring `TMPDIR` env knowledge. PR
   welcome.
2. **A `decklib/scheduler` package** — extract a generic
   Job/Schedule/Cadence abstraction that any quadstore-using
   application can drop in. Currently lives at
   `secdek/internal/scheduler` waiting for a second consumer to
   trigger the hoist (per the standard "wait for second user"
   abstraction policy in this repo).
3. **A reference application** showing Pattern 1 with all four
   operational concerns wired up (scheduler, admin checkpoint,
   read-only verify, file-feed bulk import). Currently lives in
   `secdek` but tangled with domain code; a stripped reference
   would help adoption.

These are not blockers for choosing Pebble today; just rough edges
worth filing for v1.0.

---

## Acknowledgments

Findings synthesized from production operation of SecDek
([github.com/dukkandcards/secdek](https://github.com/dukkandcards/secdek))
through a 2026-05-06 SQLite→Pebble cutover, 2026-05-07 rollback,
and 2026-05-07/08 side-environment validation. The rollback was
caused by exactly the multi-binary-writer anti-pattern this doc
warns against; the in-process scheduler was the resolution.

Pebble itself performed correctly in every observed case. The
mistakes were entirely on the application side, born from carrying
SQLite assumptions into the Pebble world. Hence this doc.
