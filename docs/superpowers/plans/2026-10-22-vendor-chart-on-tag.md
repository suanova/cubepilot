# Vendor chart on tags and release branches Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make `vendor-chart.yml` vendor whatever chart this repo generates -- the released `cubepilot-X.Y.Z.tgz` for a tag, the rolling `cubepilot-<version>-latest.tgz` for a branch -- into the matching cubestack-operator branch.

**Architecture:** One new pure shell function for ref -> operator branch mapping (unit-tested and wired into `make test`), then the existing workflow gains tag/release triggers, a target-resolution step, per-target automation branches, and a global concurrency group. All archive/pin/README rewriting stays untouched.

**Tech Stack:** GitHub Actions YAML, bash, helm.

## Global Constraints

- Do not modify `release.yaml` or the `cubestack-operator` repository.
- Only one `cubepilot-*.tgz` may remain in the operator's `assets/charts` after a run.
- PR title must keep the `chore(charts): vendor cubepilot <version>` prefix so the operator auto-merges PRs against `main`.
- Comments: 1-3 lines, explain why, English.

---

### Task 1: Ref -> operator branch resolution script (TDD)

**Files:**
- Create: `scripts/vendor-chart-target.sh`
- Test: `scripts/vendor-chart-target-test.sh`
- Modify: `Makefile:70-72`

**Interfaces:**
- Produces: `scripts/vendor-chart-target.sh <branch|tag> <ref-name>` prints the operator branch (`main`, `release-X.Y`) and exits non-zero for an invalid tag or ref type. Task 2 calls it with `GITHUB_REF_TYPE` and `GITHUB_REF_NAME`.

- [ ] **Step 1: Write the failing test**

Create `scripts/vendor-chart-target-test.sh`:

```bash
#!/usr/bin/env bash
# Unit tests for vendor-chart-target.sh. Run from the repo root: make test.
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
script="${here}/vendor-chart-target.sh"
fail=0

check() {
  local desc=$1 expected=$2
  shift 2
  local actual
  if actual=$("$script" "$@" 2>/dev/null); then :; else actual=FAILED; fi
  if [ "$actual" = "$expected" ]; then
    printf 'ok   %s -> %s\n' "$desc" "$actual"
  else
    printf 'FAIL %s: expected %s, got %s\n' "$desc" "$expected" "$actual" >&2
    fail=1
  fi
}

check "main maps to main"             main         branch main
check "release branch maps to itself" release-2.0  branch release-2.0
check "release tag maps to line"      release-2.0  tag v2.0.0
check "prerelease tag maps to line"   release-2.0  tag v2.0.0-rc1
check "two-digit minor maps"          release-10.20 tag v10.20.30
check "malformed tag fails"           FAILED       tag v2.0
check "unknown ref type fails"        FAILED       tag-or-branch v2.0.0

if [ "$fail" -ne 0 ]; then
  exit 1
fi
echo "all target-resolution cases passed"
```

Make it executable:

```bash
chmod +x scripts/vendor-chart-target-test.sh
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `bash scripts/vendor-chart-target-test.sh`
Expected: FAIL for every case (the script does not exist yet), exit 1.

- [ ] **Step 3: Write the script**

Create `scripts/vendor-chart-target.sh`:

```bash
#!/usr/bin/env bash
# Print the cubestack-operator branch a cubepilot ref vendors into.
# A tag does not record its source branch, so derive release-X.Y from the
# version; a branch maps to the same branch.
set -euo pipefail

ref_type=$1
ref_name=$2

case "$ref_type" in
  branch)
    printf '%s\n' "$ref_name"
    ;;
  tag)
    tag=${ref_name#v}
    # Same SemVer rule as release.yaml: vendor-chart starts alongside release.yaml,
    # so it must reject a malformed tag on its own.
    SEMVER='^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-((0|[1-9][0-9]*|[0-9A-Za-z-]*[A-Za-z-][0-9A-Za-z-]*)(\.(0|[1-9][0-9]*|[0-9A-Za-z-]*[A-Za-z-][0-9A-Za-z-]*))*))?$'
    if ! printf '%s' "$tag" | grep -Eq "$SEMVER"; then
      printf '%s is not a release tag\n' "$ref_name" >&2
      exit 1
    fi
    core=${tag%%-*}
    printf 'release-%s\n' "${core%.*}"
    ;;
  *)
    printf 'unknown ref type: %s\n' "$ref_type" >&2
    exit 1
    ;;
