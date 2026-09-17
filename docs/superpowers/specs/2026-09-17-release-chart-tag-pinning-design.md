# Release chart pins the released images (issue #206)

## Problem

The `release` workflow publishes four images and the Helm chart, but a tag-push release
produces a chart that does not reference the images it was released with.

1. **The released chart pulls rolling images.** All four image references in
   `deploy/charts/cubepilot-chart/values.yaml` are complete, hardcoded refs ending in
   `:latest`, and no template reads `.Chart.AppVersion`. `helm package --app-version X.Y.Z`
   writes only metadata, so `helm install cubepilot-chart --version X.Y.Z` still deploys the
   rolling `:latest` images. Pinning a release today means overriding all four refs by hand
   (`--set operator.image=...,api.image=...,web.image=...,agents.image=...`), which
   `README.md` currently instructs.
2. **A malformed tag fails late.** The tag is resolved but never validated. A non-semver tag
   such as `v1.0` is only rejected by `helm package --version`, which runs *after* the
   `Build images` / `Push images` steps. The job dies with the images already in the registry
   and no matching chart published.
3. **No GitHub Release.** Pushing a tag leaves no release record and no notes anywhere.

## Version semantics (what the three numbers mean)

`Chart.yaml`'s `version` and `appVersion` are independent of each other and of the git tag:

- `version` — the chart's own version. Helm gives it real semantics: it must be valid semver,
  it is the selector for `helm install --version` / `helm pull --version`, and it becomes the
  OCI tag. It versions the templates+values contract.
- `appVersion` — the version of the application the chart deploys, as an informational
  string. Helm requires nothing of it and does nothing with it; it is exposed to templates as
  `.Chart.AppVersion`. Helm's convention is that for a container-image application this is the
  image tag, but Helm does not enforce or check that — it holds only if the chart author makes
  it hold.
- the git tag — an identifier for the source snapshot. Orthogonal to both.

This repo collapses all three onto the git tag for releases: the workflow overrides `version`
and `appVersion` with the tag. That stays. What changes is that `appVersion` stops being
decorative — it becomes the default image tag, so the collapsed version number reaches the
image references.

## Design

### 1. Chart image values: `repository` + `tag`

Each of the four images becomes a pair, with an empty `tag` meaning "use the chart's
appVersion":

```yaml
operator:
  image:
    repository: harbor.isuanova.com/suanova/cubepilot-operator
    tag: ""    # empty -> .Chart.AppVersion
```

A template helper resolves the pair once:

```
{{- define "cubepilot.image" -}}
{{- printf "%s:%s" .image.repository (.image.tag | default .root.Chart.AppVersion) -}}
{{- end -}}
```

called as `{{ include "cubepilot.image" (dict "image" .Values.operator.image "root" .) }}`
from the three workload templates and from the agent-image env in `_helpers.tpl`.

This needs no special-casing at package time, because the two packaging paths already set
`appVersion` to exactly the right value:

| Chart packaged by | `--app-version` | `.Chart.AppVersion` | default image tag |
|---|---|---|---|
| push to `main` | `latest` | `latest` | `latest` (unchanged) |
| tag `vX.Y.Z` | `X.Y.Z` | `X.Y.Z` | `X.Y.Z` |
| `workflow_dispatch` | `<input>` | `<input>` | `<input>` |

So main's rolling chart behaves exactly as before, and a release chart pins the released
images with no `--set` at all.

**Behavior change to note.** Installing from a source checkout
(`helm install ./deploy/charts/cubepilot-chart`) now resolves to the on-disk
`appVersion` (`0.1.0`) instead of `:latest`. That flow is already unsupported — `make images`
tags local builds `:local`, so a bare install never matched the local images either —
`scripts/setup.sh` is the supported path and always passes explicit refs, as does
`scripts/redeploy.sh`. Both move to the new field names:

```bash
--set agents.image.repository="$IMAGE_REPO/cubepilot-openclaw" --set agents.image.tag="$IMAGE_TAG"
```

### 2. Validate the tag before anything is pushed

The first step of the `publish` job resolves the tag; it also validates it there. Only the
tag-push and `workflow_dispatch` paths are validated — `latest` (push to main) bypasses it.

Accepted form: `X.Y.Z` or `X.Y.Z-<prerelease>`. Build metadata (`+`) is rejected even though
it is valid semver, because `+` is not legal in a Docker image tag and would fail later at
`docker push`. A rejected tag exits before `Build images`, so a bad tag publishes nothing.

### 3. GitHub Release on tag pushes

A second job, `github-release`, runs `needs: publish` and only on tag pushes
(`github.event_name == 'push' && github.ref_type == 'tag'`). It checks out (the `gh` CLI
resolves the repository from the working tree) and creates the release with
`gh release create "$GITHUB_REF_NAME" --generate-notes --verify-tag`, using GitHub's
generated notes — **no notes file is committed to the repository**.

Running after `publish` means a release is never created for artifacts that failed to
publish. Gating on tag pushes only means a `workflow_dispatch` verification run leaves no
junk release behind.

The write permission stays on this job alone: `publish` keeps `contents: read` and the Harbor
credentials, while only `github-release` gets `contents: write`, and it never sees the Harbor
secrets.

## Affected files

- `deploy/charts/cubepilot-chart/values.yaml` — four image refs become `repository`/`tag`
- `deploy/charts/cubepilot-chart/templates/_helpers.tpl` — new `cubepilot.image` helper;
  agent-image env uses it
- `deploy/charts/cubepilot-chart/templates/{operator,api,web}.yaml` — use the helper
- `scripts/setup.sh`, `scripts/redeploy.sh` — pass the new field names
- `.github/workflows/release.yaml` — tag validation, `github-release` job, header comment
- `README.md` — values doc, the install example, the release table, GitHub Release

Breaking change to the chart's values interface. The project is pre-release, so no
compatibility shim is kept and no fallback branch reads the old `image` key.

## Verification

- `helm lint deploy/charts/cubepilot-chart`
- Prove the pinning by rendering a packaged chart, which is the only way to control
  `appVersion`:
  - `helm package --version 9.9.9 --app-version 9.9.9 -d /tmp <chart>` then
    `helm template t /tmp/cubepilot-chart-9.9.9.tgz` → all four images end in `:9.9.9`
  - same with `--app-version latest` → all four end in `:latest`
  - same with `--set operator.image.tag=v9` → that image ends in `:v9`, the others `:9.9.9`
- Validate the tag step's regex against: `0.1.0`, `0.1.0-rc1` (accept);
  `1.0`, `v`, `0.1.0+build`, `0.1.0-` (reject)
- `scripts/setup.sh` still deploys a working stack (kind + e2e path)
- End to end: the workflow has never run its release path — no tag exists on upstream. A
  `workflow_dispatch` run with a throwaway tag exercises image+chart publish without creating
  a GitHub Release; the final proof is tagging `v0.1.0`.

## Out of scope

- **Making the on-disk `Chart.yaml` agree with the tag.** The disk values are overridden at
  package time and are only used for main's `<version>-latest` chart tag. Enforcing
  `Chart.yaml version == tag` would force a `Chart.yaml` edit on every release, contradicting
  the one-version-per-release design, and buys only a tidier rolling tag name.
- **Atomic publish.** Images and the chart are pushed in separate steps, so a mid-job failure
  still leaves one without the other. Every tag the job pushes is deterministic, so re-running
  it converges; no rollback machinery.
- Multi-arch builds, image signing, and a committed `CHANGELOG.md`.
