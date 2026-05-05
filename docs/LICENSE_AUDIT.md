# quadstore — License Audit

**Last verified:** 2026-05-05
**Next recheck due:** 2026-08-05 (quarterly cadence)

quadstore is MIT-licensed. Every direct and transitive dependency the
project pulls in must remain on a permissive license (MIT / BSD / Apache
2.0). No AGPL, no BSL, no SSPL, no dual-licensed-with-commercial-tier.
This file is the canonical inventory; the per-recheck procedure at the
end keeps it honest.

If a dependency relicenses out from under us, the response is the
documented mitigation in [`CONTINGENCY.md`](./CONTINGENCY.md). This file
records *what* we depend on; that one records *what we do if any of it
moves*.

## Project license

quadstore: **MIT** ([`LICENSE`](../LICENSE)). No patent grant, no
contributor licensing agreement, no commercial tier. Deliberate; see
[`README.md` § License](../README.md#license).

## Direct dependencies

| Module | Version | License | Source |
|---|---|---|---|
| `github.com/google/uuid` | v1.6.0 | BSD-3-Clause | [google/uuid](https://github.com/google/uuid/blob/master/LICENSE) |
| `modernc.org/sqlite` | v1.48.2 | BSD-3-Clause | [pkg.go.dev](https://pkg.go.dev/modernc.org/sqlite?tab=licenses) |

## Pebble backend (opt-in via `OpenPebble`)

The Pebble backend is the largest single source of transitive deps in
quadstore. Every dep listed below is currently permissive. Six of them
are maintained by Cockroach Labs — the same vendor whose flagship
product (CockroachDB the database) was relicensed from Apache 2.0 to
BSL in 2019 and to a hybrid in 2024. **No Cockroach Labs *library* has
ever been relicensed**, but the trust profile warrants explicit
inventory + the contingency plan in `CONTINGENCY.md`.

### Pebble itself

| Module | Version | Pinned SHA | License | Source |
|---|---|---|---|---|
| `github.com/cockroachdb/pebble/v2` | v2.1.5 | `36a5551312e40777b3afff9846796aaadca5f877` | BSD-3-Clause (LevelDB-Go Authors lineage) | [LICENSE](https://github.com/cockroachdb/pebble/blob/master/LICENSE) |

The SHA above is the freeze point referenced by
[`scripts/fork-pebble-on-trigger.sh`](../scripts/fork-pebble-on-trigger.sh)
— if Cockroach Labs ever relicenses Pebble, that script clones this
exact commit and proves quadstore builds + tests pass against the
fork before we publish anything. **Verified end-to-end on 2026-05-05;**
mitigation is real, not hypothetical. See
[`CONTINGENCY.md`](./CONTINGENCY.md) for the full procedure.

### Cockroach Labs auxiliary libraries

| Module | Version | License | Source |
|---|---|---|---|
| `github.com/cockroachdb/errors` | v1.11.3 | Apache 2.0 | [LICENSE](https://github.com/cockroachdb/errors/blob/master/LICENSE) |
| `github.com/cockroachdb/redact` | v1.1.5 | Apache 2.0 | [LICENSE](https://github.com/cockroachdb/redact/blob/master/LICENSE) |
| `github.com/cockroachdb/swiss` | v0.0.0-20251224 | Apache 2.0 | [LICENSE](https://github.com/cockroachdb/swiss/blob/main/LICENSE) |
| `github.com/cockroachdb/crlib` | v0.0.0-20241112 | Apache 2.0 | [LICENSE](https://github.com/cockroachdb/crlib/blob/master/LICENSE) |
| `github.com/cockroachdb/logtags` | v0.0.0-20230118 | Apache 2.0 | [LICENSE](https://github.com/cockroachdb/logtags/blob/master/LICENSE) |
| `github.com/cockroachdb/tokenbucket` | v0.0.0-20230807 | Apache 2.0 | [LICENSE](https://github.com/cockroachdb/tokenbucket/blob/master/LICENSE) |

### Other Pebble-pulled transitive deps

| Module | License | Notes |
|---|---|---|
| `github.com/DataDog/zstd` | BSD-2-Clause | zstandard CGo binding (we use the pure-Go path) |
| `github.com/klauspost/compress` | BSD-3-Clause | |
| `github.com/golang/snappy` | BSD-3-Clause | Google |
| `github.com/getsentry/sentry-go` | MIT | Pebble uses for crash reporting; not initialized unless caller does |
| `github.com/prometheus/client_golang` | Apache 2.0 | metrics; not exposed by quadstore |
| `github.com/prometheus/client_model` | Apache 2.0 | |
| `github.com/prometheus/common` | Apache 2.0 | |
| `github.com/prometheus/procfs` | Apache 2.0 | |
| `github.com/RaduBerinde/axisds` | MIT | btreemap-style ds |
| `github.com/RaduBerinde/btreemap` | MIT | |
| `github.com/cespare/xxhash/v2` | MIT | |
| `github.com/dustin/go-humanize` | MIT | |
| `github.com/kr/pretty` / `kr/text` | MIT | |
| `github.com/beorn7/perks` | MIT | |
| `github.com/gogo/protobuf` | BSD-3-Clause | |
| `github.com/golang/protobuf` | BSD-3-Clause | (deprecated, replaced by google.golang.org/protobuf) |
| `google.golang.org/protobuf` | BSD-3-Clause | |
| `golang.org/x/*` | BSD-3-Clause | Go team |

## Recheck procedure

Quarterly. Takes ~10 minutes.

```
# 1. Confirm Pebble itself unchanged
curl -sL https://raw.githubusercontent.com/cockroachdb/pebble/master/LICENSE | head -3

# 2. Same for each cockroachdb auxiliary
for repo in errors redact swiss crlib logtags tokenbucket; do
  echo "=== $repo ==="
  curl -sL "https://raw.githubusercontent.com/cockroachdb/$repo/master/LICENSE" | head -3
done

# 3. Re-run go list -m -u all and look for any module whose latest
#    version diverges from go.mod — those are upgrade candidates and
#    must re-pass this audit before being pulled in.
go list -m -u all 2>&1 | grep -v 'indirect'
```

If any of the above shows a license change, escalate to the contingency
plan in `CONTINGENCY.md` before merging any further upgrade.

## Recheck log

| Date | Outcome | Notes |
|---|---|---|
| 2026-05-05 | clean | Initial audit — all 8 Pebble-related deps verified Apache 2.0 / BSD-3 |
| 2026-05-05 | drill | Ran `scripts/fork-pebble-on-trigger.sh` end-to-end as a mitigation drill: cloned pebble@36a5551 locally, retargeted go.mod, ran tests against the fork, all green. Reverted clean. No quadstore code changes needed. |

Append a row on every quarterly recheck. If something moves, escalate.
