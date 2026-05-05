---
name: Feature request
about: Something you'd like quadstore to do that it doesn't
title: ''
labels: enhancement
assignees: ''
---

## What would you like quadstore to do

A clear description of the capability. If you've got a code shape in mind, sketch it:

```go
// hypothetical API
store.SomeNewMethod(...)
```

## What problem does this solve

What are you trying to do that the current API makes hard? Specific is better than general.

## Have you considered alternatives

Is there a way to do this today with the current API that's good enough? If so, what's the deficiency?

## Scope notes

quadstore is intentionally narrow:

- **No** distributed / sharding features.
- **No** query language compilers.
- **No** server mode.
- **No** graph-algorithm primitives (PageRank, shortest path, community detection).

If your request is one of these, we'll close it politely with a pointer to a project that does that thing well. quadstore is for the cases where the graph fits on one machine and the operational budget is one binary; we'd rather stay narrow than become broader and worse.

If your request is *not* one of those, we want to hear it.
