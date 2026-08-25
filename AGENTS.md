# AGENTS.md - Project Conventions

This file is the operating guidance for AI agents and contributors working on CubePilot.

## Code & Text Language (Mandatory)

- **All code, comments, commit messages, and user-facing strings must be in English.**
- **Do not use Chinese (简体中文 / Traditional) anywhere in the codebase** - not even as comments, placeholders, log messages, UI text, or example values.
- **Use ASCII punctuation only.** Replace common non-ASCII punctuation with their ASCII equivalents inside code/comments:
  - em-dash `—` -> `--`
  - en-dash `–` -> `-`
  - arrow `→` -> `->`
  - ellipsis `…` -> `...`
  - `≠` -> `!=`, `≈` -> `~=`
- The section symbol `§` (used for design-doc cross-references like "design §3.2") is permitted - it is standard Latin notation, not Chinese.
- This applies to all file types: Go (`.go`), TypeScript/React (`.ts`/`.tsx`), YAML (`.yaml`/`.yml`/Helm templates), Markdown (`.md`, including `SKILL.md` capability files), shell scripts (`.sh`), Dockerfiles, `Makefile`, and JSON.
- **Exception:** the `docs/` directory holds Chinese-language design deliverables and is intentionally not converted.

## Why

Keeping the entire codebase in English with ASCII punctuation keeps the project consistent, avoids encoding issues across toolchains, and matches the maintainer's convention.

## Working With This Repo

- The core tools run through `exec` -> `kubectl` against the current cluster; consult the capability `SKILL.md` files in `internal/controller/capabilities/` before operating resources.
- Run `go build ./...`, `go vet ./...`, and `go test ./...` after changes; run the web build from `web/` (`npm run build`).