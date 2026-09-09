# CRD Scope Review: namespacing the six ai.cubestack.io CRDs (2026-09-09)

## Decision

All six `ai.cubestack.io` CRDs (AgentTemplate, TaskTemplate, Skill,
AgentInstance, Task, TaskRun) move from `scope: Cluster` to
`scope: Namespaced`, living in the install namespace. Tracked by issue #146.

## Why

The six objects were cluster-scoped only because of the current "one install
per cluster" topology, not because their semantics are cluster-wide. The
control plane's real home is a namespace: every runtime resource it manages
(agent Pods / Services / PVCs, credential Secrets, per-user ServiceAccounts,
the operator's Role) already lives in the install namespace. Only the CRs
were cluster-wide.

Namespacing is the dominant strategy -- compatible with every deployment
model and strictly safer:

- **RBAC returns to the k8s layer.** Component CRD access drops from
  ClusterRole/ClusterRoleBinding to a Role/RoleBinding in the install
  namespace (least privilege). A direct-k8s consumer (e.g. an external
  platform UI operating these CRDs) can be scoped with a namespace Role /
  kubeconfig instead of a cluster-wide grant.
- **Per-user isolation becomes real.** Previously the `owner` field was
  enforced only in the REST handlers; the per-user ServiceAccount could read
  every tenant's CRs. Now CRD access for a per-user identity is a RoleBinding
  in the install namespace (cluster `view` is retained only for discovery --
  the agent is a cluster-ops assistant by design).
- **Coexistence.** Independent installs can share one cluster (helm release
  namespaces) without CR-name collisions. Cluster scope forbade this, and k8s
  forbids changing an installed CRD's scope in place -- the later this change
  happens, the more painful it is. Pre-release, there is no compatibility
  burden.

## What changed

- `internal/api/v1alpha1/*_types.go`: dropped the
  `+kubebuilder:resource:scope=Cluster` marker (6 files).
- CRD manifests regenerated with `controller-gen` (v0.19.0, matching the
  committed version) in `config/crd/bases/` and the identical Helm copy
  `deploy/charts/cubepilot/crds/`. The only schema change is `spec.scope`.
- Every CR Get/List/Create/Update/Patch/Delete in the operator and API now
  scopes to the install namespace (`cfg.Namespace`): controllers, scheduler,
  resolver, instance manager, REST handlers, builtin bootstrap.
- RBAC (`deploy/charts/cubepilot/templates/rbac.yaml`):
  - operator: `ai.cubestack.io` CRUD + status, per-user RoleBinding
    management move into its namespaced Role; the cluster-scope pieces that
    remain (CRD discovery via apiextensions, binding per-user cluster `view`)
    stay in a trimmed ClusterRole.
  - api: platform CRD access moves into a namespaced Role (`<api>-crds`);
    only apiextensions CRD discovery stays cluster-scoped.
  - `cubepilot-user-crds` is now a namespaced Role (was ClusterRole); the
    builtin bootstrap creates a RoleBinding per user instead of a
    ClusterRoleBinding. Per-user `view` ClusterRoleBinding unchanged.
- Tests/e2e fixtures updated to create/read the CRs in a namespace.

## Notes / out of scope

- Object names are unchanged (`<user>-agent-for-cloud`, `<user>-task-<id>`,
  ...): instances still share one namespace today. Name simplification can
  follow if a per-user/tenant namespace model is adopted later.
- Deploying this change requires replacing the installed CRDs (scope cannot be
  mutated in place) -- delete then re-install; pre-release, no migration is
  carried.
- The agent's cluster `view` read is a product-positioning property
  (cluster-ops assistant operating cluster-wide resources), not affected by
  namespacing the platform CRDs.
- Applying validation (admission webhook) for the REST-layer checks is still
  outstanding (see the architecture review) and becomes more relevant once
  clients write the CRDs directly.
