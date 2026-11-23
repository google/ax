---
name: sync-contract
description: >
  End-of-change pass that brings AGENTS.md and skills/ back in step with the code.
  Load after any code change, before declaring done; it is the last step of the Definition of done.
---

# Sync the agent contract with the code

`AGENTS.md` and `skills/*/SKILL.md` quote facts about the tree: file paths,
function names, versions, flag defaults, CI steps. Code changes silently
invalidate them. Run this diff-driven procedure before declaring a task done.

## 1. Inspect what changed

```bash
git diff --name-only origin/main...HEAD      # or: git status --short for uncommitted work
```

## 2. Find which contract docs mention what you changed

1. **Search `AGENTS.md` and `skills/`** for the file paths, package names, CLI
   flags, or exported symbols touched in your diff.
2. **Check by area**:
   - Package layout, CLI subcommands, CI steps (`.github/workflows/go.yml`),
     `Makefile`, `go.mod`, or `CONTRIBUTING.md` → check `AGENTS.md`.
   - Any domain listed in the `AGENTS.md` **Skills** table (for example,
     `internal/substrate`, `internal/guest`, `internal/metadata`,
     `ate-system` / `atenet-router` addresses, or E2E setup →
     `skills/substrate/SKILL.md`) → check that skill's `SKILL.md`.

## 3. Per-document checklist

For each document you opened:

- Every path it names exists (`ls` it).
- Every function/const/flag it names still exists under that name.
- Every number it states (file counts, package counts, versions) is re-derived,
  not trusted.
- Every "only place that…" / "no X exists" statement is re-checked across the
  tree, not just in the changed file.
- Removed behaviour is removed from the doc; renamed behaviour is renamed.
- New behaviour that a future agent would get wrong without being told is
  added — as a fact with its location, not as advice.
- Prefer a pointer ("see `go.mod`") over copying a value that changes often.

## 4. Quick structural check

Run this one-liner whenever `AGENTS.md` or `skills/` was touched:

```bash
# Verify AGENTS.md is the only agent contract and that every
# skills/<name>/SKILL.md is indexed in AGENTS.md
test ! -e CLAUDE.md && test ! -e GEMINI.md || echo "EXTRA: CLAUDE.md or GEMINI.md"
for d in skills/*/; do
  s=$(basename "$d")
  test -f "$d/SKILL.md" && grep -q "\`/$s\`" AGENTS.md || echo "MISSING: $s"
done
```
