# Incremental Processing Patterns

**Audience:** consumers of `quadstore` (this library) building ingest /
derivation / migration pipelines.

**TL;DR:** if your tick reads "every quad in label X" or "every row in
table Y," you've built a loop that processes data you already
processed. Don't. The simplest fix — and the right one in most cases —
is a per-subject sentinel check that gates the expensive work. See
the SecDek convention below for a worked specialization, or one of
three patterns lower in this doc.

This doc exists because SecDek (the first production-scale consumer)
fell into the anti-pattern across every batch phase, the cost
compounded as the corpus grew, and the failure mode (timeouts on
60-min jobs that should have run in 30 sec) was severe enough to
warrant a written warning to future consumers — including a future
version of yourself.

## Reference implementations (the simplest pattern)

Before reaching for watermarks or any new abstraction, look at how
existing working binaries in your codebase handle this. Most
incremental-processing problems have a three-line answer.

SecDek's `docs/INGEST_CONVENTION.md` formalizes this in one
sentence: **every binary skips work for subjects it has already
processed, by checking for a per-subject sentinel before doing the
expensive work.** Three working SecDek binaries match this shape:

| binary | sentinel | check |
|---|---|---|
| `cmd/ingest` | quadstore: any quad with the letter's subject | `letterExists(ctx, d, subj)` |
| `cmd/correspondence-extract` | filesystem: `extracted.json` + schema version | per-filing `os.Stat` + version compare |
| `cmd/correspondence-ocr` | filesystem: per-image engine sentinel | per-image `os.Stat` |

If your data is in the quadstore, the sentinel is "any quad with
this subject exists" or "this specific predicate exists for this
subject." If your data is on disk, the sentinel is a marker file
with a schema version. The check goes BEFORE the expensive work,
not after.

Reach for the patterns below only when this shape can't cover your
case. Watermarks generalize when you have many phases consuming a
shared change-set stream and want the empty-tick cost to be three
reads. Per-subject sentinels generalize when each subject's
processing is independent — which is most ingest pipelines.

## The anti-pattern: "rebuild from scratch every tick"

The shape:

```
every N minutes/hours:
    remove all derived quads for labels I own
    read all source quads
    compute derivation for every source quad
    commit the result
```

Why it's seductive:

- Idempotent. Re-running on the same source gives the same output.
- No state to track between runs.
- Easy to reason about during initial design when the corpus is small.

Why it breaks under load:

- Time is proportional to corpus size, not change-set size. The same
  job that ran in 2 minutes when the corpus was 1 GB takes 60
  minutes when the corpus is 28 GB — even if only 30 letters
  changed that day.
- Full-corpus scans hammer disk IOPS. On a gp3 EBS volume at 6000
  IOPS, even an indexed `GROUP BY label` over a 28 GB DB takes
  several minutes.
- Fetches against external APIs (SEC EDGAR, in our case) repeat
  every cycle, wasting bandwidth and the external party's rate
  budget.
- Failure modes cascade: a single tick that overruns its timeout
  takes down the whole derivation, which then runs longer the next
  time because more time has passed since a successful run.

You will write the rebuild-from-scratch shape on day one. You will
keep it longer than you should. The corpus will quietly grow until
the substrate (SQLite + EBS + cron timeout budget) cannot absorb a
full re-derive in the allotted time. This guide is for the day
after that.

## Three patterns that fix it

### Pattern 1: Watermark (preferred for derivation pipelines)

Keep a per-phase **last-processed-at** timestamp. On each tick:

```
last_seen = read meta:phase-watermark:{my-name}
src_max   = read meta:source-watermark:{my-source-label}
if src_max <= last_seen:
    return                              -- nothing new, exit
new_subjects = scan(label, inserted-at > last_seen)
process(new_subjects)
write meta:phase-watermark:{my-name} = src_max
```

What you need:

- A monotonic per-subject `meta:inserted-at` quad emitted at first
  commit. The library does not emit this for you (intentionally —
  there are correctness cases where you don't want it). Your ingest
  path emits it.
- A rolling `meta:source-watermark:{label}` updated by ingest
  commits. Computable via
  `SELECT max(commits.created_at) FROM commits WHERE label = ?` if
  you don't want to maintain it separately.
- A per-phase `meta:phase-watermark:{phase-name}` written on every
  successful run (and *only* on successful runs — failures must
  not advance it).

Properties:

- Empty tick cost: 3 reads, ~ms.
- Loaded tick cost: linear in change set, not corpus size.
- Failure semantics: a failed phase doesn't advance its watermark;
  the next run re-processes the same window, which is idempotent
  via your existing `INSERT OR IGNORE`.
- A `--rebuild` flag clears the watermark and reprocesses
  everything. That becomes your forced-full-refresh path, not your
  default behavior.

When *not* to use it:

- The phase legitimately needs to consider every existing row each
  cycle (rare — usually the "every row" framing is wrong).
- The phase's output depends on a global aggregate (median,
  TF-IDF, rank) that changes when any row changes. Then watermark
  doesn't apply directly; you need a different incrementality story
  (delta updates to the aggregate, or accept full recompute on a
  longer cycle).

