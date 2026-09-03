---
name: refresh-cubestack-platform-skill
description: Use when the cubestack-platform freshness guard test fails, or when CubeStack CRD types / platform runtime behavior change, to regenerate or edit the builtin cubestack-platform skill (crd-reference.md is generated; SKILL.md is narrative)
---

# Refresh the builtin cubestack-platform skill

The builtin `cubestack-platform` skill (issue #98) has two parts:

- `internal/skill/skills/cubestack-platform/crd-reference.md` — **generated**.
  Do not hand-edit it; the freshness guard
  (`internal/skill/cubestackgen/cubestackgen_test.go:TestCommittedCRDReferenceIsFresh`)
  fails CI when it disagrees with the generator.
- `internal/skill/skills/cubestack-platform/SKILL.md` — hand-authored narrative
  (usage guide, known-good example, common mistakes).

## When to use this skill

- The freshness guard test fails.
- CubeStack's CRD types changed upstream (you just bumped the vendored CRDs).
- CubeStack's runtime behavior changed (narrative facts are now wrong).

## Runbook

1. If upstream CubeStack CRDs changed, refresh the vendored snapshots:
   `make update-crds`
2. Regenerate the schema map: `make update-cubestack-skill`
3. If runtime behavior changed, edit the narrative in `SKILL.md`, sourcing facts
   from the CubeStack operator code / CRD field docs. Never put facts in
   `crd-reference.md` that belong in SKILL.md.
4. Run the guards: `go test ./internal/skill/cubestackgen/ ./internal/skill/`
5. Review the diff; in the commit message, note which CubeStack behavior/CRD
   change the update tracks.