esac
```

Make it executable:

```bash
chmod +x scripts/vendor-chart-target.sh
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `bash scripts/vendor-chart-target-test.sh`
Expected: seven `ok` lines and `all target-resolution cases passed`, exit 0.

- [ ] **Step 5: Wire the test into `make test`**

In `Makefile`, change:

```make
test:
	$(GO) vet ./...
	$(GO) test $(GO_TEST_PACKAGES)
```

to:

```make
test:
	$(GO) vet ./...
	$(GO) test $(GO_TEST_PACKAGES)
	bash scripts/vendor-chart-target-test.sh
```

- [ ] **Step 6: Commit**

```bash
git add scripts/vendor-chart-target.sh scripts/vendor-chart-target-test.sh Makefile
git commit -s -m "test: cover the vendor-chart target-branch mapping

Assisted-by: Claude Code"
```

---

### Task 2: Extend the workflow

**Files:**
- Modify: `.github/workflows/vendor-chart.yml`

**Interfaces:**
- Consumes: `scripts/vendor-chart-target.sh` from Task 1.
- Produces: workflow outputs `steps.target.outputs.branch` (operator base) and `steps.target.outputs.automation-branch`.

- [ ] **Step 1: Replace the header comment (lines 11-21)**

```yaml
# This workflow closes that loop. On a merge to main or a release branch that
# touches the chart it packages the rolling `-latest` chart; on a vX.Y.Z tag it
# packages the released chart with the tag as both version and appVersion. It
# then opens -- or refreshes -- a pull request in the operator that re-vendors
# the archive and its pins, so whatever this repo generated last is what the
# operator pins. That PR is checked by the operator's own PR workflow, which runs
# TestCatalogChartsAreVendored, so the guard is not duplicated here.
#
# A tag does not record the branch it was cut from, so the operator target is
# derived from the version (vX.Y.Z -> release-X.Y). A branch maps to the same
# branch. A target the operator does not have falls back to main.
#
# The archive is packaged here rather than pulled back from Harbor: the pull would
# have to wait on release.yaml's push to land, and a plain `on: push` races it.
```

- [ ] **Step 2: Extend the trigger and serialize runs**

Replace lines 22-41 with:

```yaml
on:
  push:
    branches: [main, 'release-*']
    tags: ['v*']
    # Paths are not evaluated for tag pushes, so a release always vendors;
    # branch pushes vendor only when the chart (or this file) changed.
    paths:
      - 'deploy/charts/cubepilot-chart/**'
      - '.github/workflows/vendor-chart.yml'
  # Not path-filtered, so a dispatch is always answered -- this is how the
  # workflow is exercised on demand. It only becomes dispatchable once it is on
  # the default branch, and the job itself refuses any ref but main.
  workflow_dispatch:

permissions: {}

concurrency:
  # One group across every ref: runs that both fall back to main must not race
  # the same automation branch. Serializing also leaves the most recently
  # generated artifact pinned.
  group: vendor-chart
  cancel-in-progress: false
```

- [ ] **Step 3: Drop the static automation branch from job env**

Replace lines 43-47 with:

```yaml
env:
  TARGET_REPO: suanova/cubestack-operator
  COMPONENT: cubepilot
  ORDER: "80"
```

- [ ] **Step 4: Replace the job gate**

Replace the comment and `if` at lines 53-60 with:

```yaml
    # `workflow_dispatch` can be pointed at any ref; the push filters already
    # restrict pushes, so only a dispatch needs the ref gate. A dispatch on a
    # release branch or tag would package that ref's chart over the rolling
    # branch, which is not what a manual re-run is for.
    if: >-
      (github.event_name == 'push' &&
       (github.ref == 'refs/heads/main' ||
        startsWith(github.ref, 'refs/heads/release-') ||
        startsWith(github.ref, 'refs/tags/v'))) ||
      (github.event_name == 'workflow_dispatch' && github.ref == 'refs/heads/main')
```

- [ ] **Step 5: Teach the package step about tags**

Replace lines 74-88 (comment + first half of the package step) with:

