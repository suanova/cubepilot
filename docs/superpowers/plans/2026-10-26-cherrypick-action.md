# Cherry-pick-by-comment Workflow Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let a maintainer comment `/cherrypick <branch>` on a merged `main` PR and get a reviewable PR carrying the same change on each named branch.

**Architecture:** An `issue_comment`-triggered workflow with two jobs. `prepare` validates the commenter's repository permission, the source PR, and every target branch with the read-only `GITHUB_TOKEN`. `cherrypick` mints the existing `SUANOVA_BOT` App token, checks out with it (git push uses these credentials), and delegates the cherry-pick and PR creation to `korthout/backport-action@v4`.

**Tech Stack:** GitHub Actions (YAML), bash, `gh`, `korthout/backport-action@v4`, `actions/create-github-app-token`, `actionlint`.

## Global Constraints

- Trigger on `issue_comment: [created]` only; an edited comment must not re-run.
- Source PR must be merged and its base must be `main`.
- Target accepted when it is a valid git branch name, exists on the remote, and is not `main`; no naming pattern such as `release-*` is required.
- Commenter must have `admin`, `maintain`, or `write` permission on the repository; do not rely on `author_association` (`MEMBER` does not imply write access on a public repo).
- Unauthorized commenters get no bot reply.
- Passing `GITHUB_TOKEN`-created PRs do not trigger CI: the cherry-pick PR must be created with a `SUANOVA_BOT` App token.
- Never auto-merge; no Mergify rule change.
- Comment text reaches shell only through `env`, never through `${{ }}` inside `run:`.
- Anything written to GitHub (comments, PR text, commit messages) is in English.

---

### Task 1: Target-branch parser and its tests

**Files:**
- Create: `scripts/cherrypick-targets.sh`
- Create: `scripts/cherrypick-targets_test.sh`
- Modify: `.github/workflows/ci.yaml` (append a step to the `test` job)

**Interfaces:**
- Consumes: nothing.
- Produces: `scripts/cherrypick-targets.sh` reads a comment body on stdin and prints target branches space-separated on one line. Exit 0 on success; exit 1 with a user-facing reason on stderr when the command is malformed, targets `main`, or names an invalid ref. The workflow in Task 2 calls it as `printf '%s' "$COMMENT_BODY" | scripts/cherrypick-targets.sh`.

- [ ] **Step 1: Write the failing test**

Create `scripts/cherrypick-targets_test.sh`:

```bash
#!/usr/bin/env bash
# Cases for the /cherrypick comment parser. Run directly; exits non-zero on
# the first aggregate failure so it can gate CI.
set -euo pipefail

script="$(cd "$(dirname "$0")" && pwd)/cherrypick-targets.sh"
failures=0

check() { # check <name> <want-exit> <want-output> <comment-body>
  local name=$1 want_code=$2 want_out=$3 body=$4 got_code=0 got_out
  got_out=$(printf '%s' "$body" | "$script" 2>&1) || got_code=$?
  if [ "$got_code" != "$want_code" ] || [ "$got_out" != "$want_out" ]; then
    printf 'FAIL %s\n  want exit %s, output: %s\n  got  exit %s, output: %s\n' \
      "$name" "$want_code" "$want_out" "$got_code" "$got_out"
    failures=$((failures + 1))
  else
    printf 'ok   %s\n' "$name"
  fi
}

usage="expected '/cherrypick <target-branch>...' on the first line"

check single            0 'release-2.0'                '/cherrypick release-2.0'
check multiple          0 'release-2.0 release-2.1'    '/cherrypick release-2.0 release-2.1'
check first-line-only   0 'release-2.0'                $'/cherrypick release-2.0\nanything below is ignored'
check crlf              0 'release-2.0'                $'/cherrypick release-2.0\r'
check extra-spaces      0 'release-2.0'                '/cherrypick     release-2.0'
check any-branch-name   0 'hotfix/payments'            '/cherrypick hotfix/payments'
check other-command     1 "$usage"                     '/cherrypickx release-2.0'
check no-argument       1 "$usage"                     '/cherrypick'
check no-command        1 "$usage"                     'please /cherrypick release-2.0'
check main              1 'cannot cherry-pick into main' '/cherrypick main'
check invalid-ref       1 "'bad..name' is not a valid branch name" '/cherrypick bad..name'
check option-like-ref   1 "'-x' is not a valid branch name"        '/cherrypick -x'
check one-bad-of-two    1 "'bad..name' is not a valid branch name" '/cherrypick release-2.0 bad..name'

if [ "$failures" -ne 0 ]; then
  printf '%s case(s) failed\n' "$failures" >&2
  exit 1
fi
echo 'all cherrypick-targets cases passed'
```

