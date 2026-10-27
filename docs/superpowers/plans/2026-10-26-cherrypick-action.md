# Cherry-pick-by-comment Workflow Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let a maintainer comment `/cherrypick <branch>` on a merged `main` PR and get a reviewable PR carrying the same change on each named branch.

**Architecture:** An `issue_comment`-triggered workflow with two jobs. `prepare` validates the commenter's repository permission, the source PR, and every target branch under job-scoped `GITHUB_TOKEN` permissions (`contents: read`, `pull-requests: write`, `issues: write`). `cherrypick` runs with no `GITHUB_TOKEN` permissions (`permissions: {}`) and mints the existing `SUANOVA_BOT` App token, checks out with it (git push uses these credentials), and delegates the cherry-pick and PR creation to `korthout/backport-action` (pinned to a v4 SHA).

**Tech Stack:** GitHub Actions (YAML), bash, `gh`, `korthout/backport-action` (SHA-pinned), `actions/create-github-app-token`, `actionlint`.

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

### Task 1: Parser logic (developed, then inlined)

The parser reads only the first line of the comment body. The command must be
exactly `/cherrypick`, followed by one or more whitespace-separated target
branches. Each target must be a syntactically valid git branch name
(`git check-ref-format --branch`) and must not be `main`. Pathname expansion is
disabled, so a target such as `release-*` is kept literal and rejected as an
invalid ref; one invalid target rejects the whole command.

Eighteen local cases validated these rules: single and multiple targets, a
first-line-only comment, CRLF, extra whitespace, branch names with slashes,
wrong/missing command, missing argument, `main`, invalid and option-like refs, a
bad target among several, glob targets, empty input, duplicates, tab
separation, and a real glob-expansion check.

The final code lives inline in `.github/workflows/cherrypick.yaml` (the
"Validate comment" step); no parser script or separate test file remains.

---

### Task 2: The cherrypick workflow

**Files:**
- Create: `.github/workflows/cherrypick.yaml`

**Interfaces:**
- Consumes: nothing (the parser is inline).
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

permissions: {}

concurrency:
  group: cherrypick-${{ github.event.issue.number }}
  cancel-in-progress: false
  # Workflow-level concurrency also sees ordinary comments (including the bot's
  # own summary); the default single pending slot would let a later one cancel a queued run.
  queue: max

jobs:
  prepare:
    name: Validate command
    # PR conversation comments (gh pr comment) need pull-requests:write, not
    # only issues:write.
    permissions:
      contents: read
      pull-requests: write   # gh pr comment on the source PR
      issues: write          # PR conversation comments also accept the issues scope
    # The body is parsed again below; this only skips unrelated comments.
    if: >
      github.event.issue.pull_request &&
      startsWith(github.event.comment.body, '/cherrypick')
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

          # Only the first line is read. Glob characters are invalid in refs, so
          # disable pathname expansion and keep a target like 'release-*' literal.
          set -f
          first_line=${COMMENT_BODY%%$'\n'*}
          first_line=${first_line%$'\r'}
          read -r command rest <<<"$first_line" || true
          if [ "$command" != /cherrypick ] || [ -z "${rest//[[:space:]]/}" ]; then
            reject "expected '/cherrypick <target-branch>...' on the first line"
          fi
          targets=()
          for branch in $rest; do
            if [ "$branch" = main ]; then
              reject 'cannot cherry-pick into main'
            fi
            if ! git check-ref-format --branch "$branch" >/dev/null 2>&1; then
              reject "'$branch' is not a valid branch name"
            fi
            targets+=("$branch")
          done
          targets="${targets[*]}"

          info=$(gh pr view "$PR_NUMBER" --repo "$REPO" --json mergedAt,baseRefName,headRefName,isCrossRepository)
          if [ "$(jq -r 'if .mergedAt then "true" else "false" end' <<<"$info")" != true ]; then
            reject "PR #$PR_NUMBER is not merged."
          fi
          base=$(jq -r .baseRefName <<<"$info")
          if [ "$base" != main ]; then
            reject "only PRs merged into main can be cherry-picked (this one targets '$base')."
          fi

          # The action drops any target equal to the head branch name, even when
          # a fork's head branch only shares that name.
          if [ "$(jq -r .isCrossRepository <<<"$info")" = true ]; then
            head=$(jq -r .headRefName <<<"$info")
            for branch in $targets; do
              if [ "$branch" = "$head" ]; then
                reject "'$branch' matches the head branch of this fork PR; the cherry-pick action would skip it."
              fi
            done
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
    permissions: {}   # writes only through the App token
    runs-on: ubuntu-latest
    timeout-minutes: 10
    steps:
      # A PR opened with GITHUB_TOKEN triggers no workflows, so its CI would
      # never run. contents:write lets the push through; pull-requests:write
      # lets the action create PRs and comment on the source PR.
      - name: Mint SUANOVA_BOT token
        id: app-token
        uses: actions/create-github-app-token@bcd2ba49218906704ab6c1aa796996da409d3eb1 # v3.2.0
        with:
          client-id: ${{ vars.SUANOVA_BOT_CLIENT_ID }}
          private-key: ${{ secrets.SUANOVA_BOT_APP_PRIVATE_KEY }}
          owner: suanova
          repositories: cubepilot
          permission-contents: write
          permission-pull-requests: write

      # git push authenticates through the checkout credentials, not the
      # action's github_token, so the App token has to go here too.
      - uses: actions/checkout@v7
        with:
          token: ${{ steps.app-token.outputs.token }}

      - name: Create cherry-pick PRs
        uses: korthout/backport-action@8560fb503c275d433c56f05a2f64850093baa9f7 # v4
        with:
          github_token: ${{ steps.app-token.outputs.token }}
          source_pr_number: ${{ github.event.issue.number }}
          target_branches: ${{ needs.prepare.outputs.targets }}
          cherry_picking: auto
          branch_name: 'cherrypick-${pull_number}-to-${target_branch}'
          pull_title: '[cherrypick ${target_branch}] ${pull_title}'
          comment_style: summary
          # Only the validated command targets: label-derived targets would
          # bypass the prepare job (e.g. a "backport main" label).
          label_pattern: ''
