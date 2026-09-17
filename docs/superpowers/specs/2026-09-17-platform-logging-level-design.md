# A platform-wide log level, and the operator log's 82% body dump (issue #209) -- design

Date: 2026-09-17 · Status: approved for implementation · Scope: issue #209

## Context

The platform has no notion of a log level. Nothing in the operator registers one:
`cmd/cubepilot-operator/main.go` contains no `flag.` call at all -- no
`flag.Parse`, no klog `InitFlags` -- so `-v` does not exist as a flag, and the
chart (`deploy/charts/cubepilot-chart/values.yaml`) exposes no level. The sink
that controller-runtime logs flow through discards the level it is handed:

```go
func (l *stdLogr) Enabled(level int) bool { return true }   // logrlog.go:20
```

`Info` takes a `level` parameter too and ignores it
(`internal/logrlog/logrlog.go:21-31`; that file becomes
`internal/logging/logging.go` under this change, so it is cited as a path rather
than linked). The result is that
every `V(1)`-`V(10)` call site in client-go and controller-runtime is emitted:
the effective verbosity is `V(10)`, the highest level anything in the dependency
tree asks for.

The three components each ended up on a different logging stack, and each fails
in a different direction. None of them was chosen; they are what falls out of
which constructor each binary happens to call.

| Component | Own logs | Dependency logs | Failure |
|---|---|---|---|
| operator | stdlib `log` | controller-runtime -> `logrlog` | everything on: V(1)-V(10) |
| api | stdlib `log` (`s.logf`) | controller-runtime -> `NullLogSink` | everything dropped, plus a one-time stack-trace warning |
| supervisor | stdlib `log` | client-go -> klog global (default 0) | correct by accident; different semantics from the other two |

### Why each one behaves the way it does

The routing hinge is `klog.FromContext`, which returns `logr.FromContext(ctx)`
when the context carries a logr logger and otherwise falls back to klog's global
logger (`k8s.io/klog/v2@v2.140.0/contextual.go:156-164`).
client-go's `rest` package asks for its logger this way (`rest/request.go`
passes `klog.FromContext(ctx)` into `logBody`).

- **operator** calls `ctrllog.SetLogger(logrlog.New())`
  ([main.go:39](../../../cmd/cubepilot-operator/main.go)), so the sink reaches
  the context and `Enabled(8)` answers true. The body dump is printed.
- **api** never calls `SetLogger`. controller-runtime v0.25.0's `SetLogger`
  no longer bridges klog -- it fulfils a deferred root logger
  (`sigs.k8s.io/controller-runtime@v0.25.0/pkg/log/log.go:49-52`)
  -- and a timer fulfils it with `NullLogSink` after 30s, printing a stack trace
  (`sigs.k8s.io/controller-runtime@v0.25.0/pkg/log/log.go:60-76`).
  So `logr.FromContext` *succeeds* and returns the null logger: the API's
  controller-runtime output is discarded, not routed to klog.
- **supervisor** builds a client-go clientset
  ([supervisor.go:38-39](../../../internal/supervisor/supervisor.go)) but has no
  controller-runtime at all, so `logr.FromContext` fails and it lands on klog's
  global logger at klog's own default of 0. It is quiet for the right reason and
  for the wrong one.

### The credential surface

`V(8)` is `rest/request.go`'s `logBody`
(`k8s.io/client-go@v0.37.0/rest/request.go:1294-1304`):

```go
func logBody(logger klog.Logger, callDepth int, prefix string, body []byte) {
	if loggerV := logger.V(8); loggerV.Enabled() {
		...
		loggerV.Info(prefix, "body", truncateBody(logger, hex.Dump(body)))
```

The guard is checked first, so a sink that answers false never builds the dump.
A sink that answers true builds and prints it for every request and response --
including the ones whose body is a Secret. `hex.Dump` is chosen over the JSON
path whenever the body contains a byte below `0x0a`, which is always true for a
protobuf body.

Nothing in the captured logs shows a credential: the only body dumps in that
window are `Lease` objects, and grepping the hexdump ASCII gutters for
`kubeconfig`, `client-key`, `apiVersion` and `BEGIN` returns zero hits. The
point is that the switch is wired to on-by-default with no way to turn it off,
not that it has already fired.

