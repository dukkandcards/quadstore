# Contributing to quadstore

Thanks for your interest. The project is small on purpose and will stay small on purpose. This document lays out what we welcome, what we will politely decline, and how to submit.

## What we welcome

- **Additional storage backends that fit the single-binary shape.** Embedded BadgerDB, embedded BoltDB, embedded DuckDB — anything that does not require a separate process or a network round-trip. We will not accept backends that assume a running database server.
- **Indexing patterns that speed common queries** without adding dependencies. If the idea is "add a compound index on `(predicate, label, subject)` and document when to use it," we want to hear it.
- **Label-namespace patterns documented from real use.** The four namespaces (`source:`, `derived:`, `human:`, `meta:`) are the opinionated core of quadstore. If you are using them in a live product, a short write-up of the schema you settled on is more valuable than code.
- **Bug reports from real deployments.** "I tried to write a million quads and observed X" is a better bug report than "I read the README and had a question."
- **Examples.** A 100-line runnable program over a domain we haven't covered (citations, membership, kinship, hyperlinks, anything) is a welcome PR into `examples/`.
- **Documentation fixes, typos, and clarifications.** Always welcome.

## What we will politely decline

- **Distributed consensus, replication, sharding, cluster management.** These are good problems. They are not this project's problems.
- **Cypher / Gremlin / SPARQL / GraphQL compilers.** If you want a query language, use a database that ships one. quadstore's API is Go, and that is a design choice, not a gap.
- **Remote server modes, gRPC surfaces, REST wrappers, admin dashboards.** A quadstore-backed application can ship any of these; the library itself will not.
- **Enterprise-readiness features.** Role-based access control beyond the tenant-label pattern, audit logs beyond what SQLite gives you, single-sign-on adapters, etc.
- **Switching the default backend off SQLite.** Alternative backends are welcome alongside; the default stays SQLite.
- **Adding a required non-open-source dependency.** No AGPL, no dual-license, no BSL.

None of these are personal. They are shape decisions.

## How to submit

1. Open an issue describing the change before you write non-trivial code. This saves everyone time when the answer is "that lives in a different project."
2. Fork, branch, commit, open a PR against `main`.
3. Keep PRs focused. One change per PR. A README fix and a new backend do not belong in the same PR.
4. Tests expected for any code path not purely a docs change. Run `go test ./...` before opening.
5. No CLA. Standard GitHub contribution model applies; by opening a PR you license your contribution under the project's MIT license.

## Running locally

```bash
git clone https://github.com/dukkandcards/quadstore
cd quadstore
go test ./...
```

Go 1.22+ expected. SQLite is vendored via the pure-Go driver; no CGO required.

## Coding conventions

- `gofmt` / `go vet` clean.
- Exported functions have doc comments. Unexported functions can explain themselves if they are non-obvious.
- Tests live next to the code they test (`foo.go` / `foo_test.go`).
- No dependencies added without a discussion in the issue tracker first. The dependency budget is deliberately tiny.

## Reporting security issues

Do not open a public issue for security reports. Email [maintainer email] with the details. We will acknowledge within a few days.

## Code of Conduct

Participation in the project is governed by [CODE_OF_CONDUCT.md](./CODE_OF_CONDUCT.md).