- [ ] **Step 2: Run the test to verify it fails**

Run:

```bash
chmod +x scripts/cherrypick-targets_test.sh
scripts/cherrypick-targets_test.sh
```

Expected: every case prints `FAIL` and the run exits 1 with `<n> case(s) failed`, because `scripts/cherrypick-targets.sh` does not exist yet.

- [ ] **Step 3: Write the parser**

Create `scripts/cherrypick-targets.sh`:

```bash
#!/usr/bin/env bash
# Parse the target branches from a /cherrypick comment. Only the first line is
# read; branches are whitespace-separated. Prints them space-separated so the
# caller can pass the result straight to backport-action's target_branches.
set -euo pipefail

first_line=$(head -n1)
first_line=${first_line%$'\r'}   # a browser may submit CRLF

read -r command rest <<<"$first_line" || true
if [ "$command" != /cherrypick ] || [ -z "${rest//[[:space:]]/}" ]; then
  echo "expected '/cherrypick <target-branch>...' on the first line" >&2
  exit 1
fi

targets=()
for branch in $rest; do
  if [ "$branch" = main ]; then
    echo 'cannot cherry-pick into main' >&2
    exit 1
  fi
  if ! git check-ref-format --branch "$branch" >/dev/null 2>&1; then
    echo "'$branch' is not a valid branch name" >&2
    exit 1
  fi
  targets+=("$branch")
done

printf '%s' "${targets[*]}"
```

- [ ] **Step 4: Run the tests to verify they pass**

Run:

```bash
chmod +x scripts/cherrypick-targets.sh
scripts/cherrypick-targets_test.sh
```

Expected:

```text
ok   single
ok   multiple
ok   first-line-only
ok   crlf
ok   extra-spaces
ok   any-branch-name
ok   other-command
ok   no-argument
ok   no-command
ok   main
ok   invalid-ref
ok   option-like-ref
ok   one-bad-of-two
all cherrypick-targets cases passed
```

- [ ] **Step 5: Wire the test into CI**

In `.github/workflows/ci.yaml`, inside the `test` job, insert this step after the `go mod verify` step and before the `golangci-lint` step:

```yaml
      - name: cherrypick target parser test
        if: steps.changes.outputs.code == 'true' || github.event_name == 'workflow_dispatch'
        run: scripts/cherrypick-targets_test.sh
```

- [ ] **Step 6: Validate the edited workflow**

Run:

```bash
go run github.com/rhysd/actionlint/cmd/actionlint@latest .github/workflows/ci.yaml
```

Expected: no output, exit status 0.

- [ ] **Step 7: Commit**

```bash
git add scripts/cherrypick-targets.sh scripts/cherrypick-targets_test.sh .github/workflows/ci.yaml
git commit -s -m "ci: parse /cherrypick targets in a tested script

Assisted-by: Claude Code"
```

---

### Task 2: The cherrypick workflow

**Files:**
- Create: `.github/workflows/cherrypick.yaml`

**Interfaces:**
- Consumes: `scripts/cherrypick-targets.sh` from Task 1.
- Produces: `prepare` job output `targets` (space-separated branches, empty when the command is rejected); the `cherrypick` job consumes it as `needs.prepare.outputs.targets`.

- [ ] **Step 1: Write the workflow**

Create `.github/workflows/cherrypick.yaml`:

```yaml
# Cherry-pick a merged main PR to other branches on request:
#
#   /cherrypick release-2.0
#
# The comment must be on a merged PR whose base is main, by someone with write
# access. Each target branch must already exist. The cherry-pick keeps the
# original commit reference (-x) and opens a PR for human review; nothing is
# merged automatically. The design is in
# docs/superpowers/specs/2026-10-26-cherrypick-action-design.md.

name: cherrypick

on:
  issue_comment:
    types: [created]

permissions:
  contents: read         # actions/checkout + git ls-remote
  pull-requests: read    # gh pr view on the source PR
  issues: write          # rejection comment on the source PR

jobs:
  prepare:
    name: Validate command
    # The body is parsed again below; this only skips unrelated comments.
    if: >
      github.event.issue.pull_request &&
      github.event.issue.state == 'closed' &&
      startsWith(github.event.comment.body, '/cherrypick ')
    runs-on: ubuntu-latest
    timeout-minutes: 5
    outputs:
      targets: ${{ steps.validate.outputs.targets }}
    steps:
      - uses: actions/checkout@v7

      - name: Validate comment
        id: validate
        env:
          GH_TOKEN: ${{ github.token }}
          REPO: ${{ github.repository }}
          PR_NUMBER: ${{ github.event.issue.number }}
          ACTOR: ${{ github.event.comment.user.login }}
          COMMENT_BODY: ${{ github.event.comment.body }}
        run: |
          set -euo pipefail

          reply()  { gh pr comment "$PR_NUMBER" --repo "$REPO" --body "$1"; }
          reject() { reply "Cherry-pick skipped: $1"; exit 0; }

          # On a public repo author_association=MEMBER does not imply write
          # access, so ask for the repository permission instead.
          permission=$(gh api "repos/$REPO/collaborators/$ACTOR/permission" --jq .permission 2>/dev/null || true)
          case "$permission" in
            admin|maintain|write) ;;
            *) exit 0 ;;   # silent for arbitrary commenters
          esac

          # The parser reads only the first line and rejects an invalid target.
          if ! targets=$(printf '%s' "$COMMENT_BODY" | scripts/cherrypick-targets.sh 2>&1); then
            reject "$targets"
          fi

          info=$(gh pr view "$PR_NUMBER" --repo "$REPO" --json mergedAt,baseRefName)
          if [ "$(jq -r 'if .mergedAt then "true" else "false" end' <<<"$info")" != true ]; then
            reject "PR #$PR_NUMBER is not merged."
          fi
          base=$(jq -r .baseRefName <<<"$info")
          if [ "$base" != main ]; then
            reject "only PRs merged into main can be cherry-picked (this one targets '$base')."
          fi

          # ls-remote avoids URL-encoding a branch name with slashes in an API path.
          for branch in $targets; do
            if ! git ls-remote --exit-code --heads origin "refs/heads/$branch" >/dev/null 2>&1; then
              reject "target branch '$branch' does not exist."
            fi
          done

          echo "targets=$targets" >> "$GITHUB_OUTPUT"

  cherrypick:
    name: Cherry-pick to ${{ needs.prepare.outputs.targets }}
    needs: prepare
    if: needs.prepare.outputs.targets != ''
    runs-on: ubuntu-latest
    timeout-minutes: 10
    steps:
      # A PR opened with GITHUB_TOKEN triggers no workflows, so its CI would
      # never run. Use the bot App token; issues:write lets the action comment.
      - name: Mint SUANOVA_BOT token
        id: app-token
        uses: actions/create-github-app-token@v3
        with:
          client-id: ${{ vars.SUANOVA_BOT_CLIENT_ID }}
          private-key: ${{ secrets.SUANOVA_BOT_APP_PRIVATE_KEY }}
          owner: suanova
          repositories: cubepilot
          permission-contents: write
          permission-pull-requests: write
          permission-issues: write

      # git push authenticates through the checkout credentials, not the
      # action's github_token, so the App token has to go here too.
      - uses: actions/checkout@v7
        with:
          token: ${{ steps.app-token.outputs.token }}

      - name: Create cherry-pick PRs
        uses: korthout/backport-action@v4
        with:
          github_token: ${{ steps.app-token.outputs.token }}
          source_pr_number: ${{ github.event.issue.number }}
          target_branches: ${{ needs.prepare.outputs.targets }}
          cherry_picking: auto
          branch_name: 'cherrypick-${pull_number}-to-${target_branch}'
          pull_title: '[cherrypick ${target_branch}] ${pull_title}'
          comment_style: summary
```