`V(9)` is a more aggressive dump -- the curl command, including request headers
(`k8s.io/client-go@v0.37.0/transport/round_trippers.go:495`)
-- but it is not reachable here: `DebugWrappers` installs that round tripper only
when the *global* `klog.V(6)` is enabled
(`k8s.io/client-go@v0.37.0/transport/round_trippers.go:79-83`),
and the global logger is still at 0. Its `toCurl` masks `Authorization` through
`maskValue` (`k8s.io/client-go@v0.37.0/transport/round_trippers.go:463-483`),
so it would not leak the bearer token either. Two verbosity paths exist and only
the per-context one is uncontrolled.

## Measured impact

A kind cluster on the chart defaults, ~72 minutes of operator logs across both
replicas:

- 35,823 physical lines; **29,290 (81.8%)** are continuation lines of hex-dumped
  request/response bodies.
- 1,943 body dumps: 949 `Response Body` and 994 `Request Body`. 945 of the
  response bodies are protobuf and hex-dumped; 4 are JSON, of 944/1371/1573/1726
  characters.
- One `Response Body` record spans ~30 physical lines, so counting records
  understates the volume by roughly 15x.
- 880 of the 949 `Response Body` records are the standby replica's
  `Failed to acquire lease` retries; the leader contributes 69.
- Volume reached kubelet's 10 MiB `containerLogMaxSize` and rotated. The rotated
  file holds the only record of the startup banner and of
  `Successfully acquired lease` -- records `kubectl logs` never shows, because it
  reads `0.log` only. **Logs are being lost today.**

Two more defects in the same sink. Unlike the volume problem, these are
source-verified rather than measured in the captured window, and the two differ
in whether the level fix happens to cover them:

- `WithValues` returns the receiver, discarding the per-reconcile logger that
  controller-runtime builds from its `LogConstructor`
  (`sigs.k8s.io/controller-runtime@v0.25.0/pkg/builder/controller.go:434-451`),
  which is what attaches `controller`, the object, and `reconcileID`. **The level
  fix does not cover this**, because it lands on `Error`, which prints at every
  level. The captured window happens to contain no `Reconciler error` line at all
  (0 occurrences in 35,823 lines -- the cluster was healthy), so the symptom was
  reproduced in a harness driving the sink with controller-runtime's own call
  sequence rather than observed in this cluster:
  `ERROR Reconciler error: secrets "user-admin-kubeconfig" not found []` with no
  object identity, so the one line that matters cannot be traced to a CR.
- `WithName` replaces rather than composes the name, and an empty `kv` renders as
  a literal `[]`. This one is **mostly covered by the level fix**: 29 of 3,920
  `ctrl`-prefixed lines end in `[]` (0.7%), and all but two of those are above
  V(0) and disappear at the default level. The two that survive are
  `Starting metrics server` and `starting server`; the rest are
  `Checking CA file content` (9), `CA file unchanged, skipping transport
  rotation` (9), `Reconciling` (5) and `Reconcile done, requeueing after ...` (4).
  The sink is still fixed, because the same formatting is wrong wherever a
  sub-default level is configured.

## Design

### 1. `internal/logging` (replacing `internal/logrlog`)

The package stops being "the standard log package, adapted to logr" -- its
current doc comment -- and becomes a real leveled sink, so the name follows.

```go
// New returns a logr.Logger writing to stderr at the given verbosity.
// Level 0 emits V(0) and every Error; each increment admits one more V level.
func New(level int) logr.Logger
```

The sink fixes four things:

| Current | Change | Symptom fixed |
|---|---|---|
| `Enabled(level) { return true }` | `return level <= l.max` | 82% of operator lines |
| `WithValues(kv...) { return l }` | accumulate into the sink | `Reconciling []`, subject-less `Reconciler error` |
| `WithName(n) { return &stdLogr{n} }` | compose as `parent/child` | nested loggers lose their prefix |
| `Info` ignores its `level` | honor it | levels below and above the cap |

`Enabled` is what does the work: logr calls it from `Logger.V`, and a false
returns a null logger, so the call site -- including `hex.Dump` -- is skipped
rather than filtered after the fact. The cost of a suppressed V(8) is one
comparison.

