# Platform-wide log level Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give the platform a working, deploy-time-settable log level, replace the sink that ignores it, and fix the log lines a human cannot read.

**Architecture:** One `logr.LogSink` in a new `internal/logging` package replaces `internal/logrlog`. Each of the three binaries reaches it through the path that binary actually uses -- `ctrllog.SetLogger` for the operator and api, which run controller-runtime, and `klog.SetLoggerWithOptions(..., klog.ContextualLogger(true))` for the supervisor, which does not. The level is read from env by `config.Load` / `supervisor.LoadFromEnv`, set by the chart, and defaults to 0.

**Tech Stack:** Go 1.26, `github.com/go-logr/logr`, `sigs.k8s.io/controller-runtime` v0.25.0, `k8s.io/klog/v2` v2.140.0, Helm 3, stdlib `testing` (no testify).

**Spec:** `docs/superpowers/specs/2026-09-17-platform-logging-level-design.md`

## Global Constraints

- All code, comments and log messages are English. ASCII punctuation only -- no em-dash, ellipsis or arrow characters.
- Tests use the stdlib `testing` package. The repo has no testify and must not gain one.
- `make test` runs `go vet ./...` and `go test`; it does **not** run golangci-lint. Run `golangci-lint run` separately before every commit.
- Work happens in this worktree on branch `feat/issue209-platform-logging-level`. Every commit is `-s` with an English message ending in:
  ```
  Assisted-by: Claude Code
  Co-Authored-By: Claude Code <noreply@anthropic.com>
  ```
- The default level is `0` everywhere. Do not add a ceiling.
- cubepilot's own `log.Printf` call sites are **not** re-leveled. The level governs dependency verbosity only.

---

### Task 1: The `internal/logging` sink

Replaces `internal/logrlog`, switches the operator to it, and deletes the old package. At the end of this task the operator has a working log level of 0 and the body dumps are gone.

**Files:**
- Create: `internal/logging/logging.go`
- Create: `internal/logging/logging_test.go`
- Modify: `cmd/cubepilot-operator/main.go:31` (import) and `:39` (call)
- Delete: `internal/logrlog/` (one file, `logrlog.go`)

**Interfaces:**
- Consumes: nothing.
- Produces: `logging.New(level int) logr.Logger`, and the unexported `newSink(w io.Writer, level int) *sink` that the tests use. Task 2 changes the level argument at the operator call site; Task 3 adds two more call sites.

- [ ] **Step 1: Write the failing test**

Create `internal/logging/logging_test.go`:

```go
package logging

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func newTestSink(level int) (*sink, *bytes.Buffer) {
	var buf bytes.Buffer
	return newSink(&buf, level), &buf
}

// out returns the rendered lines without the trailing newline log.Print adds.
func out(buf *bytes.Buffer) string {
	return strings.TrimRight(buf.String(), "\n")
}

func TestEnabledHonorsLevel(t *testing.T) {
	s, _ := newTestSink(4)
	for _, tc := range []struct {
		level int
		want  bool
	}{
		{0, true}, {1, true}, {4, true}, {5, false}, {8, false},
	} {
		if got := s.Enabled(tc.level); got != tc.want {
			t.Errorf("Enabled(%d) = %v, want %v", tc.level, got, tc.want)
		}
	}
}

// The regression this whole change exists for: client-go's V(8) body dump must
// not reach the sink at the default level. If this fails, the operator starts
// printing request and response bodies again.
func TestBodyDumpIsDroppedAtDefaultLevel(t *testing.T) {
	s, buf := newTestSink(0)
	s.Info(8, "Response Body", "body", "00000000  6b 38 73 00")
	if buf.Len() != 0 {
		t.Fatalf("V(8) reached the sink at level 0: %q", out(buf))
	}
}

func TestErrorPrintsAtDefaultLevel(t *testing.T) {
	s, buf := newTestSink(0)
	s.Error(errors.New("boom"), "Reconciler error")
	got := out(buf)
	if !strings.Contains(got, "ERROR") || !strings.Contains(got, "err=boom") {
		t.Fatalf("Error did not render: %q", got)
	}
}

// controller-runtime attaches controller / object / reconcileID through
// WithValues. Dropping them is what left reconcile errors with no subject.
func TestWithValuesAccumulates(t *testing.T) {
	s, buf := newTestSink(0)
	s.WithValues("controller", "agentinstance").
		WithValues("reconcileID", "abc").
		Info(0, "Reconciling")
	got := out(buf)
	for _, want := range []string{"controller=agentinstance", "reconcileID=abc"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
}

func TestWithNameComposes(t *testing.T) {
	s, buf := newTestSink(0)
	s.WithName("controller").WithName("agentinstance").Info(0, "hi")
	if got := out(buf); !strings.Contains(got, "controller/agentinstance: hi") {
		t.Fatalf("got %q", got)
	}
}

func TestEmptyValuesRenderNoBrackets(t *testing.T) {
	s, buf := newTestSink(0)
	s.Info(0, "Starting metrics server")
	got := out(buf)
	if strings.Contains(got, "[]") {
		t.Errorf("empty kv rendered brackets: %q", got)
	}
	if !strings.HasSuffix(got, "Starting metrics server") {
		t.Errorf("unexpected trailing content: %q", got)
	}
}

func TestValueWithSpaceIsQuoted(t *testing.T) {
	s, buf := newTestSink(0)
	s.Error(errors.New(`secrets "user-admin-kubeconfig" not found`), "Reconciler error")
	if got := out(buf); !strings.Contains(got, `err="secrets \"user-admin-kubeconfig\" not found"`) {
		t.Fatalf("got %q", got)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/logging/ -count=1`
