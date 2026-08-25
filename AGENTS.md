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
- The section symbol `§` (used for design-doc cross-references like "design §3.2") is permitted - it is standard Latin notation, not Chinese.
- This applies to all file types: Go (`.go`), TypeScript/React (`.ts`/`.tsx`), YAML (`.yaml`/`.yml`/Helm templates), Markdown (`.md`, including `SKILL.md` capability files), shell scripts (`.sh`), Dockerfiles, `Makefile`, and JSON.

## Why

Keeping the entire codebase in English with ASCII punctuation keeps the project consistent, avoids encoding issues across toolchains, and matches the maintainer's convention.

## Working With This Repo

- The core tools run through `exec` -> `kubectl` against the current cluster; consult the capability `SKILL.md` files in `internal/controller/capabilities/` before operating resources.
- Run `go build ./...`, `go vet ./...`, and `go test ./...` after changes; run the web build from `web/` (`npm run build`).