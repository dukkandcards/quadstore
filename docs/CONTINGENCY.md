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
v2.1.5 in `go.mod`. If a future v2.x.0 ships under a non-permissive
license:

1. **Don't upgrade.** v2.1.5's `go.sum` checksum is in our committed
   `go.sum`, so any moved tag fails verification automatically.
   Refusing the upgrade buys us time.

2. **Fork the last Apache commit** to `github.com/dukkandcards/pebble`.
   Pebble is Apache-2.0 — every commit through the relicense
   announcement is permanently usable under Apache terms. The
   in-flight commit is the one we keep. Apply the fork-on-trigger
   when the announcement happens; not before.

3. **Retarget `go.mod`** with a `replace` directive:
   ```
   replace github.com/cockroachdb/pebble/v2 => github.com/dukkandcards/pebble/v2 vX.Y.Z
   ```
   Run `go mod tidy`; commit. No quadstore code changes — `pebbleq`
   talks to the same Pebble API; the fork preserves it.

4. **Drop or carve out non-essential CRL libraries.** Pebble's runtime
   path uses `cockroachdb/errors`, `redact`, `swiss`, `crlib`,
   `logtags`, `tokenbucket`. If any of those also relicenses, they're
   smaller and simpler to fork or replace with stdlib + small
   in-tree shims. Most of them are <1k lines.

5. **Communicate.** Update `LICENSE_AUDIT.md`'s recheck log, bump
   quadstore's CHANGELOG with the fork rationale, and announce in
   the README. Anyone using quadstore via `OpenPebble` should know.

The upstream-fork interface boundary is already clean: quadstore's
public API is `Reader` / `Writer` / `Batch`. Pebble lives entirely
behind `internal/pebbleq`. The fork swap touches `go.mod` and
nothing else in quadstore's public surface.

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
