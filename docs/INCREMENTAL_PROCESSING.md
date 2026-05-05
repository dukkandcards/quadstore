# Incremental Processing Patterns

**Audience:** consumers of `quadstore` (this library) building ingest /
derivation / migration pipelines.

**TL;DR:** if your tick reads "every quad in label X" or "every row in
table Y," you've built a loop that processes data you already
processed. Don't. Use one of three patterns below.

This doc exists because SecDek (the first production-scale consumer)
fell into the anti-pattern across every batch phase, the cost
compounded as the corpus grew, and the failure mode (timeouts on
60-min jobs that should have run in 30 sec) was severe enough to
warrant a written warning to future consumers — including a future
version of yourself.

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

## See also

- `docs/PARTITIONING_DESIGN.md` — partitioning helps reads scope
  to one fact family. Orthogonal to incrementality but
  complementary: a partitioned corpus + watermark pipeline is
  what gets you to "tick runs in seconds regardless of
  total-corpus size."
- `~/secdek/docs/INGEST_REDESIGN_PLAN.md` — the SecDek-specific
  application of these patterns, including the staged-migration
  approach.
