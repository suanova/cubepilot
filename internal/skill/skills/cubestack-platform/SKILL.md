---
name: cubestack-platform
description: "Use when operating the CubeStack platform — creating, inspecting or connecting to ai.cubestack.io resources (DevEnvironment / InferenceService / ModelVersion / InferenceRuntimeProfile). Gives the generated CRD map (crd-reference.md) plus a known-good DevEnvironment manifest and usage guidance, so you do not guess ai.cubestack.io schemas via repeated kubectl apply --dry-run=server"
---

# CubeStack Platform Usage

This cluster exposes the CubeStack platform under the `ai.cubestack.io` group:
dev machines (`DevEnvironment`), model serving (`InferenceService` +
`InferenceRuntimeProfile`), and the model catalog (`ModelVersion`). Read this
skill before creating any such resource.

The per-kind schema map — required fields, defaults, enums and types — is
generated and lives in **`crd-reference.md`** in this skill directory. Open it
and trust it over guessing; it is regenerated from the exact CRDs this
environment installs (`make update-cubestack-skill`).

## Creating a DevEnvironment (known-good example)

A `DevEnvironment` is a containerized dev machine ("开发机"). Key semantics:

- `spec.type` picks the container entry — `ssh` (default), `jupyter`, or
  `vscode`.
- `spec.running` (default `false`) is the desired state: `true` = Running,
  `false` = Stopped. Omit it unless you must start the machine now.
- `spec.resources` is **flat** — `cpu`/`memory` are top-level strings here, NOT
  a k8s `requests`/`limits` map. `gpuCount` is an integer (≥ 1, default 1);
  `gpuType` is `nvidia` (default) or `metax`.
- Omit `spec.storage` to skip a managed workspace PVC (default 10Gi mounted at
  `/workspace` when present); omit `spec.volumes` unless you mount an existing
  PVC as the workspace.

A minimal manifest matching "create a dev machine with N CPU / M memory and
image X in namespace Y" (created Stopped, no extra storage):

```yaml
apiVersion: ai.cubestack.io/v1alpha1
kind: DevEnvironment
metadata:
  name: dev-cuda
  namespace: default
spec:
  image: pytorch/pytorch:2.3.1-cuda12.1-cudnn8-runtime
  resources:
    cpu: "4"
    memory: 16Gi
```

Add `running: true` (and, for a persistent workspace,
`storage: {size: 10Gi}`) to actually start it.

## After creation: reading the resource back

A DevEnvironment may exist while still Stopped/Pending. Read `status` to know
what to hand the user:

- `status.phase.name`: `Pending` / `Running` / `Stopped` / `Failed` /
  `Terminating`. `status.conditions` (e.g. `PodScheduled`, `StorageReady`,
  `Ready`) explains why.
- `status.endpoints` lists access addresses once Running — Jupyter as a URL,
  SSH as `host:port`, and extra `ports[].name` entries likewise. Report these
  to the user rather than inventing URLs.
- SSH access only works if the image runs an sshd and the environment exposes
  it (the `ssh` type exposes SSH by default).

## Other kinds

- `InferenceRuntimeProfile` — a serving profile: an `engine`, the `roles`
  workloads, `endpoint` selection, `modelRequirements`, and any
  user-adjustable `overrides`. Usually created by an admin first.
- `ModelVersion` — a model artifact: `model` + `version` identify it;
  `storage` says where it lives; `architecture` / `quantization` describe it.
- `InferenceService` — the running service: reference a `modelRef`
  (ModelVersion) and a `profileRef` (InferenceRuntimeProfile); the controller
  reconciles the workload from the profile.

Required fields and defaults for each kind are in `crd-reference.md`.

## Common mistakes

- Wrapping `spec.resources` in `requests`/`limits` — it is flat; unknown
  fields are rejected by the API server.
- Passing `cpu`/`memory` as numbers — they are strings (`cpu: "4"`,
  `memory: 16Gi`).
- Using the wrong group/version — everything here is `ai.cubestack.io/v1alpha1`.
- Assuming `running` is implicit — a DevEnvironment is created Stopped unless
  you set `running: true`.
- Guessing kinds the map does not cover — fall back to the `kubectl-platform`
  skill's generic schema-discovery recipe instead.