Expected: FAIL to build -- `undefined: newSink`, `undefined: sink`.

- [ ] **Step 3: Write the sink**

Create `internal/logging/logging.go`:

```go
// Package logging provides the platform's logr sink: one line per record on
// stderr, gated by a verbosity level.
//
// controller-runtime and client-go reach this sink through logr, so the level
// passed to New is what decides how much of their verbosity reaches the pod
// log. Left ungated, the effective verbosity is the highest level anything in
// the dependency tree asks for, which is V(10) -- and V(8) alone prints whole
// request and response bodies.
package logging

import (
	"fmt"
	"io"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/go-logr/logr"
)

// rfc3339Millis is the timestamp format. Milliseconds are enough to order
// records within a second without the noise of full nanosecond precision.
const rfc3339Millis = "2006-01-02T15:04:05.000Z"

// sink is a logr.LogSink rendering one line per record.
//
// It owns its *log.Logger instead of using the global one: the components log
// their own messages through log.Printf, and that global instance's flags must
// keep producing the timestamps those lines have always carried.
type sink struct {
	out    *log.Logger
	name   string
	values []any
	max    int
}

var _ logr.LogSink = (*sink)(nil)

// New returns a logr.Logger writing to stderr. Level 0 emits V(0) and every
// Error; each increment admits one more V level from controller-runtime and
// client-go.
//
// Level 8 is client-go's request/response body dump (rest/request.go logBody),
// which hex-dumps the body whole -- Secret bodies included. Raise the level
// past 7 only to watch actual API traffic, and expect that.
func New(level int) logr.Logger {
	return logr.New(newSink(os.Stderr, level))
}

func newSink(w io.Writer, level int) *sink {
	return &sink{out: log.New(w, "", 0), max: level}
}

func (l *sink) Init(logr.RuntimeInfo) {}

// Enabled is the level gate, and it is what makes the configured level
// authoritative for dependency verbosity -- but not because logr's own
// Logger.V consults it. V only accumulates the requested level; it never
// calls Enabled and never returns a null logger. The gate bites at call
// sites that ask Enabled before doing expensive work, the way client-go's
// rest/request.go logBody does:
//
//	if loggerV := logger.V(8); loggerV.Enabled() {
//		loggerV.Info(prefix, "body", hex.Dump(body))
//	}
//
// The Enabled() check precedes hex.Dump, so a false answer means the dump is
// never built. A call site that instead passes an already-computed value to
// Logger.Info is evaluated regardless of level -- Info runs after Go has
// already built the argument -- and must guard itself the same way.
func (l *sink) Enabled(level int) bool { return level <= l.max }

func (l *sink) Info(level int, msg string, kv ...any) {
	if !l.Enabled(level) {
		return
	}
	l.out.Print(l.line("INFO", msg, kv))
}

// Error is deliberately not gated: it prints at every level.
func (l *sink) Error(err error, msg string, kv ...any) {
	l.out.Print(l.line("ERROR", msg, append([]any{"err", err}, kv...)))
}

// WithValues accumulates. controller-runtime builds its per-reconcile logger
// this way (controller / object / reconcileID), so returning the receiver
// would leave every reconcile error without a subject.
func (l *sink) WithValues(kv ...any) logr.LogSink {
	if len(kv) == 0 {
		return l
	}
	values := make([]any, 0, len(l.values)+len(kv))
	values = append(values, l.values...)
	values = append(values, kv...)
	return &sink{out: l.out, name: l.name, values: values, max: l.max}
}

func (l *sink) WithName(name string) logr.LogSink {
	if name == "" {
		return l
	}
	if l.name != "" {
		name = l.name + "/" + name
	}
	return &sink{out: l.out, name: name, values: l.values, max: l.max}
}

// line renders "<RFC3339 millis> <LEVEL> <logger>: <msg> <k=v ...>", with the
// name carried by WithValues first so a line still names its subject.
func (l *sink) line(level, msg string, kv []any) string {
	var b strings.Builder
	b.WriteString(time.Now().UTC().Format(rfc3339Millis))
	b.WriteString(" ")
	b.WriteString(level)
	for i := len(level); i < 5; i++ {
		b.WriteString(" ")
	}
	b.WriteString(" ")
	if l.name != "" {
		b.WriteString(l.name)
		b.WriteString(": ")
	}
	b.WriteString(msg)
	for _, group := range [][]any{l.values, kv} {
		for i := 0; i+1 < len(group); i += 2 {
			b.WriteString(" ")
			b.WriteString(key(group[i]))
			b.WriteString("=")
			b.WriteString(value(group[i+1]))
		}
	}
	return b.String()
}

func key(k any) string {
	if s, ok := k.(string); ok {
		return s
	}
	return fmt.Sprint(k)
}

// value renders a value so a copy-paste of the line is unambiguous: anything
// containing whitespace is quoted, and a value implementing fmt.Stringer (an
// error, a namespaced name) is asked for its own rendering.
func value(v any) string {
	if v == nil {
		return "null"
	}
	if err, ok := v.(error); ok && err == nil {
		return "null"
	}
	s := fmt.Sprint(v)
	if s == "" || strings.ContainsAny(s, " \t\n\"") {
		return strconv.Quote(s)
	}
	return s
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/logging/ -count=1`
Expected: PASS (7 tests).

