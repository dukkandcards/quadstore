# quadstore — Contingency plan

What we do if a permissive dependency relicenses out from under us.
This document exists because a chunk of quadstore's optional Pebble
backend is maintained by Cockroach Labs, the vendor whose flagship
product (CockroachDB) was relicensed from Apache 2.0 to BSL in 2019
and to a hybrid in 2024. No Cockroach Labs *library* has ever been
relicensed — but planning for the case where one does is cheap, and
not planning for it is the kind of thing that ends a project.

The current inventory and licenses are in
[`LICENSE_AUDIT.md`](./LICENSE_AUDIT.md). This file describes the
mitigation procedure if any item there changes license.

## Pebble (`github.com/cockroachdb/pebble/v2`) relicenses

The most significant single dependency. Apache-2.0 today; pinned at
v2.1.5 (SHA `36a5551312e40777b3afff9846796aaadca5f877`) in `go.sum`.

**Mitigation is automated.** Run:

```
./scripts/fork-pebble-on-trigger.sh
```

What that script does:

1. Clones `cockroachdb/pebble` at the pinned SHA into
   `~/quadstore-pebble-fork`. Refuses to proceed if upstream's tag
   has been moved — the SHA mismatch alarm catches that case.
2. Verifies the LICENSE file at that commit is still permissive
   (matches `LevelDB-Go` / `Apache` / `BSD`) before going further.
3. Adds a `replace github.com/cockroachdb/pebble/v2 => $WORK_DIR`
   directive to quadstore's `go.mod` and runs `go mod tidy`.
4. Runs `go test -run "Pebble|Crash|Migrate|Property" ./...` to
   confirm quadstore builds + tests pass against the fork.
5. Prints the operator-action steps — pushing the local clone to
   `github.com/dukkandcards/pebble`, swapping the replace target
   to the GitHub fork, updating docs.

**Verified end-to-end on 2026-05-05.** The script ran clean, all
Pebble-related tests passed against the local fork, no quadstore
code changes were needed. Mitigation is real, not hypothetical.

It's safe to run as a drill any time. The drill exits with the
revert command at the bottom.

The non-`go.mod` work after the script — pushing the fork to
GitHub, retargeting the replace, updating CHANGELOG + README —
is operator action, not automation. The fork-to-GitHub step is
deliberately manual so we don't inadvertently publish a fork
during a drill.

Why this works: quadstore's public API is `Reader` / `Writer` /
`Batch`. Pebble lives entirely behind `internal/pebbleq`. The
fork swap touches `go.mod` and nothing else in quadstore's
public surface. quadstore's tests are sufficient because the
`pebbleq` adapter is the only Pebble caller.

### Auxiliary library co-relicense

Pebble's runtime path uses `cockroachdb/errors`, `redact`, `swiss`,
`crlib`, `logtags`, `tokenbucket`. Each is small (<1k lines mostly)
and individually replaceable. If any of these also relicenses, the
right move is usually to fork **Pebble** at a commit before its
go.mod required the relicensed auxiliary, then pin from there.
Pebble's git history gives us this option for free.

If only one auxiliary relicenses and Pebble keeps using a
not-yet-relicensed version, we may not need to fork at all —
just pin the auxiliary at its last permissive version and rely on
go.sum's checksum.

## Cockroach Labs auxiliary library relicenses

Smaller blast radius. Each is replaceable with relatively little code:

- `cockroachdb/errors` → could swap to standard `errors` + `%w` wrapping; only a small shim layer used by Pebble
- `cockroachdb/redact` → not exposed in quadstore's API; trivial to vendor or replace with a no-op
- `cockroachdb/swiss` → SwissTable Go map; the Go runtime now ships SwissTables natively (Go 1.24+), so this can likely be dropped or replaced
- `cockroachdb/crlib` / `logtags` / `tokenbucket` → Pebble-internal; if Pebble itself moves to a fork, our fork pins these (or replaces them) and quadstore is unaffected

If only one of the auxiliaries relicenses (and Pebble itself does not),
the cleanest move is to fork **Pebble** at the last commit before
Pebble's go.mod requires the relicensed auxiliary, and pin from there.
Pebble has its own contingency surface to give us this option.

## Non-Cockroach Pebble dependencies

DataDog/zstd, klauspost/compress, getsentry/sentry-go, prometheus/*,
RaduBerinde/* — all permissive (BSD-2/BSD-3/MIT/Apache-2.0). None has
shown any sign of relicensing risk, but the same general procedure
applies: pin, fork-on-trigger, communicate.

## modernc.org/sqlite (the default backend)

The SQLite-backed `Open(path)` path depends on `modernc.org/sqlite`
— BSD-3, single maintainer (Jan Mercl). If it stalls (no commit for
12+ months) or relicenses:

- The corpus of pure-Go SQLite reimplementations is shallow. Falling
  back to a CGo SQLite would break the "no CGo" promise.
- Forking is feasible but maintaining a transpiled C-to-Go SQLite
  reimplementation is a real undertaking — it's what `modernc.org/sqlite`
  *is*. Realistic mitigation in that scenario is: pin the last good
  version, run on it indefinitely (SQLite itself is famously stable),
  and accept that newer SQLite features won't land.
- The Pebble backend is partially insurance against this scenario:
  if the SQLite path breaks irrecoverably, Pebble is the alternate.

## Trigger procedure

If anyone notices a license change on any dep — or if the recheck
log in `LICENSE_AUDIT.md` flags one:

1. **Don't `go get` the new version.** Even unintentionally — it can
   pull a relicensed module into a clean tree.
2. **File a CHANGELOG note** describing what changed and what we're
   doing.
3. **Open a quadstore issue** (or a private one if the relicense isn't
   public yet) to track the migration.
4. **Execute the relevant section above** end-to-end: fork, retarget,
   release, communicate.

The bias is **toward action, not toward optimism**. A relicense
announcement is a one-way door; the longer we wait to fork, the more
upstream commits diverge from our last-Apache snapshot. Fork early.

## What this isn't

This document is not a guarantee that quadstore continues working
through every supply-chain failure. Some scenarios — Go itself
relicensing, a critical CVE in a forked dep with no upstream patch —
require their own response. This document covers the specific risk
class we explicitly took on by depending on Cockroach Labs' libraries.
