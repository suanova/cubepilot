---
name: kubectl-platform
description: "Run kubectl via exec to query and operate cluster resources and ai.cubestack.io CRDs. kubectl runs as the CURRENT USER by default (least privilege, RBAC is the gate); platform CRD/kind schema discovery uses the platform read-only kubeconfig ($CUBEPILOT_PLATFORM_KUBECONFIG). Read-only runs directly; write operations run cautiously and state the blast radius"
---

# Cluster Resource Operations (kubectl)

Use the `exec` tool to run `kubectl` against the current cluster.

Two identities are available (dual kubeconfig, issue #19):

- **User (default)**: plain `kubectl ...` runs as the **current user** (`~/.kube/config`). Use this for ALL real operations — queries and writes alike. RBAC is the final gate: if the user lacks permission, the API server rejects it — report that honestly.
- **Platform (discovery only)**: schema reads use the platform read-only identity via `kubectl --kubeconfig=$CUBEPILOT_PLATFORM_KUBECONFIG ...`. Do **not** use it for business operations; it exists so the agent can read CRD schemas without the user needing CRD-read rights.

## Common Read-Only Commands (run as the user)

```bash
kubectl get nodes                          # node status
kubectl get pods -n <ns>                   # Pods in a namespace
kubectl get pods -A                        # Pods in all namespaces
kubectl get pods -n <ns> --field-selector=status.phase!=Running   # abnormal Pods
kubectl describe pod <name> -n <ns>        # Pod details and events
kubectl logs <pod> -n <ns> --tail=50       # logs
kubectl get events -n <ns> --sort-by=.lastTimestamp   # events
kubectl get namespaces                     # namespaces
```

## Write Operations (phase-one direct pass-through; state the action and blast radius before running)

```bash
kubectl apply -f <file> -n <ns>
kubectl delete pod <name> -n <ns>
kubectl scale deployment <name> --replicas=N -n <ns>
```

## Schema Discovery (zero-registration CRD support)

Platform CRDs live under the `ai.cubestack.io` group (`DevEnvironment`, `InferenceService`, `ModelVersion`, `InferenceRuntimeProfile`, …). There is no per-CRD skill — discover the kind / group / schema before applying:

1. Find the resource type (platform discovery identity): `kubectl --kubeconfig=$CUBEPILOT_PLATFORM_KUBECONFIG api-resources | grep -i <keyword>` (e.g. `grep -i deve` → `devenvironments.ai.cubestack.io`). If the platform identity cannot discover either, ask the user or fall back to the dry-run in step 3 with the user identity.
2. Read the schema with the platform discovery kubeconfig:

   ```bash
   kubectl --kubeconfig=$CUBEPILOT_PLATFORM_KUBECONFIG explain devenvironment --api-version=ai.cubestack.io/v1alpha1
   # or, when explain is unavailable:
   kubectl --kubeconfig=$CUBEPILOT_PLATFORM_KUBECONFIG get crd devenvironments.ai.cubestack.io -o yaml
   ```

   If the platform identity also lacks CRD-read permission, skip to step 3.
3. Validate against the server before creating (use the user identity): apply with `--dry-run=server` and iterate on the error — the API server lists the required fields (e.g. `spec.image`, `spec.resources`) and rejects unknown ones. Server-side dry-run enforces normal RBAC: creating a new object needs `create`, updating an existing one needs `patch`. If the user identity is read-only you will get Forbidden — probe first with `kubectl auth can-i create <resource>` / `kubectl auth can-i patch <resource>` and report honestly instead of retrying the same rejected apply:

   ```bash
   kubectl apply --dry-run=server -f - -n <ns> <<'EOF'
   apiVersion: ai.cubestack.io/v1alpha1
   kind: DevEnvironment
   metadata:
     name: dev
   spec:
     image: pytorch/pytorch:2.3.1-cuda12.1-cudnn8-runtime
     resources:
       cpu: "4"
       memory: 16Gi
   EOF
   ```

4. Only then apply for real (as the user) and verify: `kubectl get devenvironments -n <ns>`.

## Principles

- Prefer `--field-selector`, `-o wide`, `--sort-by` to get structured results.
- Default kubectl is the **user's** identity — never switch to `--kubeconfig=$CUBEPILOT_PLATFORM_KUBECONFIG` to do a real operation or to bypass the user's RBAC.
- When the user lacks permission (RBAC denied), tell the user honestly and do not retry the same rejected operation.
- When output is long, truncate with `| head` / `--tail`.
