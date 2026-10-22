# Vendor the generated chart into cubestack-operator, whatever generated it

## Problem

`cubestack-operator` vendors this repo's chart as `assets/charts/cubepilot-<pin>.tgz`
and pins it in `assets/components/80-cubepilot.yaml`. Today only one thing refreshes
that archive: `.github/workflows/vendor-chart.yml`, triggered by a push to `main` that
touches the chart. It always packages the chart as `<Chart.yaml version>-latest`.

Two consequences:

1. A tag release never reaches the operator. `v2.0.0` publishes
   `cubepilot-chart:2.0.0`, while the operator keeps pinning `2.0.0-latest` (or an
   older `-latest`), so a released, immutable version is never what the operator
   installs.
2. A release branch has no path to the operator. Once `release-2.0` exists, a
   cherry-picked chart fix on that branch has no way to update the operator's
   matching line.

The desired invariant is simpler than both: **whatever chart artifact this repo
generates, the operator pins that artifact** -- release or rolling, on the matching
branch.

## Trigger

```yaml
on:
  push:
    branches: [main, 'release-*']
    tags: ['v*']
    paths:
      - 'deploy/charts/cubepilot-chart/**'
      - '.github/workflows/vendor-chart.yml'
  workflow_dispatch:   # still main only
```

GitHub does not evaluate `paths` filters for tag pushes, so every `v*` tag triggers
the workflow regardless of which files the tag touches. Branch pushes keep the path
filter.

The job gate becomes: run for a push to `main`, a push to `release-*`, or a push of a
`v*` tag; a `workflow_dispatch` is still accepted only on `main`, as today.

## Version and artifact

| Trigger | Chart package | Vendored file |
|---|---|---|
| push `main` or `release-X.Y` | `--version <Chart.yaml version>-latest --app-version latest` | `cubepilot-<Chart.yaml version>-latest.tgz` |
| push tag `vX.Y.Z` (or `-<prerelease>`) | `--version X.Y.Z --app-version X.Y.Z` | `cubepilot-X.Y.Z.tgz` |

The tag path strips the leading `v` before validation and packaging: `v2.0.0`
is validated and built as `2.0.0`. It uses the same SemVer regex `release.yaml`
uses. Duplicating the regex is deliberate: the two workflows run concurrently on
a tag push, and vendor-chart must reject a malformed tag even if `release.yaml`
fails before its own validation reports it.

The existing `cubepilot-[0-9]*.tgz` cleanup, catalog `oci.version` rewrite, and the
two README table rewrites are reused unchanged. Only one `cubepilot-*.tgz` ever
remains in `assets/charts`, so a release replaces the previous `-latest` archive
rather than sitting beside it.

The operator's `test/e2e/images.txt` moves with the pin. Its
`TestOwnImagesArePreloaded` asserts that the list names exactly the images the
chart renders, and an empty image tag resolves to the chart's appVersion, so a
release rewrites the cubepilot tags from `:latest` to `:X.Y.Z` and the next branch
build rewrites them back.

The chart is packaged locally rather than pulled from Harbor. A tag push starts
`release.yaml` and this workflow at the same time; pulling would race the chart push.
`helm package --version X --app-version X` is exactly how `release.yaml` builds the
published artifact, so the content is identical (tar mtimes differ, which is already
the accepted trade-off for the `main` path).

## Target branch

The operator PR targets the operator branch that matches this repo's line:

| This repo | Operator PR base |
|---|---|
| `main` | `main` |
| `release-X.Y` | `release-X.Y`, or `main` if that branch does not exist |
| tag `vX.Y.Z` / `vX.Y.Z-<prerelease>` | `release-X.Y`, or `main` if that branch does not exist |

A tag does not record which branch it came from, so `X.Y` is derived from the version
rather than read from git. The existence check uses the App token against the
operator API; a missing branch falls back to `main` and logs why, so a release is
never blocked by the operator not having created its release branch yet.

Each target gets its own automation branch, so two lines cannot overwrite each
other's PR:

- `main` keeps `automation/vendor-cubepilot-chart` (existing open PRs keep working).
- Other targets use `automation/vendor-cubepilot-chart-<target>`, e.g.
  `automation/vendor-cubepilot-chart-release-2.0`.

The workflow checks out the target branch, pushes the automation branch into the
operator repository, and opens or refreshes the PR against that base -- the same
App-token flow as today.

## Ordering and merge behavior

`vendor-chart` uses one global concurrency group with `cancel-in-progress: false`.
Runs across `main`, `release-*`, and tags therefore serialize, and the most recently
generated artifact is the one left pinned. Without this, a tag run and a main run
that both fall back to `main` would race the same automation branch and one would be
rejected by the `force-with-lease` push.

GitHub keeps at most one *pending* run per group: an in-progress run is never
cancelled, but a newer queued run replaces an older pending one. A tag release run
that is still pending when a newer chart-changing run queues is therefore dropped,
and the newer artifact wins. That is the intended "last generated wins" invariant,
not a lost release: the tag can be re-pushed to regenerate its artifact.

GitHub does not guarantee FIFO order among queued runs, so a run that started
earlier could execute after a newer one and win. That residual race is accepted:
both artifacts are valid charts of this repo, and re-pushing the newer tag (or
re-running the workflow) re-asserts it. A cross-repo freshness marker would add
state to the operator for a race that self-corrects.

The PR title stays `chore(charts): vendor cubepilot <version>`, so PRs against
`main` are still auto-merged by the operator's `auto-merge-bot-pr.yml`. That workflow
hard-codes `BASE_BRANCH: main`; PRs against `release-*` are merged by a maintainer.
Changing that is an operator-side decision and is out of scope here.

## Failure behavior

If `release.yaml` fails after this workflow has already opened its PR, the operator
can pin a chart whose images were not published. This is the same trade-off the
existing `main` path already accepts; gating on a Harbor existence check would add
credentials and retries for a case that is re-run by pushing the tag again.

## Verification

- Parse the workflow YAML locally.
- Exercise target resolution for each shape: `main`, `release-2.0`, tag `v2.0.0`,
  prerelease tag, and a target branch that does not exist in the operator.
- Copy the operator's `assets/` and `README.md` into a scratch tree and run the
  replacement logic for the `-latest` -> release and release -> next `-latest`
  transitions; check that exactly one cubepilot archive remains, the catalog pin
  names it, and both README rows point at it.
- End to end, in order:
  1. Merge this change and the chart version bump to `main`. The merge triggers the
     `main` path, which vendors `cubepilot-2.0.0-latest.tgz`.
  2. Create `release-2.0` from the merged `main` and push `v2.0.0`. The tag run
     vendors `cubepilot-2.0.0.tgz`, replacing the `-latest` archive. The operator
     has no `release-2.0` branch yet, so the PR falls back to `main` and auto-merges.

## Out of scope

- Changing the operator's auto-merge workflow to accept `release-*` bases.
- Pulling the published chart back from Harbor instead of repackaging it.
- Vendoring the operator chart back into cubepilot or any other component's assets.
