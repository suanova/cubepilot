# Integration contract: an external UI operating the platform CRDs directly (2026-09-09)

## Goal

CubePilot ships its own Portal on top of the `cubepilot-api` REST service. A
consumer platform (cubestack) embeds CubePilot and wants its own unified UI to
operate the six `ai.cubestack.io` platform CRDs **directly against the
kube-apiserver** (CRD-first), falling back to CubePilot REST only where a CRD
cannot carry the operation. This note is the agreed contract.

> Prerequisite: the six platform CRDs are Namespaced (issue #146 / PR #147,
> pending merge) and live in the install namespace.

## Deployment model (assumed)

- CubePilot is installed in the same namespace as the consuming UI (e.g.
  `cubestack-system`): control-plane pods, the six CRs, and the UI
  ServiceAccount are all co-located. RBAC is therefore a plain RoleBinding in
  that namespace.
- Today there is a single namespace and no tenant concept. The future model is
  tenant = namespace: each tenant namespace runs its own CubePilot install (or
  the control plane reconciles per-tenant namespaces) and holds its own CRs.
  Definitions (AgentTemplate / TaskTemplate / Skill) are per-tenant by default;
  no platform-shared catalog tier is designed yet.
- The consuming UI runs as its own ServiceAccount. Today there is a single
  `admin` account; owner values are written by the UI itself (the REST
  `X-CubePilot-User` header does not exist on the direct path).

## RBAC

One ClusterRole holds the rules (reusable across namespaces); one RoleBinding
per namespace grants them there. A RoleBinding referencing a ClusterRole is
still scoped to the RoleBinding's namespace only.

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: cubepilot-crd-operator
rules:
  - apiGroups: ["ai.cubestack.io"]
    resources:
      - agenttemplates
      - tasktemplates
      - skills
      - agentinstances
      - tasks
      - taskruns
    verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
  # status is read-only for consumers (controllers own status writes)
  - apiGroups: ["ai.cubestack.io"]
    resources: ["agentinstances/status", "tasks/status", "taskruns/status", "skills/status"]
    verbs: ["get", "list", "watch"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: cubepilot-crd-operator
  namespace: cubestack-system   # the install namespace
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: cubepilot-crd-operator
subjects:
  - kind: ServiceAccount
    name: cubestack-ui          # the consuming UI's own SA
    namespace: cubestack-system
```

RBAC rule to remember: a RoleBinding's ServiceAccount subject must be in the
same namespace as the RoleBinding. Co-locating the UI SA with the install keeps
the grant namespace-scoped; a foreign-namespace SA would force a
ClusterRoleBinding (cluster-wide), which we deliberately avoid.

Optional: only if the UI renders a "kinds / field descriptions" page, add
`apiextensions.k8s.io` `customresourcedefinitions` get/list/watch.

## Feature -> data-plane mapping

| UI feature | Data plane | Notes |
|---|---|---|
| AgentTemplate / TaskTemplate / Skill list + detail | k8s `list`/`get`/`watch` | catalogs; watch is free |
| AgentInstance list + state | k8s `list`/`watch` (+ `/status`) | phase / podName live |
| Self-service provision an instance | k8s `create agentinstances` | name `sanitize(owner)-agent-for-cloud`; `spec.owner` = the UI account |
| Model / userInstructions / enabledSkills / confirm override | k8s `patch agentinstances.spec` | direct spec writes |
| Task CRUD + enable/pause | k8s | `spec.state` |
| Manually trigger a task | k8s | set annotation `cubepilot/manual-run=<RFC3339>`; the scheduler fires and clears it |
| TaskRun reports | k8s | `list` with labelSelector `cubepilot/task=<task CR name>` |
| Live chat / session history / approvals | REST | conversation lives in the live per-instance runtime/gateway, not in any CR |
| Audit log | REST | per-user JSON ledger on the API PVC |
| Skill publish (upload tar) | REST | content (tar) lives on the API PVC; install/uninstall of an existing skill is a CRD `spec.enabledSkills` patch |
| One-shot inspection | REST (optional) | live agent turn |

Minimal REST keep-list and why: live chat/approvals (no CRD representation),
`GET /api/audit` (API-owned PVC state), skill publish (file content),
`POST /api/inspect` (optional). `/internal/*` endpoints are cluster-internal
(agent supervisor pulling config) and are not for any UI.

## Conventions a direct writer must replicate

- Instance CR name: `sanitize(owner)-agent-for-cloud`.
- Task CR name: `sanitize(owner)-task-<8 hex>`; the human name is the
  `cubepilot/display-name` annotation (CR name is DNS-1123).
- Manual run: patch annotation `cubepilot/manual-run` with an RFC3339
  timestamp (idempotency key); the scheduler owns execution.
- TaskRuns carry label `cubepilot/task=<task CR name>`; reports are that label
  selector, newest first.
- Confirmation posture is displayed **without merging**: show the template
  default (`AgentTemplate.spec.confirmPolicy`) and the instance override
  (`AgentInstance.spec.confirmPolicy`, empty = "inherits template") as two
  lines. No effective-value merge is written to status by design.
- Skills: `spec.enabledSkills` empty = all visible platform skills; once the UI
  customizes, write an explicit list. Uninstalling from the inherited (empty)
  baseline therefore means materializing the visible list minus the removed
  skill in one patch.
- Consumers write spec only; status subresources are read-only to them
  (controllers own status writes).

## Behavioral gaps / prerequisites

1. **Per-user assistant identity minting is static today.** The operator
   mints each user's ServiceAccount + kubeconfig Secret from the configured
   `CUBEPILOT_USERS` list, and the AgentInstance controller waits for that
   kubeconfig Secret before creating the agent Pod (issue #100). A single
   configured `admin` works today. Self-service provisioning of an owner
   outside the list needs **on-demand identity minting** (controller mints the
   identity when it sees an unknown owner) -- a future change, out of scope
   here, tracked separately.
2. **Validation posture (no admission webhook).** By decision, no webhook and
   no ValidatingAdmissionPolicy are added. Validation stays as:
   - CRD schema `x-kubernetes-validations` (CEL, evaluated in the API server,
     already used for e.g. Skill source and inline model invariants);
   - REST-server checks (kept for the Portal's write path);
   - controller fail-closed: semantically invalid input (bad cron, unknown
     model/ref) is not executed and is surfaced as a status condition/phase on
     the consuming controller, which the UI reads back.
   Direct k8s writers accept that create-time validation is shallow and rely on
   controller-surfaced status for semantic errors.
3. The live-chat REST fallback still uses the phase-one identity header model;
   authentication on the REST surface is out of scope for this note.

## Deliberately not done (keep it simple)

- No effective/merged config written into AgentInstance.status.
- No admission webhook / VAP.
- No platform-shared catalog namespace / cross-namespace definition references
  (per-tenant catalogs only, same-namespace resolution).
- No on-demand identity minting yet (see gaps).