```

- [ ] **Step 2: Lint the workflow**

Run:

```bash
go run github.com/rhysd/actionlint/cmd/actionlint@latest -ignore 'unexpected key "queue"' .github/workflows/cherrypick.yaml
```

Expected: no output, exit status 0. The only raw diagnostic is the known
`queue` false positive from the older actionlint; this repo's
`vendor-chart.yml` needs the same ignore.

- [ ] **Step 3: Check the comment gating expression by hand**

Run:

```bash
grep -n "issue_comment\|startsWith\|author_association\|collaborators" .github/workflows/cherrypick.yaml
```

Expected: the event is `issue_comment` with `types: [created]`; there is no `author_association` check; the permission API call is present.

- [ ] **Step 4: Commit**

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
(separate several targets with spaces on the same line). The command must be a
conversation comment on the PR; a comment on a review does not trigger the
workflow. A target must already exist and must not be `main`. The generated PR
runs the normal checks and is merged by hand — nothing is merged automatically.
Repeating the command does not open a duplicate PR, and a conflict is reported
on the original PR instead of opening a half-applied PR. A cherry-pick that
adds a new file under `.github/workflows/` works with the bot's current App
permissions; modifying an existing workflow file may still be rejected unless
the App holds the Workflows permission.
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

In the `suanova` organization's GitHub App settings, confirm `SUANOVA_BOT` is installed on `cubepilot` with `contents: write` and `pull-requests: write`. Without these the token mint or PR creation fails. Adding a new workflow file works with these permissions; modifying an existing workflow file may need the App's Workflows permission.

- [ ] **Step 2: Happy path**

Pick a recent PR merged into `main`, comment `/cherrypick release-2.0`, and verify:

- the `cherrypick` workflow run succeeds;
- one summary comment appears on the original PR with a link to the new PR;
- the new PR is `cherrypick-<number>-to-release-2.0` against `release-2.0`, titled `[cherrypick release-2.0] <original title>`;
- the new PR's commits contain the original commit reference (`git log -1` shows a `(cherry picked from commit ...)` line);
- the new PR runs `ci` and `e2e` (not stuck at "Expected");
- repeating the comment recovers the existing branch and skips PR creation rather than opening a second one.

- [ ] **Step 3: Negative paths**

Verify each on the same or another merged PR:

- `/cherrypick main` replies `Cherry-pick skipped: cannot cherry-pick into main`.
- `/cherrypick does-not-exist` replies `Cherry-pick skipped: target branch 'does-not-exist' does not exist.`
- A comment by a user without write access produces no bot reply and no workflow run beyond `prepare`.
- A comment on a closed-but-unmerged PR replies that the PR is not merged.
- A comment on a PR whose base is a release branch replies that only main PRs can be cherry-picked.
- A conflicting backport reports the conflict on the original PR and pushes no branch.