- [ ] **Step 5: Switch the operator to the new sink**

In `cmd/cubepilot-operator/main.go`, change the import at line 31 from
`"github.com/suanova/cubepilot/internal/logrlog"` to
`"github.com/suanova/cubepilot/internal/logging"` (it keeps the same position:
`internal/k8s` < `internal/logging` < `internal/runner`), and line 39 from:

```go
	ctrllog.SetLogger(logrlog.New())
```

to:

```go
	ctrllog.SetLogger(logging.New(0))
```

The literal `0` is replaced by `cfg.LogLevel` in Task 2.

- [ ] **Step 6: Delete the old package and verify nothing references it**

```bash
rm -rf internal/logrlog
grep -rn "logrlog" --include='*.go' . || echo "no references"
go build ./...
```

Expected: `no references`, then a clean build.

- [ ] **Step 7: Lint**

Run: `golangci-lint run`
Expected: no findings. Deleting a package is exactly when `unused` and `errcheck` surface leftovers.

- [ ] **Step 8: Commit**

Step 6 already deleted the files from disk, so `git add -A` stages the deletions.

```bash
git add -A internal/logging internal/logrlog cmd/cubepilot-operator/main.go
git status --short            # expect: A internal/logging/..., D internal/logrlog/...
git commit -s -m "fix(logging): gate logr output by level and stop dropping values

The sink answered Enabled(level) with an unconditional true and ignored the
level in Info, so every V(1)-V(10) call site in client-go and
controller-runtime was emitted -- 81.8% of the operator log was client-go's
hex-dumped request and response bodies. WithValues returned the receiver,
which discarded the per-reconcile logger and left reconcile errors with no
object identity.

Assisted-by: Claude Code
Co-Authored-By: Claude Code <noreply@anthropic.com>"
```

---

### Task 2: Make the level settable

Adds the config field, the supervisor's own copy of it, the chart values and templates, and the plumbing that gets the level into agent Pods so the supervisor is set independently of the operator.

**Files:**
- Modify: `internal/config/config.go` (struct field, and `Load`)
- Modify: `internal/supervisor/supervisor.go` (`Config` struct, `LoadFromEnv`, add `getInt`)
- Modify: `internal/k8s/resources.go` (`AgentSpec` struct at `:17-38`, env list at `:222-259`)
- Modify: `internal/controller/agentinstance_controller.go:116` (`AgentSpec` literal)
- Modify: `cmd/cubepilot-operator/main.go:39` (use `cfg.LogLevel`)
- Modify: `deploy/charts/cubepilot-chart/values.yaml`
- Modify: `deploy/charts/cubepilot-chart/templates/operator.yaml`
- Modify: `deploy/charts/cubepilot-chart/templates/api.yaml`

**Interfaces:**
- Consumes: `logging.New(int)` from Task 1.
- Produces: `config.Config.LogLevel int` and `config.Config.AgentLogLevel int`; `supervisor.Config.LogLevel int`; `k8s.AgentSpec.LogLevel int`. Task 3 reads `cfg.LogLevel` at the api and supervisor call sites.

**Why two operator-side fields:** `agents.logLevel` must reach the agent Pods without the operator's own level leaking into them. The supervisor's client-go calls read Secrets, so a level raised to debug the operator must not silently raise them too. The operator therefore reads `CUBEPILOT_AGENT_LOG_LEVEL` (chart: `agents.logLevel`) and passes that value into `AgentSpec`, while `CUBEPILOT_LOG_LEVEL` is its own.