### Pattern 2: Conditional GET (for HTTP-fetching phases)

Cache the `ETag` and `Last-Modified` headers from the prior fetch.
Send `If-None-Match` / `If-Modified-Since` next time. `304 Not
Modified` → keep prior derived state, skip re-fetch and re-derive.

```
prior = read derived:http-cache:{url}        -- {etag, last-modified}
GET url with If-None-Match: prior.etag, If-Modified-Since: prior.last-modified
if status == 304:
    return                                   -- nothing changed
if status == 200:
    derive from response body
    cache new etag + last-modified
```

Storage shape:

```
<url-as-subject> derived:http-cache:etag           "..."
<url-as-subject> derived:http-cache:last-modified  "..."
<url-as-subject> derived:http-cache:fetched-at     "..."
```

Properties:

- Cheap probe to the external API even when nothing changed
  (one round-trip, no body bytes).
- 90%+ hit rate is realistic for SEC submissions.json, GitHub
  events, S3 listings, anywhere the external party publishes
  proper cache headers.
- Combines well with Pattern 1: outer loop is watermark-driven
  ("which CIKs need refreshing"), inner per-CIK fetch is
  conditional-GET-based ("did SEC actually publish anything new").

When *not* to use it:

- The external API doesn't honor conditional-GET semantics. SEC
  EDGAR does. Many small services do not.
- The data is push-only / no GET endpoint exists.

### Pattern 3: Resumable migration (for one-shot data moves)

Migrations are special-case incremental processing: the inputs are
fixed (a snapshot), but you want the option to resume after a
failure without re-copying the millions of quads you already moved.

The library's `Migrate` and `MigrateFromSnapshot` are idempotent
via `INSERT OR IGNORE` — re-running over the same source produces
the same destination state. But "idempotent" is not "fast" — a
re-run still reads every row from source and tries to insert every
row into destination, paying the read + dedupe cost.

For migrations big enough that a re-read is expensive (~hours), use
the existing `OnlySince` option:

```go
quadstore.Migrate(ctx, src, dst, quadstore.MigrateOptions{
    OnlySince: lastSuccessfulCommitTime,
})
```

`OnlySince` filters source quads to those whose containing commit's
`created_at >= since`. Run an initial migration with `OnlySince=0`,
record the latest commit timestamp at completion, and any retry /
top-up uses that timestamp as the new `OnlySince`.

For migrations of *legacy data without commit timestamps* (e.g.,
quads written via the permissive `Add` / `AddBatch` path before
commit-tracking existed), `OnlySince` skips those rows by design —
the audit trail is the only watermark this library knows. If your
source has legacy data, do an initial pass with `OnlySince=0` (full
copy), then watermark from that point.

A more general pattern — store a `meta:migration-watermark` quad in
the destination after each successful chunk. Resume by reading that
quad and starting the next chunk from there. This is application
code; the library does not provide it out of the box because the
right chunk shape (per-label, per-time-window, per-quad-count) is
consumer-specific.

## How this maps to migration tooling

Your first migration:

1. Snapshot source via `MigrateFromSnapshot` (race-free: see
   `docs/PARTITIONING_DESIGN.md` §Migration tooling).
2. Run `Migrate` with `OnlySince: 0` to copy everything.
3. After success: record `now()` as the last-watermark-time in
   destination's `meta:migration-watermark` quad.

Subsequent migrations (drift recovery, re-partitioning):

1. Read `meta:migration-watermark` from destination.
2. Run `Migrate` with `OnlySince: <that time>`.
3. Update the watermark on completion.

This is the same Pattern 1 applied to migrations. The library
already supports it via `OnlySince`; you just need to remember to
record the watermark.

## Anti-patterns that look incremental but aren't

These are easy to write and look incremental at a glance. They are
not.

### Skip-if-marker-exists

```
for subj in all_subjects:
    if has_marker(subj):
        continue
    process(subj)
```

What's wrong: you still scan every subject. The cost is
`O(corpus_size)`, not `O(change_set_size)`. The "skip" only saves
the *processing* work, not the *scan* work.

When it's defensible: as a transitional patch on top of a
rebuild-from-scratch shape, while you're building the watermark
version. SecDek shipped this as a band-aid on 2026-05-05 with the
plan to retire it after watermark conversion.

### Diff against a hashed checksum of all inputs

```
current_hash = hash(all source quads)
prior_hash   = read last-processed-hash
if current_hash == prior_hash:
    skip
else:
    process(all source quads)
```

