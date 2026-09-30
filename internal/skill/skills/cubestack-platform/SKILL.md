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
  a k8s `requests`/`limits` map. A GPU is a **block**: `resources.gpu.vendor`
  (`nvidia` default / `metax`) and `resources.gpu.count` (default 1). Ask for no
  accelerator by omitting the whole `gpu` block — the pod then carries no vendor
  GPU resource and the image brand is not checked. There is no `count: 0`.
- `spec.runtime.user` is the container account the environment runs as (default
  `user`); set it for a bring-your-own image that runs as something else (e.g.
  `jovyan`), and keep `spec.runtime.securityContext.runAsUser` on that account.
  An environment running as root is advertised as `root` whatever this says.
- Omit `spec.storage` to skip a managed workspace PVC (10Gi when present). Its
  mount path *is* the container's HOME — the controller states it as HOME on the
  main container — so where the workspace is and where the container thinks home
  is are one decision. It is derived from `spec.runtime` (HOME from
  `spec.runtime.env` when that names an absolute path outright, else `/root` for
  a root container, else `/home/<user>` when `spec.runtime.user` names an
  account, else `/workspace`), or pinned with `spec.storage.mountPath`. The
  claim is deleted together with the environment unless you set
  `spec.storage.pvcRetention: retain`. Omit `spec.volumes` unless you mount an
  existing PVC as the workspace; a referenced PVC is mounted as-is, so it must
  already grant the environment's account access.
- `spec.ssh.enabled` exposes the SSH endpoint (the `ssh` type does that for you);
  it does not start an sshd — the image has to run one. The controller generates
  the keypair by default and records the private half in
  `status.sshClientKeySecret`; to use your own public keys, point
  `spec.ssh.authorizedKeysSecret` at a Secret labelled
  `ai.cubestack.io/ssh-keys-delegated: "true"` — only a Secret that names itself
  for this use is mounted into a workload.
- `spec.lifecycle.idleTimeout` (seconds; 0 disables) stops an idle environment
  without touching `spec.running`: it marks the environment with the annotation
  `ai.cubestack.io/auto-stopped`, and that mark — not `spec.running` — is what
  says the environment is stopped. Start one by clearing the annotation; with
  `running` already true, a start changes nothing the platform can observe.
- `spec.ports[].type` picks the exposure form: `http` publishes the port as a
  cleartext sub path of the Gateway (`/dev/<ns>/<env>/port/<name>/`), `tcp`/`udp`
  give it a listener of its own over L4. A port serving TLS is exposed as `tcp`
  and only as `tcp`: the platform neither terminates nor re-originates TLS, so
  the client validates the container's own certificate against the address in
  `status.endpoints`.

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
  `Terminating`. `status.conditions` (`Accepted`, `PodScheduled`, `RouteReady`,
  `Ready`) explains why; `Accepted` is where the controller reports what it
  substituted or ignored — an overridden `HOME`, a dropped env entry, a
  `runtime.user` it did not use.
- `status.endpoints` lists access addresses once Running — Jupyter as a URL,
  SSH as `host:port`, and extra `ports[].name` entries likewise — each with the
  `listenerPort` it was published on. Report these to the user rather than
  inventing URLs; `listenerPort` is the stable allocation, while the port inside
  `Address` is only where the endpoint is reachable now (a NodePort dataplane
  renumbers it).
- A jupyter environment's token lives in the Secret named by
  `status.jupyterTokenSecret` (data key `token`), not in the spec.
- SSH access only works if the image runs an sshd and the environment exposes
  it (the `ssh` type exposes SSH by default).

## Other kinds

- `InferenceRuntimeProfile` — a serving profile: an `engine`, the `roles`
  workloads, `endpoint` selection, `modelRequirements`, and any
  user-adjustable `overrides`. Usually created by an admin first.
- `ModelVersion` — a model artifact: `model` + `version` identify it;
  `storage` says where it lives (`strategy` picks one of `HostPath` / `Dynamic`
  / `Static` / `S3`, and exactly one matching sub-object must accompany it);
  `architecture` / `quantization` describe it.
- `InferenceService` — the running service: reference a `modelRef`
  (ModelVersion) and a `profileRef` (InferenceRuntimeProfile); the controller
  reconciles the workload from the profile. `spec.route.publish` puts the
  service into the platform model catalog under `spec.route.modelName` — the
  name clients send in the request body's `model` field, unique among published
  services. `route.timeoutSeconds` defaults to `0`, which caps nothing (every
  streamed chunk counts toward it when set), and `route.idleTimeoutSeconds`
  (default 300) cuts a request whose upstream sent no bytes for that long —
  which bounds a non-streaming generation too, since it sends nothing until it
  completes.

Required fields and defaults for each kind are in `crd-reference.md`.

## Common mistakes

- Wrapping `spec.resources` in `requests`/`limits` — it is flat; unknown
  fields are rejected by the API server.
- Passing `cpu`/`memory` as numbers — they are strings (`cpu: "4"`,
  `memory: 16Gi`).
- Writing `resources.gpuType`/`resources.gpuCount` — the GPU request is a `gpu`
  block (`gpu.vendor`, `gpu.count`), and `gpu.count` has no `0`.
- Using the wrong group/version — everything here is `ai.cubestack.io/v1alpha1`.
- Assuming `running` is implicit — a DevEnvironment is created Stopped unless
  you set `running: true`.
- Expecting `running: true` alone to start an environment the idle timeout
  stopped — that stop is marked with the annotation `ai.cubestack.io/auto-stopped`,
  which says the environment is stopped while `running` still reads `true`;
  clear the annotation to start it.
- Pointing `spec.ssh.authorizedKeysSecret` at a Secret that lacks the label
  `ai.cubestack.io/ssh-keys-delegated: "true"` — it is not mounted.
- Expecting `spec.storage` to work in a namespace pinned to the Restricted Pod
  Security Standard — the workspace-claim init container runs as root with
  `CAP_CHOWN`/`CAP_FOWNER`/`CAP_FSETID`, so the namespace has to be at Baseline.
- Assuming `spec.storage.pvcRetention` protects the workspace on deletion — the
  default is `delete`; set it to `retain` before deleting an environment whose
  data must outlive it.
- Guessing kinds the map does not cover — fall back to the `kubectl-platform`
  skill's generic schema-discovery recipe instead.