- [ ] **Step 1: Add the operator/api config fields**

In `internal/config/config.go`, add to the `Config` struct (after `ProbeAddr`):

```go
	// LogLevel is the verbosity handed to controller-runtime and client-go.
	// 0 emits V(0) and every error; each increment admits one more V level.
	// Level 8 is client-go's request/response body dump, which prints whole
	// objects -- Secret bodies included.
	LogLevel int

	// AgentLogLevel is the level the operator passes to the per-user agent
	// Pods for their supervisor. Separate from LogLevel because the
	// supervisor's client-go calls read Secrets: raising the operator's level
	// to debug it must not raise theirs.
	AgentLogLevel int
```

and to `Load`, after the `ProbeAddr` line:

```go
		LogLevel:      getInt("CUBEPILOT_LOG_LEVEL", 0),
		AgentLogLevel: getInt("CUBEPILOT_AGENT_LOG_LEVEL", 0),
```

- [ ] **Step 2: Add the supervisor's config field**

In `internal/supervisor/supervisor.go`, add to the `Config` struct:

```go
	// LogLevel is the verbosity handed to client-go through klog. 0 emits V(0)
	// and every error; level 8 is client-go's request/response body dump,
	// which prints whole objects -- Secret bodies included.
	LogLevel int
```

Add to the `LoadFromEnv` literal, after `CredentialsPath`:

```go
		LogLevel:        getInt("CUBEPILOT_LOG_LEVEL", 0),
```

and add the helper next to the existing `getenv` at `supervisor.go:90`. This
package has its own copy of these env helpers rather than sharing
`internal/config`'s; `config.go:148` has an identical `getInt`, and the body
below matches it, including falling back to the default on an unparsable value
rather than failing the process:

```go
func getInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}
```

Add `"strconv"` to that file's imports (alphabetically after `"os/exec"` and
before `"path/filepath"`).

- [ ] **Step 3: Pass the level into agent Pods**

In `internal/k8s/resources.go`, add to the `AgentSpec` struct:

```go
	// LogLevel is the supervisor's CUBEPILOT_LOG_LEVEL inside the Pod. It
	// comes from the operator's AgentLogLevel, not its own LogLevel.
	LogLevel int
```

and to the container env list (after the `CUBEPILOT_WORKSPACE` entry):

```go
					// The supervisor's own verbosity. Set from the operator's
					// CUBEPILOT_AGENT_LOG_LEVEL so it stays independent of the
					// operator's level.
					{Name: "CUBEPILOT_LOG_LEVEL", Value: strconv.Itoa(s.LogLevel)},
```

Add `"strconv"` to that file's imports.

In `internal/controller/agentinstance_controller.go`, add to the `k8s.AgentSpec` literal at line 116:

```go
		LogLevel:     r.Cfg.AgentLogLevel,
```

- [ ] **Step 4: Use the configured level in the operator**

In `cmd/cubepilot-operator/main.go`, change line 39 from `logging.New(0)` to:

```go
	ctrllog.SetLogger(logging.New(cfg.LogLevel))
```

- [ ] **Step 5: Build and run the existing tests**

Run: `go build ./... && go test ./internal/config/ ./internal/supervisor/ ./internal/k8s/ ./internal/controller/ -count=1`
Expected: PASS. Nothing reads `AgentLogLevel` in a way that changes behavior at the default of 0, so existing tests are unaffected.

- [ ] **Step 6: Add the chart values**

In `deploy/charts/cubepilot-chart/values.yaml`, add to the `agents` block:

```yaml
  # Log verbosity for the per-user agent Pods' supervisor. 0 emits V(0) and
  # every error; each increment admits one more V level from client-go. Kept
  # separate from operator.logLevel and api.logLevel because the supervisor's
  # client-go calls read Secrets: debugging the operator must not start
  # dumping them. Above 7 those calls print full request and response bodies,
  # Secret bodies included.
  logLevel: 0
```

and to the `operator` block:

```yaml
  # Log verbosity for this component. 0 emits V(0) and every error; each
  # increment admits one more V level from client-go and controller-runtime.
  # Above 7 the operator prints full request and response bodies, Secret
  # bodies included -- raise it only to watch actual API traffic. There is no
  # ceiling: the default is the defence, not the range.
  logLevel: 0
```

and to the `api` block:

```yaml
  # Log verbosity for this component. See operator.logLevel; the api reaches
  # the same sink, so the same consequence applies above 7.
  logLevel: 0
```

- [ ] **Step 7: Add the chart env entries**

In `deploy/charts/cubepilot-chart/templates/operator.yaml`, add after the `CUBEPILOT_PROBE_ADDR` entry:

```yaml
            - name: CUBEPILOT_LOG_LEVEL
              value: {{ .Values.operator.logLevel | quote }}
            # The agent Pods the operator creates carry this value, not the
            # operator's own -- see values.yaml.
            - name: CUBEPILOT_AGENT_LOG_LEVEL
              value: {{ .Values.agents.logLevel | quote }}
```

In `deploy/charts/cubepilot-chart/templates/api.yaml`, add after the `CUBEPILOT_SKILLS_DIR` entry:

```yaml
            - name: CUBEPILOT_LOG_LEVEL
              value: {{ .Values.api.logLevel | quote }}
```

- [ ] **Step 8: Verify the chart renders**

```bash
helm lint deploy/charts/cubepilot-chart
helm template cubepilot deploy/charts/cubepilot-chart -n cubepilot > /tmp/rendered.yaml
grep -n -A1 'CUBEPILOT_LOG_LEVEL\|CUBEPILOT_AGENT_LOG_LEVEL' /tmp/rendered.yaml
```

Expected: lint passes; the rendered output shows `CUBEPILOT_LOG_LEVEL` on the operator and api containers and `CUBEPILOT_AGENT_LOG_LEVEL` on the operator container, all `"0"`.

- [ ] **Step 9: Lint**

Run: `golangci-lint run`

- [ ] **Step 10: Commit**

```bash
git add -A
git commit -s -m "feat(logging): make the log level settable per component

CUBEPILOT_LOG_LEVEL configures the operator and api; the operator reads
CUBEPILOT_AGENT_LOG_LEVEL (chart agents.logLevel) and passes it to the agent
Pods' supervisor, so raising the operator's own level cannot silently start
dumping the Secret reads the supervisor makes. All default to 0.

Assisted-by: Claude Code
Co-Authored-By: Claude Code <noreply@anthropic.com>"
```

---

### Task 3: Wire the api and the supervisor

The api currently discards its controller-runtime output through `NullLogSink`; the supervisor has no logr at all and falls back to klog's global logger.

**Files:**
- Modify: `cmd/cubepilot-api/main.go` (import, and a call after `cfg := config.Load()`)
- Modify: `cmd/cubepilot-supervisor/main.go` (import, and a call after `cfg := supervisor.LoadFromEnv()`)

**Interfaces:**
- Consumes: `logging.New(int)` (Task 1), `config.Config.LogLevel` and `supervisor.Config.LogLevel` (Task 2).
- Produces: nothing further; this is the last wiring.

- [ ] **Step 1: Wire the api**

In `cmd/cubepilot-api/main.go`, add the import
`sigs.k8s.io/controller-runtime/pkg/log` as `ctrllog` (alongside the existing
`sigs.k8s.io/controller-runtime/pkg/client`), add
`"github.com/suanova/cubepilot/internal/logging"` to the cubepilot group, and
add immediately after `cfg := config.Load()`:

```go
	// Route controller-runtime and client-go logs into the platform sink.
	// Without this, controller-runtime fulfils its deferred root logger with a
	// NullLogSink after 30s and prints a stack trace -- everything this process
	// would have logged is discarded.
	ctrllog.SetLogger(logging.New(cfg.LogLevel))
```

- [ ] **Step 2: Wire the supervisor**

In `cmd/cubepilot-supervisor/main.go`, add the imports:

```go
	"k8s.io/klog/v2"

	"github.com/suanova/cubepilot/internal/logging"
```

and add immediately after `cfg := supervisor.LoadFromEnv()`:

```go
	// The supervisor runs no controller-runtime, so there is no context for a
	// logger to travel in: client-go's klog.FromContext falls back to
	// klog.Background(). Background returns the logger set here only when
	// ContextualLogger is set, so that option is load-bearing rather than
	// decorative.
	klog.SetLoggerWithOptions(logging.New(cfg.LogLevel), klog.ContextualLogger(true))
```

- [ ] **Step 3: Build and vet**

Run: `go build ./... && go vet ./...`
Expected: clean. A wrong klog option name or a missing import fails here.

- [ ] **Step 4: Confirm all three binaries reach the sink**

```bash
grep -rn "logging.New(" cmd/
```

Expected: three call sites -- `cmd/cubepilot-operator/main.go`,
`cmd/cubepilot-api/main.go`, `cmd/cubepilot-supervisor/main.go`.

- [ ] **Step 5: Lint and commit**

```bash
golangci-lint run
git add -A
git commit -s -m "fix(logging): route the api and supervisor logs into the platform sink

The api never called SetLogger, so controller-runtime fulfilled its deferred
root logger with a NullLogSink after 30s: its controller-runtime output was
discarded and it printed a stack-trace warning. The supervisor has no
controller-runtime at all, so klog.FromContext fell through to klog's global
logger; ContextualLogger is what makes klog.Background return the logger set
here.

Assisted-by: Claude Code
Co-Authored-By: Claude Code <noreply@anthropic.com>"
```