- [ ] **Step 2: Lint the workflow**

Run:

```bash
go run github.com/rhysd/actionlint/cmd/actionlint@latest .github/workflows/cherrypick.yaml
```

Expected: no output, exit status 0.

- [ ] **Step 3: Check the comment gating expression by hand**

Run:

```bash
grep -n "issue_comment\|startsWith\|author_association\|collaborators" .github/workflows/cherrypick.yaml
```

Expected: the event is `issue_comment` with `types: [created]`; there is no `author_association` check; the permission API call is present.

- [ ] **Step 4: Run the parser tests again**

Run:

```bash
scripts/cherrypick-targets_test.sh
```

Expected: `all cherrypick-targets cases passed`.

- [ ] **Step 5: Commit**

```bash
git add .github/workflows/cherrypick.yaml
git commit -s -m "ci: cherry-pick merged PRs to a named branch by comment

Assisted-by: Claude Code"
```

---

### Task 3: Document the command

**Files:**
- Modify: `README.md` (after the "Releases (CI)" section, before "## Run")

**Interfaces:**
- Consumes: the command semantics from Task 2.
- Produces: nothing code-related.

- [ ] **Step 1: Insert the subsection**

Read `README.md` to locate the end of the "Releases (CI)" section (before `## Run`). Insert:

```markdown
### Cherry-picking to a release branch

After a PR is merged into `main`, a maintainer can ask for the same change on
another branch by commenting on the merged PR:

```
/cherrypick release-2.0
```

The `cherrypick` workflow checks that the commenter has write access, then
cherry-picks the change with `-x` and opens a PR against each target branch
(separate several targets with spaces on the same line). A target must already
exist and must not be `main`. The generated PR runs the normal checks and is
merged by hand — nothing is merged automatically. Repeating the command updates
the existing PR, and a conflict is reported on the original PR instead of
opening a half-applied PR.
```

- [ ] **Step 2: Check the section renders in the right place**

Run:

```bash
grep -n "^### Cherry-picking to a release branch\|^## Run" README.md
```

Expected: the new heading appears immediately before `## Run`, with a smaller line number than `## Run`.

- [ ] **Step 3: Commit**

```bash
git add README.md
git commit -s -m "docs: describe /cherrypick on a merged PR

Assisted-by: Claude Code"
```

---

### Task 4: Verify against the real repository after merge (manual)

This task cannot run before the workflow is on `main`, because `issue_comment`
workflows are loaded from the default branch.

**Files:** none.

- [ ] **Step 1: Confirm the App installation**

In the `suanova` organization's GitHub App settings, confirm `SUANOVA_BOT` is installed on `cubepilot` with `contents: write`, `pull-requests: write`, and `issues: write`. Without these the token mint or PR creation fails.

- [ ] **Step 2: Happy path**

Pick a recent PR merged into `main`, comment `/cherrypick release-2.0`, and verify:

- the `cherrypick` workflow run succeeds;
- one summary comment appears on the original PR with a link to the new PR;
- the new PR is `cherrypick-<number>-to-release-2.0` against `release-2.0`, titled `[cherrypick release-2.0] <original title>`;
- the new PR's commits contain the original commit reference (`git log -1` shows a `(cherry picked from commit ...)` line);
- the new PR runs `ci` and `e2e` (not stuck at "Expected");
- repeating the comment updates the existing PR rather than opening a second one.

- [ ] **Step 3: Negative paths**

Verify each on the same or another merged PR:

- `/cherrypick main` replies `Cherry-pick skipped: cannot cherry-pick into main`.
- `/cherrypick does-not-exist` replies `Cherry-pick skipped: target branch 'does-not-exist' does not exist.`
- A comment by a user without write access produces no bot reply and no workflow run beyond `prepare`.
- A comment on a closed-but-unmerged PR replies that the PR is not merged.
- A comment on a PR whose base is a release branch replies that only main PRs can be cherry-picked.
- A conflicting backport reports the conflict on the original PR and pushes no branch.
