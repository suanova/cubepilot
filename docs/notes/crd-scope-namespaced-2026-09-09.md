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

- **Component RBAC returns to the k8s layer.** The operator's and API's CRD
  access drops from ClusterRole/ClusterRoleBinding to a Role/RoleBinding in
  the install namespace (least privilege). A direct-k8s consumer (e.g. an
  external platform UI operating these CRDs per tenant) can be scoped with a
  namespace Role / kubeconfig instead of a cluster-wide grant.
- **Coexistence.** Independent installs can share one cluster (helm release
  namespaces) without CR-name collisions. Cluster scope forbade this, and k8s
  forbids changing an installed CRD's scope in place -- the later this change
  happens, the more painful it is. Pre-release, there is no compatibility
  burden.
- **The per-user assistant identity is unchanged.** The agent executes kubectl
  with the per-user identity and must operate platform CRs in *any* namespace
  (generic CRD discovery creates e.g. CubeStack DevEnvironments in arbitrary
  namespaces, not only the install namespace). So `cubepilot-user-crds` stays
  a ClusterRole bound via ClusterRoleBinding, alongside cluster `view`. The
  isolation gained by namespacing is at the component/coexistence layer, not
  by narrowing the assistant (an e2e chat spec creates a DevEnvironment in the
  `default` namespace and drove this decision).

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
  - operator: `ai.cubestack.io` CRUD + status move into its namespaced Role;
    the cluster-scope pieces that remain (CRD discovery via apiextensions,
    binding the per-user assistant roles) stay in a trimmed ClusterRole.
  - api: platform CRD access moves into a namespaced Role (`<api>-crds`);
    only apiextensions CRD discovery stays cluster-scoped.
  - per-user assistant identity (`view` + `cubepilot-user-crds` ClusterRole,
    ClusterRoleBindings): unchanged, by design (see Why).
- Tests/e2e fixtures updated to create/read the CRs in a namespace.

## Notes / out of scope

- Object names are unchanged (`<user>-agent-for-cloud`, `<user>-task-<id>`,
  ...): instances still share one namespace today. Name simplification can
  follow if a per-user/tenant namespace model is adopted later.
- Deploying this change requires replacing the installed CRDs (scope cannot be
  mutated in place): delete the six cluster-scoped CRDs and let the chart
  install the namespaced ones. **Deleting a CRD deletes all of its custom
  resources.** The builtin bootstrap / API seed re-creates only the supported
  builtin objects (agent-for-cloud template, per-user instances, preset skills,
  the daily-inspection task template); user-created Tasks, TaskRuns and other
  custom resources are **not** restored and must be backed up if they need to
  survive. Pre-release, no in-place migration is carried.
- Applying validation (admission webhook) for the REST-layer checks is still
  outstanding (see the architecture review) and becomes more relevant once
  clients write the CRDs directly.
