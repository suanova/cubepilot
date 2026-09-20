# AGENTS.md - Project Conventions

This file is the operating guidance for AI agents and contributors working on CubePilot.

## Code & Text Language (Preferred)

- **Prefer English for all code, comments, and user-facing strings.** Use English whenever reasonable.
- **Avoid Chinese where English works just as well** - especially in code, comments, placeholders, log messages, UI text, and example values. Chinese may be kept where it is genuinely clearer or expected (e.g. persona files, docs).
- **Prefer ASCII punctuation.** Replace common non-ASCII punctuation with their ASCII equivalents inside code/comments:
  - em-dash `—` -> `--`
  - en-dash `–` -> `-`
  - arrow `→` -> `->`
  - ellipsis `…` -> `...`
  - `≠` -> `!=`, `≈` -> `~=`
- The section symbol `§` is permitted - it is standard Latin notation, not Chinese. The cross-reference it usually carries ("design §3.2") is internal bookkeeping, so see "User-Facing Copy" below for where it may not appear.
- This applies to all file types: Go (`.go`), TypeScript/React (`.ts`/`.tsx`), YAML (`.yaml`/`.yml`/Helm templates), Markdown (`.md`, including `SKILL.md` capability files), shell scripts (`.sh`), Dockerfiles, `Makefile`, and JSON.

## Why

Keeping the entire codebase in English with ASCII punctuation keeps the project consistent, avoids encoding issues across toolchains, and matches the maintainer's convention.

## User-Facing Copy

- User-visible strings (especially the web UI) must not expose internal bookkeeping: no GitHub issue/PR numbers, feature-requirement IDs (e.g. `FR-M2-005`), milestone tags (e.g. `M5`/`M4`), roadmap-phase labels (e.g. "phase one", "Phase One/Three"), or design-doc section references (e.g. "design §3.2").
- **"Text that ships" is wider than the web UI.** For these artifacts a comment *is* content, because they are published to users verbatim - so they must stay equally clean:
  - **CRD schema descriptions.** controller-gen publishes a doc comment on a CRD type or field as the OpenAPI `description`, shown by `kubectl explain` and in the CRD manifest. This reaches further than it looks: a field with no comment of its own inherits its type's comment, and a slice item type publishes its own. Keep every doc comment in `internal/api/v1alpha1/` clean, then regenerate (controller-gen v0.19.0):
    `controller-gen crd paths=./internal/api/... output:crd:artifacts:config=config/crd/bases`
    and copy the result over `deploy/charts/cubepilot-chart/crds/`.
  - **Rendered Helm manifests.** Helm preserves `#` comments, so comments anywhere under `deploy/charts/cubepilot-chart/` are visible in `helm template` and `helm get manifest` output.
  - **Embedded skills.** `internal/skill/skills/*/SKILL.md` is baked into the agent image, read by the agent, and listed in the skill catalog.
  - **`README.md`**, the repository's public front page.
  - **Reader-facing docs.** `docs/cubepilot/api.md` and `docs/cubepilot/api-conventions.md` are the contract for anyone integrating against the API, and `bruno/` is the walkthrough they follow. They number their own sections, so `§N.N` cross-references are out here too - write "第 N 节" or name the heading instead.
- Everywhere else - Go comments that never reach a schema, tests, and the working documents (`docs/cubepilot/cubepilot-design.md`, `implementation-status.md`, `docs/notes/`, `docs/superpowers/`) - internal references are fine, and belong there rather than in a shipped artifact.
- `internal/api/v1alpha1/userfacing_text_test.go` enforces this over the artifacts above; add to its `shippedText` list when a new user-visible artifact appears.

## Working With This Repo

- The core tools run through `exec` -> `kubectl` against the current cluster; consult the capability `SKILL.md` files in `internal/controller/capabilities/` before operating resources.
- Run `go build ./...`, `go vet ./...`, and `go test ./...` after changes; run the web build from `web/` (`npm run build`).