---

### Task 4: Resolve the Kind in the bootstrap log line

`bootstrap: created %s/%s` prints an empty Kind, because the bootstrapped objects are Go struct literals with no `TypeMeta` set. The line reads `bootstrap: created /admin-cubepilot`.

**Files:**
- Modify: `internal/controller/builtin.go:346`
- Modify: `internal/controller/builtin_test.go` (append; the file already exists and already defines `testScheme(t)`)

**Interfaces:**
- Consumes: `testScheme(t *testing.T) *runtime.Scheme` (already defined at `builtin_test.go:22`, registered with `v1alpha1`, `corev1`, `rbacv1` -- `corev1` is what a `ServiceAccount` needs).
- Produces: `func (r *BuiltinBootstrapReconciler) kindOf(obj client.Object) string`. Unexported; only the tests and `createIfMissing` use it.

- [ ] **Step 1: Append the failing tests**

`internal/controller/builtin_test.go` already exists with `TestBuiltinAgentShape`,
`TestInstanceNameFor`, `TestBootstrapEnsure`, `TestBootstrapEnsureNoDefaultModel`
and `TestBootstrapEnsureRejectsCollidingUsers`. **Append** to it; do not
overwrite it. The imports already present are `context`, `strings`, `testing`,
`corev1`, `rbacv1`, `metav1`, `runtime`, `types`, `client`, `fake`, `v1alpha1`,
`config`, `k8s`, `skill` -- everything these two tests need, so no import changes.

```go
func TestKindOfResolvesTypedObjects(t *testing.T) {
	r := &BuiltinBootstrapReconciler{Scheme: testScheme(t), Cfg: config.Config{}}

	// A typed literal with no TypeMeta -- exactly how the bootstrapped objects
	// are built, and why GetObjectKind().GroupVersionKind().Kind is empty.
	if got := r.kindOf(&corev1.ServiceAccount{}); got != "ServiceAccount" {
		t.Fatalf("kindOf = %q, want %q", got, "ServiceAccount")
	}
}

func TestCreateIfMissingNamesTheKind(t *testing.T) {
	scheme := testScheme(t)
	// A bare client: createIfMissing only needs Get to miss and Create to
	// succeed, and the kind comes from the scheme, not from a seeded object.
	cl := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &BuiltinBootstrapReconciler{Client: cl, Scheme: scheme, Cfg: config.Config{}}

	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(prev)

	obj := &corev1.ServiceAccount{}
	obj.Name = "admin-cubepilot"
	if err := r.createIfMissing(context.Background(), obj); err != nil {
		t.Fatalf("createIfMissing: %v", err)
	}
	if got := buf.String(); !strings.Contains(got, "bootstrap: created ServiceAccount/admin-cubepilot") {
		t.Fatalf("log = %q, want it to contain %q", got, "bootstrap: created ServiceAccount/admin-cubepilot")
	}
}
```

`TestCreateIfMissingNamesTheKind` is the only test in the package that captures
the stdlib logger, so it adds two imports: `"bytes"` and `"log"`. `strings` is
already there.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/controller/ -run 'TestKindOf|TestCreateIfMissing' -count=1`
Expected: FAIL to build -- `r.kindOf undefined`.

- [ ] **Step 3: Implement `kindOf` and use it**

In `internal/controller/builtin.go`, change the log call at line 346 from:

```go
	log.Printf("bootstrap: created %s/%s", obj.GetObjectKind().GroupVersionKind().Kind, obj.GetName())
```

to:

```go
	log.Printf("bootstrap: created %s/%s", r.kindOf(obj), obj.GetName())
