---
name: create-pr
description: Create a pull request from the current branch. Use when the user asks to "create a PR", "open a pull request", "make a PR", "raise a PR", or types "/create-pr [branch-name]". Branches off main/master if needed, commits changes, analyzes the full commit history, drafts a PR from the template, determines the correct remote and base branch, and opens it with gh.
---

# /create-pr

Create a pull request for the current branch. Usage: `/create-pr [branch-name]`.

If a branch name is provided as a parameter, use it when creating a new branch. Otherwise, generate a descriptive branch name based on the changes.

## Workflow

### 1. Branch setup

Check the current branch name (`git branch --show-current`).

- If it's `main` or `master`, checkout a new branch:
  - If a branch name was provided to this command, use that name.
  - Otherwise, generate a descriptive name based on the changes (e.g. `feat/...`, `fix/...`, `docs/...`).
- If there are uncommitted changes, commit them to the branch first (see Commit rules).

### 2. Understand the changes

Run `git status`, `git diff`, and `git log` to determine:

- Current branch status and untracked files
- All staged and unstaged changes
- Commit history for the current branch from when it diverged from the base branch

Analyze ALL commits that will be included in the PR — not just the latest one.

### 3. Draft the PR

Use this template, with an emoji/icon prefix based on the change type:

| Change type | Prefix |
|---|---|
| Feature | ✨ |
| Bug fix | 🐛 |
| Docs | 📖 |
| Proposal | 📝 |
| Breaking change | ⚠️ |
| Release | 🚀 |
| Other / misc | 🌱 |
| Requires manual review/categorization | ❓ |

**Title:** `<prefix> <short imperative summary>`

**Body:**

```markdown
## Summary

- bullet 1
- bullet 2
- bullet 3

## Related Issues

- #<issue>  (only if applicable)
```

### 4. Determine the target remote and base branch

- If the current branch already tracks a remote (`git rev-parse --abbrev-ref --symbolic-full-name @{u}`), use it.
- Otherwise, analyze the commit history to find the remote and branch with the most common ancestry:
  - Inspect `git log --all --decorate --oneline --graph` and `git branch -r` to identify remote branches.
  - Compare histories with `git merge-base` to find which remote branch shares the most commits with the current changes.
- Target that remote and base branch. Push with `-u` if needed.

### 5. Create the PR

- Use `gh pr create` with the formatted title and body, targeting the base branch identified above.
- Use a HEREDOC for the body to ensure correct formatting.
- Print the PR URL when done.

## Commit rules

- Add each changed file explicitly (e.g. `git add file1.ts file2.ts`). NEVER use `git add .` or `git add *`.
- Always commit with `--signoff`.
- Include a note in the commit message, e.g. `🤖 Generated with Claude Code`, a `Co-Authored-By: Claude` trailer, or `Assisted by Claude`.

## Don'ts

- Do NOT use TodoWrite or the Task tool for this workflow.