Rendering: `msg key=value`, with an empty `kv` printing nothing (no `[]`),
values containing whitespace quoted (`err="secrets \"user-admin-kubeconfig\" not
found"`), and `fmt.Stringer` preferred where a value implements it. A value that
is a raw pointer is what produces `items 0x3487d46d5ac0` today -- that comes
from the workqueue dump at `priorityqueue.go:552`, above the default cap, and is
not worth special-casing.

The line format is:

```
2026-09-17T01:51:11.123Z INFO  leaderelection: Failed to acquire lease lock=cubestack-system/cubepilot-operator.suanova.io
2026-09-17T01:51:11.456Z ERROR controller/agentinstance: Reconciler error controller=agentinstance agentinstance=cubestack-system/user-admin reconcileID=1d0f7c3e-... err="secrets \"user-admin-kubeconfig\" not found"
```

`<RFC3339 millis> <LEVEL> <logger>: <msg> <k=v ...>`, level padded to five
characters, the `<logger>:` part omitted when the logger has no name. This is a
wire-format change and is called out under Risks.

The timestamp comes from the sink's own `log.New(os.Stderr, "", 0)` instance
rather than the global stdlib logger, so the sink controls the timestamp format
without touching the flags that the components' own `log.Printf` calls depend on.

### 2. Wire each component on the path it actually uses

No two of these are the same call, because no two components sit in the same
place:

```go
// operator (cmd/cubepilot-operator/main.go:39) -- replaces logrlog
ctrllog.SetLogger(logging.New(cfg.LogLevel))

// api (cmd/cubepilot-api/main.go) -- currently absent; this also fixes the
// discarded controller-runtime output, since it fulfils the deferred root
// logger before the 30s NullLogSink timer fires
ctrllog.SetLogger(logging.New(cfg.LogLevel))

// supervisor (cmd/cubepilot-supervisor/main.go) -- no controller-runtime, so
// there is no context to carry a logger: FromContext falls back to
// klog.Background(), which returns the logger only when ContextualLogger is set
klog.SetLoggerWithOptions(logging.New(cfg.LogLevel), klog.ContextualLogger(true))
```

`ContextualLogger(true)` is load-bearing, not decoration: without it
`Background()` ignores the logger set here and returns the klog-backed one
(`k8s.io/klog/v2@v2.140.0/contextual.go:177-183`).
Its doc also warns that such a logger "cannot rely on verbosity checking in klog"
-- which is what we want, since the sink's own `Enabled` is then authoritative.

### 3. Level semantics: the level controls dependency verbosity only

cubepilot's own `log.Printf` call sites stay un-leveled and always visible. They
are deliberate messages written by hand, and there are very few of them: across
35,823 lines and ~72 minutes on both replicas, the operator emitted exactly one
of its own bootstrapping lines (`bootstrap: created /admin-cubepilot`). The
overwhelming majority of what a reader sees is somebody else's library talking.
Re-leveling ~100 sites is a judgement call per site for no measurable gain, and
most of them are error paths rather than debug chatter, which is the shape that
least rewards a level.

If a specific call site later needs suppressing, it gets a level then, where the
argument for it is concrete.

### 4. Configuration

`config.Config.LogLevel`, read as `CUBEPILOT_LOG_LEVEL` through the existing
`getInt` helper ([config.go](../../../internal/config/config.go)), default `0`.
The supervisor has its own `LoadFromEnv` and needs the same field.

Chart (`deploy/charts/cubepilot-chart/values.yaml`):

```yaml
operator:
  logLevel: 0   # 0 = V(0) + errors. Each +1 admits one more V level from
                # client-go / controller-runtime. Above 7 this prints full
                # request and response bodies, Secret bodies included.
api:
  logLevel: 0
agents:
  logLevel: 0
```

**No hard ceiling.** Upstream Kubernetes does not cap `-v`, and it is a
supported way to diagnose API traffic. The exposure comes from the level being
on by default with no way to turn it off, not from `V(8)` existing; a ceiling
would remove the one tool available when someone needs to watch actual requests,
and would be a departure from the convention every operator already knows. The
defence is the default, and the values.yaml comment states the consequence.

### 5. Readability fixes outside the sink

Two lines are unreadable for reasons the sink cannot fix:

