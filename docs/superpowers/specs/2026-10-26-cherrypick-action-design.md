# Cherry-pick a merged PR to another branch by comment

## Problem

Main is the only branch that receives fixes. Once a release branch (`release-2.0`)
exists, a fix merged to main has to be duplicated by hand: find the squash commit,
create a branch off the release branch, cherry-pick, push, open a PR, and repeat
for every affected release. The step that is easy to get wrong is picking the
right commit after a squash merge, when the PR's original commits are no longer
on main.

## Goal

A maintainer comments on a merged main PR:

```
/cherrypick release-2.0
```

and the workflow opens a PR against `release-2.0` with the same change,
cherry-picked with `-x`, passing the normal CI, ready for human review and merge.

Non-goals: no automatic merge, no cherry-picking between release branches or
back to main, no label/reviewer/milestone copying.

## Trigger and authorization

The workflow listens to `issue_comment: [created]` only. A comment edited later
does not re-trigger, so a command runs at most once per comment.

The run proceeds only when all of the following hold:

1. The comment is on a pull request (not a plain issue).
2. The PR is merged (a closed-but-unmerged PR gets an explanatory reply).
3. The first line of the comment starts with `/cherrypick`. The parser then
   checks that the first token is exactly `/cherrypick` and treats the rest of
   the line as target branches separated by spaces or tabs.
4. The commenter has `admin`, `maintain`, or `write` permission on the
   repository, checked with the REST API
   (`GET /repos/{owner}/{repo}/collaborators/{user}/permission`). The
   `author_association` field is not sufficient: on a public repo `MEMBER`
   means an org member, not someone with write access.
5. The source PR's base branch is `main`.

A commenter without write permission is ignored silently. An authorized
commenter whose command is rejected (PR not merged, wrong base, bad target)
gets one explanatory comment; no cherry-pick job runs.

## Command parsing

- Only the first line is read. `release-2.0` and any text after it on the same
  line are treated as whitespace-separated (spaces or tabs) target branches, so
  `/cherrypick release-2.0 release-2.1` is valid.
- Each target must be a syntactically valid git branch name
  (`git check-ref-format --branch`), must exist on the remote
  (`git ls-remote --exit-code --heads origin refs/heads/<branch>`), and must
  not be `main`.
  There is no `release-*` naming requirement: any existing branch other than
  main is a valid target.
- If one target is invalid the whole command is rejected; no partial run.
- The comment body is passed to the shell through `env`, never interpolated
  into a `run:` block, so its content cannot execute as shell.

## Workflow structure

Two jobs in `.github/workflows/cherrypick.yaml`:

```
prepare (job-scoped GITHUB_TOKEN: contents read, pull-requests write, issues write)
  - verify commenter permission
  - load the PR, require merged and base = main
  - parse and validate target branches
  - output target_branches (empty when anything is rejected)
cherrypick (needs: prepare, skipped when target_branches is empty)
  - actions/create-github-app-token -> SUANOVA_BOT, scoped to cubepilot,
    permissions contents:write, pull-requests:write
  - actions/checkout with the App token (git push authenticates with it)
  - korthout/backport-action@v4 (SHA-pinned)
```

The App token is required rather than the default `GITHUB_TOKEN`: events caused
by `GITHUB_TOKEN` do not start workflows, so a PR created with it would never
run CI. The App token is only minted for the cherry-pick job; the prepare job
never checks out or executes PR code. The `prepare` job gets job-scoped
`contents: read`, `pull-requests: write`, and `issues: write` permissions; a PR
conversation comment accepts either of the last two.

Reuses the existing `SUANOVA_BOT` GitHub App
(`vars.SUANOVA_BOT_CLIENT_ID`, `secrets.SUANOVA_BOT_APP_PRIVATE_KEY`); no new
secret.

## Cherry-pick behavior

`korthout/backport-action` is used with:

| Input | Value |
|---|---|
| `source_pr_number` | `${{ github.event.issue.number }}` |
| `target_branches` | from `prepare` |
| `label_pattern` | `''` (disable label-derived targets; only the validated command targets are used) |
| `cherry_picking` | `auto` (squash commit for squash merges, PR commits for merge/rebase merges) |
| `branch_name` | `cherrypick-${pull_number}-to-${target_branch}` |
| `pull_title` | `[cherrypick ${target_branch}] ${pull_title}` |
| `comment_style` | `summary` |
| `experimental` | `{"conflict_resolution": "fail"}` (default) |

Label-derived targets are disabled. Otherwise the action unions `target_branches`
with branches derived from labels matching its default `label_pattern`, so a
label such as `backport main` could add an unvalidated branch and bypass the
`prepare` job. With `label_pattern: ''` the action matches no labels.

Consequences:

- Cherry-picks carry `-x`, so the new commit references the original.
- The squash commit on main keeps the PR's `Signed-off-by` trailers, which the
  cherry-pick preserves; the DCO check passes on the generated PR.
- On conflict the action stops and pushes nothing; a follow-up step replays the
  cherry-pick and comments the conflicting files on the original PR.
- Re-running the same command recovers the existing
  `cherrypick-<n>-to-<branch>` branch and skips PR creation; it does not update
  the branch, and no duplicate PR is opened.
- The generated PR runs the normal `ci` / `e2e` workflows and is left for human
  review and merge. Mergify's rules match `base = main` only, so it will not
  merge release-branch PRs.
- The PR's base branch (`main`) is not a valid target.
- The action excludes the source PR's head branch by name. A fork PR whose head
  branch shares a target name is rejected in `prepare` instead of letting the
  action skip that target silently.

## Edge cases

| Situation | Behavior |
|---|---|
| PR not merged | Reply: the PR must be merged first |
| PR base is not `main` | Reply: only main PRs can be cherry-picked |
| Target invalid or missing | Reply with the offending target and the accepted form |
| Commenter lacks write access | Silent skip |
| Comment on a non-PR issue | Jobs do not run |
| Cherry-pick conflict | Action comments conflict details and the workflow lists the conflicting files; no branch pushed |
| Repeated command | Existing branch is recovered and PR creation is skipped (no duplicate) |
| Fork head branch matches a target | Reply rejecting the command; the action would silently skip that target |
| Cherry-pick adds a new `.github/workflows/**` file | Works with the current App permissions (verified) |
| Cherry-pick modifies an existing workflow file | May be rejected unless the App holds the Workflows permission |

## Files

- `.github/workflows/cherrypick.yaml` (new).
- `README.md`: a short "Cherry-picking to a release branch" subsection after
  "Releases (CI)" documenting the command, the main-only source, and that the
  generated PR still needs review.

## Testing

- Validate the workflow locally with `actionlint` (not installed in the repo;
  run via `npx actionlint` or a temporary binary). actionlint v1.7.12 predates
  GitHub's `queue` concurrency key and reports `unexpected key "queue"`; pass
  `-ignore 'unexpected key "queue"'` (this repo's `vendor-chart.yml` needs the
  same ignore).
- End-to-end only after the workflow lands on `main` (issue_comment runs the
  workflow from the default branch): comment `/cherrypick release-2.0` on a
  merged main PR and verify the branch, PR, CI, and comment reply.
