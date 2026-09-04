---
name: Issue Blueprint
about: File an issue following the branchDAM context and scope blueprint.
title: "type(scope): short description"
labels: ""
assignees: ""
---

## Context

<!-- Problem or spec pillar, current behaviour, code references. -->

```go
// Code snippet showing the affected area
```

## Scope

<!-- What this issue changes. One issue = one PR. -->

## Out of scope

<!-- What deliberately isn't covered, so it doesn't get scope-crept in review. -->

## Acceptance criteria

- [ ] <!-- Requirement 1 -->
- [ ] Docs updated (`docs/*.md`, `AGENTS.md`) if behaviour or invariants changed
- [ ] Test coverage added/modified
- [ ] `make lint && make test && make build` green

## Notes

Manual: true

Blocked by: #
Branch off `origin/main`. One PR.
