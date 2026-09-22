---
name: inference-validation
description: "Validate a deployed InferenceService end to end: references resolve, replicas ready, endpoint answers, one real request succeeds"
---

# Inference Service Validation

A `Running` Pod is not a serving model. This skill takes an `InferenceService`
from "the CR exists" to "a request came back well formed", and stops at the
first step that fails, so the report names the real cause instead of the
symptom.

`InferenceService.spec` requires `modelRef` (a `ModelVersion`) and `profileRef`
(an `InferenceRuntimeProfile`); the optional `route` carries the `modelName` a
caller must send. The `cubestack-platform` skill has the field reference.

## Steps

```bash
# 1. The service, and the two CRs it points at. A modelRef or profileRef that
#    does not resolve stops the validation here.
kubectl get inferenceservice -n <namespace> <name> -o yaml
kubectl get modelversion <modelRef>
kubectl get inferenceruntimeprofile <profileRef> -o yaml

# 2. Workload: every role the profile declares, and whether each has its ready
#    replicas. A role with zero ready replicas fails the validation.
kubectl get pods -n <namespace> -o wide
kubectl describe pod -n <namespace> <pod> | grep -A5 -E 'State|Last State|Events'

# 3. Endpoint: the Service, and the portName the profile's endpoint declares.
kubectl get svc,endpoints -n <namespace>
```

## The functional check

Everything above can pass while the service answers nothing. Send one real
request, using `route.modelName` as the model:

```bash
curl -sS -m 30 -w '\nHTTP %{http_code} in %{time_total}s\n' \
  -H 'Content-Type: application/json' \
  -d '{"model":"<modelName>","messages":[{"role":"user","content":"ping"}]}' \
  http://<service>.<namespace>:<port>/v1/chat/completions
```

Where you run it from decides what you have to ask for:

- `route.publish: true` means the service has an address reachable from
  outside -- use it directly.
- Running inside the cluster, the Service DNS name works as written above.
- Otherwise, reaching the endpoint means running a Pod in the cluster, which is
  a **write**. State the command and its blast radius and wait for approval. If
  it is not approved, report the functional check as **not performed** -- never
  as a pass.

Read the response, not only the status code: a 200 carrying an error body, an
empty `choices`, or a stream that never ends is a failure.

## What each failure means

- `modelRef` or `profileRef` does not resolve -- the CR is stale; the service
  cannot serve whatever the workload says.
- A role short of ready replicas -- read the Pod's `State` / `Last State` before
  anything else; `OOMKilled` and `CrashLoopBackOff` point at the model or the
  engine, a Pending Pod points at capacity.
- Readiness that reports up while a role is down -- check the profile's
  `readinessPolicy.requireAllRoles`.
- Port answers nothing -- compare the profile's `endpoint.portName` with the
  Service's port names; a mismatch looks exactly like a dead service.
- A well-formed answer that takes far longer than the caller's expectation --
  report the latency with the request, and leave the judgement to the caller.
- `route.publish: false` -- the service works but is not reachable from outside;
  say so rather than reporting it as broken.

## Report Format (Simplified Chinese)

- Per service: the verdict (serving / not serving / not validated), then the
  first failing step with the command and its key output.
- For a service that passes, give the evidence anyway: ready replicas per role,
  the endpoint, and the request with its response time.
- End with what has to change for a failing service to serve.