```yaml
      # A branch packages the rolling chart exactly as release.yaml does on a
      # branch push; a tag packages the release exactly as release.yaml does on a
      # tag push, so the vendored content matches the published artifact.
      - name: Package the chart
        id: package
        run: |
          set -euo pipefail
          CHART_DIR=deploy/charts/cubepilot-chart
          if [ "${GITHUB_REF_TYPE}" = "tag" ]; then
            CATALOG_VERSION="${GITHUB_REF_NAME#v}"
            APP_VERSION="${CATALOG_VERSION}"
          else
            CHART_VERSION="$(helm show chart "${CHART_DIR}" | awk '/^version:/{print $2}')"
            test -n "${CHART_VERSION}"
            CATALOG_VERSION="${CHART_VERSION}-latest"
            APP_VERSION=latest
          fi
          helm package "${CHART_DIR}" \
            --version "${CATALOG_VERSION}" --app-version "${APP_VERSION}" \
            --destination "${RUNNER_TEMP}"
```

Leave the rest of the step (archive assertions and outputs) unchanged.

- [ ] **Step 6: Add the target-resolution step**

After the `Create an installation token for the bot` step (line 110) and before `Clone the target repo`, insert:

```yaml
      - name: Resolve the target branch
        id: target
        env:
          GH_TOKEN: ${{ steps.app-token.outputs.token }}
        run: |
          set -euo pipefail
          candidate="$(scripts/vendor-chart-target.sh "${GITHUB_REF_TYPE}" "${GITHUB_REF_NAME}")"
          if [ "$candidate" != "main" ]; then
            # --include so a transient API failure is not mistaken for a missing
            # branch and silently redirected to main.
            status="$(gh api --include "repos/${TARGET_REPO}/branches/${candidate}" 2>/dev/null \
              | awk 'NR==1{print $2}')" || true
            case "$status" in
              200) ;;
              404)
                echo "::notice::${TARGET_REPO} has no ${candidate} branch; vendoring to main"
                candidate=main
                ;;
              *)
                echo "::error::cannot check ${TARGET_REPO} branch ${candidate} (HTTP ${status:-none})"
                exit 1
                ;;
            esac
          fi
          echo "branch=${candidate}" >> "${GITHUB_OUTPUT}"
          # main keeps its existing automation branch so an already-open PR is
          # refreshed instead of orphaned; every other target gets its own.
          if [ "$candidate" = "main" ]; then
            echo "automation-branch=automation/vendor-cubepilot-chart" >> "${GITHUB_OUTPUT}"
          else
            echo "automation-branch=automation/vendor-cubepilot-chart-${candidate}" >> "${GITHUB_OUTPUT}"
          fi
```

- [ ] **Step 7: Check out the resolved operator branch**

In `Clone the target repo`, replace `ref: main` with:

```yaml
          ref: ${{ steps.target.outputs.branch }}
```

- [ ] **Step 8: Use the per-target automation branch**

In `Record the rolling branch's current tip`, add to `env`:

```yaml
          TARGET_BRANCH: ${{ steps.target.outputs.automation-branch }}
```

In `Commit and push the branch`, add the same line to `env`. Update the comment block above `target_ref` so "re-derived from main" reads "re-derived from its target branch".

In `Open or refresh the pull request`, add to `env`:

```yaml
          TARGET_BRANCH: ${{ steps.target.outputs.automation-branch }}
          BASE_BRANCH: ${{ steps.target.outputs.branch }}
```

- [ ] **Step 9: Update the PR body and base**

In the PR body step, replace the single `printf` at the start of the body with:

```bash
          {
            if [ "${GITHUB_REF_TYPE}" = "tag" ]; then
              printf 'The chart was released as `%s`, so re-vendor it.\n\n' \
                "${GITHUB_REF_NAME}"
            else
              printf 'The chart changed on `%s@%s`, so re-vendor it.\n\n' \
                "$GITHUB_REPOSITORY" "${GITHUB_SHA:0:7}"
            fi
            printf -- '- `assets/charts/%s-%s.tgz` -- the archive, packaged from the chart at that commit\n- `assets/components/%s-%s.yaml` -- the `oci.version` pin, which is the file name the operator resolves\n- `assets/charts/README.md` and `README.md` -- the vendored-archive tables\n\nOpened by the `suanova-bot` App from [`.github/workflows/vendor-chart.yml`](https://github.com/%s/blob/main/.github/workflows/vendor-chart.yml). The archive is `//go:embed`ed into the operator binary, so it is committed here rather than pulled at reconcile time; merging this fires `publish.yml`, which is what actually puts the new chart in front of clusters.\n' \
              "$COMPONENT" "$CATALOG_VERSION" "$ORDER" "$COMPONENT" "$GITHUB_REPOSITORY"
          } > "$body"
