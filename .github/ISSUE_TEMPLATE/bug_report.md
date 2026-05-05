---
name: Bug report
about: Something quadstore did wrong
title: ''
labels: bug
assignees: ''
---

## What happened

A clear description of what went wrong. Stack trace or panic output is useful here.

## Reproducer

The smallest piece of Go code that triggers the issue. If it's runnable, that's the gold standard:

```go
package main

import "github.com/dukkandcards/quadstore"

func main() {
    // ...
}
```

## Environment

- quadstore version: (e.g. v0.1.0, or `git rev-parse HEAD`)
- Go version: (`go version`)
- OS / arch: (e.g. linux/arm64, darwin/arm64, windows/amd64)
- SQLite pragmas (if any non-default): `synchronous=NORMAL`, `journal_mode=WAL`, etc.
- DB size + workload shape, if relevant: "28 GB DB, ~10K writes/sec"

## What you expected

Briefly: what should have happened.