```

and add the method:

```go
// kindOf resolves an object's Kind through the scheme.
//
// GetObjectKind().GroupVersionKind().Kind is empty for the objects bootstrapped
// here: they are built as typed Go literals with no TypeMeta, so the method
// returns "" and the log line reads "bootstrap: created /admin-cubepilot". The
// scheme knows the type even when the object does not.
func (r *BuiltinBootstrapReconciler) kindOf(obj client.Object) string {
	if kinds, _, err := r.Scheme.ObjectKinds(obj); err == nil && len(kinds) > 0 {
		return kinds[0].Kind
	}
	return "unknown"
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/controller/ -count=1`
Expected: PASS.

- [ ] **Step 5: Lint and commit**

```bash
golangci-lint run
git add internal/controller/builtin.go internal/controller/builtin_test.go
git commit -s -m "fix(controller): name the bootstrap object's kind in the log

The bootstrapped objects are typed Go literals with no TypeMeta, so
GetObjectKind().GroupVersionKind().Kind is empty and the line read
\"bootstrap: created /admin-cubepilot\". Resolve it through the scheme, which
is already a field on the reconciler.

Assisted-by: Claude Code
Co-Authored-By: Claude Code <noreply@anthropic.com>"
```

---

### Task 5: Render the first config sync as `(initial)`

The config-sync log call prints `config revision  -> 42c94f84db82` on the first sync, because there is no previous revision yet.

**Files:**
- Modify: `internal/supervisor/supervisor.go` (the config-sync `log.Printf` call)
- Modify: `internal/supervisor/supervisor_test.go` (append; the file already exists in package `supervisor`)

**Interfaces:**
- Consumes: nothing.
- Produces: `func revisionLabel(from string) string` (unexported, package `supervisor`).

- [ ] **Step 1: Append the failing test**

`revisionLabel` is eight lines and belongs with the file it is called from, so
its test goes in the existing `internal/supervisor/supervisor_test.go` rather
than a new file. **Append** it; the package already contains 18 tests and none
is named `TestRevisionLabel`. No new imports -- `testing` is already there.

```go
package supervisor

import "testing"

func TestRevisionLabel(t *testing.T) {
	for _, tc := range []struct {
		name string
		from string
		want string
	}{
		{"first sync has no previous revision", "", "(initial)"},
		{"later syncs name the revision being replaced", "42c94f84db82", "42c94f84db82"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := revisionLabel(tc.from); got != tc.want {
				t.Errorf("revisionLabel(%q) = %q, want %q", tc.from, got, tc.want)
			}
		})
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/supervisor/ -run TestRevisionLabel -count=1`
Expected: FAIL to build -- `undefined: revisionLabel`.

- [ ] **Step 3: Implement it and use it**

In `internal/supervisor/supervisor.go`, change the config-sync call from:

```go
	log.Printf("supervisor: config revision %s -> %s", s.current, cfg.Revision)
```

to:

```go
	log.Printf("supervisor: config revision %s -> %s", revisionLabel(s.current), cfg.Revision)
```

and add:

```go
// revisionLabel names the revision a config sync is replacing. The first sync
// has no previous revision, and printing the empty string produced
// "config revision  -> 42c94f84db82".
func revisionLabel(from string) string {
	if from == "" {
		return "(initial)"
	}
	return from
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/supervisor/ -count=1`
Expected: PASS.

- [ ] **Step 5: Lint and commit**

```bash
golangci-lint run
git add internal/supervisor/supervisor.go internal/supervisor/supervisor_test.go
git commit -s -m "fix(supervisor): label the first config sync as (initial)

The supervisor logged an empty previous revision on its first sync, so the
line read \"config revision  -> 42c94f84db82\".

Assisted-by: Claude Code
Co-Authored-By: Claude Code <noreply@anthropic.com>"
```

---

### Task 6: The API access log

`internal/server/server.go:262` defines `logRequests` as a pass-through, so `Handler()` installs a middleware named for a capability the API does not have.

**Files:**
- Modify: `internal/server/server.go:232` (call site) and `:262-266` (the function)
- Create: `internal/server/accesslog_test.go`

**Interfaces:**
- Consumes: `(*Server).logf` (already present at `server.go:269`).
- Produces: `func (s *Server) logRequests(next http.Handler) http.Handler`, `func isProbePath(path string) bool`, and the unexported `statusRecorder`.

**The trap:** `internal/server/handlers.go:153` does `flusher, ok := w.(http.Flusher)` for SSE. A `ResponseWriter` wrapper that does not implement `http.Flusher` makes that assertion fail and breaks every streaming response. `statusRecorder` must implement it, and Step 1 tests exactly that.

- [ ] **Step 1: Write the failing test**

Create `internal/server/accesslog_test.go`:

The three tests build the Server as `&Server{}` rather than through `New(...)`:
`logRequests` touches nothing but `s.logf`, and `New` would drag in a nil
manager and store. `internal/server` has no existing `captureLog`, `isProbePath`,
`statusRecorder` or `TestLogRequests`, so nothing here collides.

```go
package server

import (
	"bytes"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// captureLog redirects the stdlib logger for the duration of fn and returns
// what was written. s.logf goes through log.Printf.
func captureLog(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	prev := log.Writer()
	prevFlags := log.Flags()
	log.SetOutput(&buf)
	defer func() {
		log.SetOutput(prev)
		log.SetFlags(prevFlags)
	}()
	fn()
	return buf.String()
}

func TestLogRequestsRecordsMethodPathAndStatus(t *testing.T) {
	s := &Server{}
	h := s.logRequests(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte("hello"))
	}))

	got := captureLog(t, func() {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/v1/sessions", nil))
	})

	for _, want := range []string{"GET", "/api/v1/sessions", "418", "5B"} {
		if !strings.Contains(got, want) {
			t.Errorf("access line %q is missing %q", got, want)
		}
	}
}

func TestLogRequestsSkipsProbePaths(t *testing.T) {
	for _, path := range []string{"/healthz", "/readyz", "/metrics"} {
		if !isProbePath(path) {
			t.Errorf("isProbePath(%q) = false, want true", path)
		}
	}

	s := &Server{}
	h := s.logRequests(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	got := captureLog(t, func() {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/healthz", nil))
	})
	if got != "" {
		t.Errorf("probe path was logged: %q", got)
	}
}

// SSE depends on this: handlers.go:153 asserts w.(http.Flusher), so a wrapper
// that swallows it breaks every streaming response.
func TestLogRequestsPreservesFlusher(t *testing.T) {
	s := &Server{}
	var got bool
	h := s.logRequests(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, got = w.(http.Flusher)
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/v1/sessions/x/messages", nil))
	if !got {
		t.Fatal("the wrapped ResponseWriter no longer satisfies http.Flusher: SSE would break")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/server/ -run TestLogRequests -count=1`
Expected: FAIL to build -- `s.logRequests undefined` (the current `logRequests` is a package-level function), and `isProbePath undefined`.

- [ ] **Step 3: Implement the middleware**

In `internal/server/server.go`, change the call site at line 232 from
`return logRequests(mux)` to:

```go
	return s.logRequests(mux)
```

and replace the function at lines 262-266 with:

```go
// logRequests records one line per request: method, path, status, response
// size and duration.
//
// Probe paths are skipped rather than logged at a lower level. An access log
// belongs with the component's own messages, which are always visible, and the
// kubelet polls /healthz and /readyz every few seconds -- logging them buries
// what a human is looking for. That is not hypothetical: one tier down, 578 of
// 812 nginx lines are kube-probe hits.
func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isProbePath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		next.ServeHTTP(rec, r)
		s.logf("%s %s %d %dB %s", r.Method, r.URL.Path, rec.status, rec.bytes,
			time.Since(start).Round(time.Millisecond))
	})
}

// isProbePath reports whether path is polled on a fixed interval by the
// kubelet or by Prometheus scraping. Only /healthz and /metrics are registered
// today (server.go:175-176); /readyz is listed because a readiness endpoint is
// the obvious next one and the cost of the extra case is nothing.
func isProbePath(path string) bool {
	switch path {
	case "/healthz", "/readyz", "/metrics":
		return true
	}
	return false
}

// statusRecorder captures what the handler wrote, which the wrapped
// ResponseWriter does not expose.
//
// Flush is required, not optional: SSE handlers assert w.(http.Flusher)
// (handlers.go:153) and a wrapper without it turns every streaming response
// into a failure.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	n, err := r.ResponseWriter.Write(b)
	r.bytes += n
	return n, err
}

func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
```

`time` is already imported in `server.go`. If `Flush` turns out not to be
sufficient for a streaming path that uses `http.NewResponseController`, add:

```go
// Unwrap lets http.ResponseController reach the underlying writer.
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/server/ -count=1`
Expected: PASS, including `TestLogRequestsPreservesFlusher`.

- [ ] **Step 5: Lint and commit**

```bash
golangci-lint run
git add internal/server/server.go internal/server/accesslog_test.go
git commit -s -m "feat(server): implement the API access log the middleware was named for

logRequests was a pass-through installed as the outermost middleware, so the
name claimed a capability the API did not have. Probe paths are skipped: the
kubelet polls them every few seconds and one tier down 578 of 812 nginx lines
are kube-probe hits. The recorder forwards Flush, because the SSE handlers
assert w.(http.Flusher).

Assisted-by: Claude Code
Co-Authored-By: Claude Code <noreply@anthropic.com>"
```

---

## Final verification

After all six tasks:

- [ ] `go build ./... && go vet ./... && go test ./... -count=1` -- all green.
- [ ] `golangci-lint run` -- clean. `make test` does not run it.
- [ ] `helm lint deploy/charts/cubepilot-chart` -- passes.
- [ ] `grep -rn "logrlog" --include='*.go' .` -- no matches.
- [ ] Deploy to the kind cluster (`scripts/setup.sh`) and confirm against the
      spec's Verification section: the operator's own lines appear and the body
      dumps do not; `Reconciler error` carries `controller` / object /
      `reconcileID`; the `[controller-runtime] log.SetLogger(...) was never
      called` warning is gone from the api; the supervisor's lines share the
      format; `kubectl logs` on a fresh pod stays far below kubelet's 10 MiB
      rotation threshold for a comparable window.
- [ ] Set `operator.logLevel=4` and confirm body dumps stay suppressed; set `8`
      and confirm they return -- that is the documented consequence, and
      confirming it is what makes the values.yaml comment trustworthy.
- [ ] Open the PR against `upstream/main` per the `upstream-fork-pr` skill
      (issue #209), and answer every review comment.