- `internal/controller/builtin.go:346` logs
  `bootstrap: created %s/%s` with `obj.GetObjectKind().GroupVersionKind().Kind`.
  Bootstrapped objects are built as Go struct literals with no `TypeMeta`, so the
  Kind is empty and the line reads `bootstrap: created /admin-cubepilot`. Resolve
  it through `r.Scheme.ObjectKinds(obj)`; `Scheme` is already a field on
  `BuiltinBootstrapReconciler` ([builtin.go:157](../../../internal/controller/builtin.go)),
  so no new import is needed and neither is `apiutil`.
- `internal/supervisor/supervisor.go:452` logs
  `config revision %s -> %s`, which on the first sync has no previous revision:
  `config revision  -> 42c94f84db82`. Print `(initial)` for the empty case.

The sink's own fixes (no `[]`, no bare pointers) cover the rest of what the
audit found.

### 6. The API access log

`internal/server/server.go:262` defines `logRequests` as a pass-through:

```go
func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r)
	})
}
```

It is installed as the outermost middleware
([server.go:232](../../../internal/server/server.go)), so the name claims a
capability the API does not have. Implement it: method, path, status, response
bytes and duration, through `s.logf`. `/healthz`, `/readyz` and `/metrics` are
skipped by default -- otherwise the probes dominate the log, which is exactly
what already happens one tier down, where 578 of 812 nginx lines are
`kube-probe/1.32`.

Skipping rather than leveling them is deliberate: an access log is at the level
of the component's own logs, which section 3 says are always visible.

## Risks and deliberately unhandled

- **The log line format changes, which is a wire format.** Anything collecting or
  parsing the operator's output -- a log pipeline, a dashboard, an alert rule --
  is affected by `2026/09/17 01:51:11 ctrl[leaderelection]: Failed to acquire
  lease [lock ...]` becoming `2026-09-17T01:51:11.123Z INFO  leaderelection:
  Failed to acquire lease lock=...`. The `ctrl[...]` marker is dropped because a
  platform-wide sink cannot claim to be controller-runtime; the logger name takes
  its place. This is unavoidable if the supervisor and API are to share the sink,
  which is the point of the change.
- **Two writers to stderr per container.** The sink uses its own `*log.Logger`
  while the components' own `log.Printf` calls use the stdlib global one. Both
  have separate mutexes, so a line from each can interleave at a pipe. Each
  write is a single `Write` to a `*os.File`, which is atomic for pipes under
  `PIPE_BUF`, so in practice lines stay intact. Worth knowing rather than fixing.
- **`V(8)` remains reachable** by setting `logLevel: 8`. Accepted, per section 4.
- **The six silent provisioning-failure paths** in
  `internal/controller/agentinstance_controller.go` (lines 101, 106, 188, 192,
  195, 199) -- each returns `ctrl.Result{}` with a nil error after writing a
  reason into `.status`, so a provisioning failure logs nothing, requeues
  nothing and increments no metric. Of the audit's findings this is the one with
  real consequences, and it is out of scope here: it is a control-flow change
  (the missing requeue is the more fundamental bug), not a logging change.
  Tracked separately.
- **`internal/server/server.go:151`** logs the HITL master Secret's *name*. That
  is a name, not a value, and the name is already fixed in the chart and Go
  source, so it is not a leak. Noted so it is not re-raised.

## Verification

- `go build ./... && go vet ./... && go test ./...`, plus `golangci-lint run`
  (not part of `make test`).
- Unit tests for the sink, asserting on rendered output: `Enabled` for levels
  around the cap; `WithValues` accumulation across two calls; `WithName`
  composition; empty `kv` producing no `[]`; a value containing a space quoted;
  `Error` carrying `err=`.
- A test that `V(8)` does not reach the sink at the default level -- the guard
  that must not regress.
- Deploy to the kind cluster and confirm: the operator's own lines appear and
  the body dumps do not; `Reconciler error` carries
  `controller` / object / `reconcileID`; the API's `[controller-runtime]
  log.SetLogger(...) was never called` warning is gone; the supervisor's own
  lines are unchanged in content and now share the format; `kubectl logs` on a
  fresh pod stays well under kubelet's rotation threshold for a comparable
  window.
- Set `logLevel: 4` and confirm the body dumps stay suppressed; set `8` and
  confirm they return, since that is the documented consequence.