What's wrong: the "diff" tells you something changed but not what.
You still process everything when anything changes — same cost as
rebuild-from-scratch on any non-empty change set.

When it might be defensible: a coarse cache invalidation gate at
the *outermost* loop, where the inner pipeline is already
incremental on the change set the gate identifies. Not as a
substitute for incrementality.

### Re-fetch with `Cache-Control: no-cache`

The HTTP cache headers exist so callers don't have to do this. If
you find yourself bypassing cache headers because "it might have
changed," your derivation is too tightly coupled to the source's
volatility. Use Pattern 2.

## Detecting the anti-pattern in code review

Three smells, in order of how often they appear:

1. **`SELECT * FROM ...` over a label that grows over time, in a
   recurring tick.** Almost always wrong if the tick runs more
   often than once a quarter.
2. **`removeSubjectLabel(...)` followed by re-derivation in the
   same function.** Doing both means you're throwing away work and
   redoing it. Sometimes correct (forced refresh) but should be
   gated by a flag, not the default cron behavior.
3. **An external HTTP fetch with no `If-None-Match` /
   `If-Modified-Since` headers in a recurring tick.** Pure waste
   ~95% of the time on any API that publishes proper cache
   headers.

If you see any of these in a tick that runs more than once a day,
flag it.

## Operational signals that you've fallen in

- Your derivation tick takes longer than yesterday and yesterday
  was longer than the day before, but no major source changes
  happened. The cost is corpus-size-driven; the corpus is
  growing.
- You raise a tick's timeout because it's running over. You will
  raise it again. The right move is the redesign, not the
  timeout.
- You explain to a colleague that "we re-fetch every day in case
  it changed." That's the moment.

## Why this happens (the culture problem)

You will write the "rebuild from scratch every tick" shape. So will
every engineer who follows you. It is not a competence failure. There
are real reasons the wrong shape gets shipped:

1. **It is genuinely the easier code to author on day one.** With 100
   subjects in the DB, re-emit + INSERT OR IGNORE produces correct
   output in 2 seconds. The shape is robust — no state to track, no
   watermark consistency questions, no edge cases around "what if
   the phase failed last time." The cost ledger doesn't appear until
   the corpus is two or three orders of magnitude bigger, by which
   point the code is years old and "just works."

2. **The library may not expose a watermark primitive at design
   time.** Each consumer rolls its own approximation. The
   approximation is always "scan everything." If you are designing a
   new derivation phase against this library, the existence of a
   watermark helper is the cheapest thing to add upfront — much
   cheaper than retrofitting after the fact.

3. **The cost is invisible until it is fatal.** A daily run that
   takes 30 min and produces 0 net new derivations does not alarm.
   It alarms when it takes 31 min and times out. By then the
   redundant work has been compounding for months. Observability
   needs a `phase_watermark_lag` metric and a `redundant_work_ratio`
   metric (work that emitted no new quads / total work) — neither of
   which is standard.

4. **Code review against a single phase looks fine.** Reviewers ask
   "does this produce correct output?" The answer is yes. Reviewers
   rarely ask "is the cost O(corpus) or O(change set)?" across all
   phases together. The anti-pattern is invisible at the per-PR
   level; you have to look at the system shape.

5. **The substrate's failure mode is silent for a while.** SQLite +
   gp3 EBS will absorb a lot of unnecessary reads before throwing.
   IOPS contention shows up as "things feel slow" before it shows
   up as "things broke." Engineers route around the slowness rather
   than diagnosing it.

The institutional fix is three things, none of which is "be smarter":

- **Library-level**: a watermark helper that makes the right pattern
  cheaper to write than the wrong one. (`Store.ReadWatermark` /
  `WriteWatermark` go a long way.)
- **Convention-level**: the three smells in the previous section
  become a code-review checklist item. Any phase that does
  full-table reads in a recurring tick has to defend it.
- **Observability-level**: a per-phase `redundant_work_ratio`
  exposed as a metric. When the ratio crosses 0.95, the alarm fires
  even if the run still completed successfully.

This document is part of the convention-level fix.

## See also

- `docs/PARTITIONING_DESIGN.md` — partitioning helps reads scope
  to one fact family. Orthogonal to incrementality but
  complementary: a partitioned corpus + per-subject sentinel
  pipeline gets you to "tick runs in seconds regardless of
  total-corpus size."
- `~/secdek/docs/INGEST_CONVENTION.md` — the in-repo specialization
  for SecDek. Names the three reference binaries and the
  anti-pattern in one page. Required reading before writing a new
  ingest binary in that repo.
- `~/secdek/docs/INGEST_AUDIT.md` — per-binary compliance tracker
  for the convention above. Worked example of how to keep the
  pattern from drifting once it's established.