```

The chart-diff append after this block stays as it is.

Then replace the create call:

```bash
          gh pr create --repo "$TARGET_REPO" --base "$BASE_BRANCH" --head "$TARGET_BRANCH" \
            --title "$title" --body-file "$body"
```

- [ ] **Step 10: Verify the workflow parses and the test passes**

Run: `python3 -c "import yaml; yaml.safe_load(open('.github/workflows/vendor-chart.yml'))"`
Expected: no output, exit 0.

Run: `bash scripts/vendor-chart-target-test.sh`
Expected: `all target-resolution cases passed`.

Run: `grep -n "TARGET_BRANCH: automation" .github/workflows/vendor-chart.yml`
Expected: no output (the static branch is gone).

- [ ] **Step 11: Commit**

```bash
git add .github/workflows/vendor-chart.yml
git commit -s -m "ci(charts): vendor the generated chart on tags and release branches

Assisted-by: Claude Code"
```

---

### Task 3: Prove the vendored file replacement locally

**Files:** none (scratch only)

- [ ] **Step 1: Recreate the operator's chart files in a scratch tree**

```bash
scratch=$(mktemp -d)
mkdir -p "$scratch/assets/charts" "$scratch/assets/components"
printf 'old\n' > "$scratch/assets/charts/cubepilot-2.0.0-latest.tgz"
printf '| 80 | `cubepilot` | `2.0.0-latest` | `cubepilot-2.0.0-latest.tgz` | `cubepilot-chart` 2.0.0-latest | cubestack repo CI (OCI) |\n' > "$scratch/assets/charts/README.md"
printf '| cubepilot | `assets/charts/cubepilot-2.0.0-latest.tgz` |\n' > "$scratch/README.md"
printf 'oci:\n  ref: oci://harbor.isuanova.com/suanova/cubepilot-chart\n  version: 2.0.0-latest\n' > "$scratch/assets/components/80-cubepilot.yaml"
echo "$scratch"
```

- [ ] **Step 2: Run the workflow's replacement logic for a release**

Copy the `Re-vendor the archive and its pins` `run:` block from the workflow into a scratch script with `COMPONENT=cubepilot`, `ORDER=80`, `CATALOG_VERSION=2.0.0`, and a fresh `cubepilot-chart-2.0.0.tgz` created by `helm package deploy/charts/cubepilot-chart --version 2.0.0 --app-version 2.0.0 -d "$RUNNER_TEMP"`. Run it with `working-directory` set to the scratch tree.

Expected after the run:

```bash
ls "$scratch/assets/charts"/cubepilot-*.tgz
# only assets/charts/cubepilot-2.0.0.tgz
grep 'version:' "$scratch/assets/components/80-cubepilot.yaml"
#   version: 2.0.0
grep '2.0.0.tgz' "$scratch/assets/charts/README.md" "$scratch/README.md"
# both tables name the release archive
```

- [ ] **Step 3: Repeat for the next rolling chart**

Package as `2.0.1-latest` and run the same block with `CATALOG_VERSION=2.0.1-latest`. Expected: `cubepilot-2.0.0.tgz` is removed, `cubepilot-2.0.1-latest.tgz` is the only archive, and the pin is `2.0.1-latest`.

---

### Task 4: Push and verify in CI

- [ ] **Step 1: Rebase on the merged main and push**

```bash
git fetch upstream
git rebase upstream/main
git push --force-with-lease origin HEAD
```

- [ ] **Step 2: Watch checks**

Run: `gh pr checks 277 -R suanova/cubepilot --watch`
Expected: Unit tests (including the new shell test), Web build, E2E, DCO all pass.

- [ ] **Step 3: End-to-end after merge (not in this PR)**

1. Merge this PR. The merge touches the workflow file, so the main path runs and vendors `cubepilot-2.0.0-latest.tgz`.
2. Create `release-2.0` from the merged main and push `v2.0.0`. The tag path runs, vendors `cubepilot-2.0.0.tgz`, and replaces the `-latest` archive in the same operator PR. The operator has no `release-2.0` branch, so the PR targets `main` and auto-merges.
3. Confirm in `cubestack-operator/assets/charts` that exactly one `cubepilot-*.tgz` remains and it is `cubepilot-2.0.0.tgz`.
