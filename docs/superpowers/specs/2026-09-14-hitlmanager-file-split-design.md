# `hitlManager`: split by file, rename by what it owns (issue #171) -- design

Date: 2026-09-14 · Status: draft for review · Scope: issue #171

## Context

`hitlManager` ([hitl.go:103](../../../internal/server/hitl.go)) is the per-user **gateway
connection** owner. Its name and doc comment
([hitl.go:100](../../../internal/server/hitl.go)) describe only one of the jobs it
carries: the comment still reads "owns the per-user approval connections", which
stopped being true when interactive chat moved onto the same connection
(issue #130) and `ask_user` followed (issue #161).

The result is a reader cost, not a correctness one. Tracing "where does a chat
turn get its data" leads into a file called `hitl.go`, and three of the five job
families found there are not HITL at all.

## Verified facts

Measured against `upstream/main` at `3e24569`.

**The file has grown since the issue was filed.** 736 lines when #171 was opened
(2026-09-11), **1008** now. The growth came from the same direction the issue
describes: #166/#168 added `Abort`, `SessionBusy`, `SessionBusyEstablished`,
`connEstablished`, `InFlightRunID`, `LiveRunID`, `gatewayConnected`,
`hasConnectedLocked` and `markConnected` -- all connection work, none of it HITL.
The drift is ongoing, not historical.

**Five job families, three of them not HITL:**

| Job | Methods | HITL? |
| --- | --- | --- |
| Device identity | `deviceFor`, `DevicePublicKeyFor`, `mustDevice`, `wsURL` | yes |
| Connection pool | `conn`, `liveConn`, `connEstablished`, `gatewayConnected`, `hasConnectedLocked`, `markConnected` | no |
| Approval policy | `PreTurn`, `channelState`, `applyPolicy`, `toWSEntries`, `ResolveApproval` | yes |
| `ask_user` questions | `GetQuestion`, `ListQuestions`, `ResolveQuestion`, `CancelQuestion` | no |
| Live chat turns | `liveTurn` + `setRunID`/`acceptRun`/`finish`/`finishWith`/`outcome`, `registerLive`, `releaseLive`, `RunLiveTurn`, `routeLive`, `LiveRunID`, `chatTerminalOutcome` | no |

**Two state groups, already half-segregated.** `mu` guards `conns`, `connecting`
and `revPol`; `liveMu` guards `live`
([hitl.go:127-138](../../../internal/server/hitl.go)). Live turns already have
their own lock and table. The only policy state sharing `mu` with the connection
pool is `revPol`, the applied-revision watermark.

**The per-concern handler files already exist; only the methods never moved.**

- [questions.go](../../../internal/server/questions.go) (358 lines) holds the
  `ask_user` relay, handlers and `questionRoutes` -- and **zero** `hitlManager`
  methods. The four question methods are called from it
  ([questions.go:228,248,250,282](../../../internal/server/questions.go)) but
  defined in `hitl.go`.
- [approvals.go](../../../internal/server/approvals.go) (400 lines) declares the
  `ApprovalResolver` interface
  ([approvals.go:56](../../../internal/server/approvals.go)) whose implementation
  `ResolveApproval` lives in `hitl.go` ([:767](../../../internal/server/hitl.go)).
- [abort.go](../../../internal/server/abort.go) (418 lines) is the **largest
  consumer** of these methods outside `hitl.go`, with 7 call sites;
  [settle.go](../../../internal/server/settle.go) has 2.
- [livetools.go](../../../internal/server/livetools.go) (448 lines) already holds
  `liveProjector`. Only the live-turn *bookkeeping* remains in `hitl.go`.

**The connection is where the families intersect.** `conn()` installs four event
callbacks, each dispatching into a different family
([hitl.go:341-370](../../../internal/server/hitl.go)):

```
conn() ──▶ m.bridge              ──▶ approval service
       ──▶ m.questionRequested   ──▶ question relay
       ──▶ m.questionResolved    ──▶ question relay
       ──▶ m.routeLive()         ──▶ live turns
```

Three of the four are already **injected `func` fields set by the server**
([hitl.go:112-117](../../../internal/server/hitl.go)), not direct method calls.
Only `routeLive` is called directly.

## Decision

**Split by file and rename the type. Do not split the type.**

Both halves of the issue's complaint -- the file misleads readers, the name lies
-- are fixed by moving methods between files and renaming identifiers. Neither
requires new types.

The reason this is cheap is a Go property, not a coincidence: methods may be
defined in any file of the same package. A moved method keeps its receiver and
its callers unchanged, so `RunLiveTurn` calling `m.conn(...)`, and `conn()`
referencing `m.routeLive`, compile untouched across the move. The diff is a
`git`-recognisable rename plus a mechanical identifier substitution.

### Target layout

All under `internal/server/`. Sizes are estimates.

| File | Contents | ~lines |
| --- | --- | --- |
| `gateway.go` (renamed from `hitl.go`) | Device identity (`deviceFor`, `DevicePublicKeyFor`, `mustDevice`, `wsURL`), connection pool (`conn`, `liveConn`, `gatewayConnected`, `hasConnectedLocked`, `markConnected`), `openClawLiveRunner`, the three injected hooks, `errNoGatewayChannel`, `channelProbeTimeout` | 485 |
| `live.go` (new) | `liveTurn` and its five methods, `wsRunTail`, `registerLive`, `releaseLive`, `RunLiveTurn`, `routeLive`, `LiveRunID`, `chatTerminalOutcome` | 370 |
| `approvals.go` (existing, 400) | + `PreTurn`, `channelState`, `applyPolicy`, `toWSEntries`, `ResolveApproval` | 560 |
| `abort.go` (existing, 418) | + `Abort`, `SessionBusy`, `SessionBusyEstablished`, `connEstablished`, `InFlightRunID` | 520 |
| `questions.go` (existing, 358) | + `GetQuestion`, `ListQuestions`, `ResolveQuestion`, `CancelQuestion` | 395 |

Each file ends between 370 and 560 lines. After the split, tracing a chat turn
runs handler → `live.go` and never enters `gateway.go`.

`gateway.go` is the only file whose name still reflects the type; the other four
are named for the concern, and the methods now live where a reader would look.

### Rename

| From | To | Occurrences |
| --- | --- | --- |
| `hitlManager` | `gatewayConns` | 66 |
| `hitlGateway` (the WS client interface) | `gatewayClient` | 21 |
| `userHitlConn` | `userGatewayConn` | 43 |
| `ConfiguredHITL` | `ConfigureGateway` | 5 |
| `hitlPairRetryDelay` | `pairRetryDelay` | 7 |

Occurrences span five files: `hitl.go`, `server.go`, `hitl_test.go`,
`abort_test.go`, `questions_test.go`. `abort_test.go` constructs
`&hitlManager{conns: map[string]*userHitlConn{...}}` directly at ~21 sites, which
is why the unexported field-and-type names dominate the count. Every occurrence is
a mechanical substitution.

`gatewayClient` also disambiguates two names that currently collide in reading:
`hitlGateway` is the WS client interface, while `hitlManager` is the pool that
holds them.

### Deliberately kept

- **`EnableHITL`** and the `hitl:` log prefix. HITL is a real product concept --
  human-in-the-loop approval -- and those names do not lie. Renaming them would
  spread the diff into `server.go`'s Secret handling and log output for no gain.
- **One type.** `gatewayConns` holds connection state *and* the approval-policy
  watermark. That is a minor grouping, worth splitting only if a second
  non-connection consumer of `revPol` appears. Splitting it now costs a new mutex
  and hands two other types a back-reference to the connection owner.
- **`SetConnFactory`** (2 occurrences). Still accurate: tests inject a factory.

## Rejected alternative: the four-way struct split in the issue

The issue proposes `connPool` / `approvalGate` / `questionRelay` / `liveTurns`,
described as "one mechanical PR that moves code without editing it". Three of the
four are not mechanical:

1. **`conn()` must be edited.** Its four callbacks dispatch into three different
   proposed structs. After the split, `connPool` needs a back-reference to
   `approvalGate`, `questionRelay` and `liveTurns` -- or, for three of them, to
   reuse the injection pattern already in place. Either way it is wiring work in
   the one function everything else depends on, not a code move.
2. **`questionRelay` has no state.** Its four methods are the same template --
   `liveConn(user)`, then delegate. A type whose every method forwards holds
   nothing to own. Those methods belong beside their handlers in `questions.go`,
   as ordinary methods on the connection owner.
3. **`revPol` is the only real entanglement.** It shares `mu` with the pool; a
   separate `approvalGate` needs its own mutex for one `map[string]string`.

The split's cost lands where review cannot check it cheaply: ~2400 lines of tests
(`hitl_test.go` 1309, `abort_test.go` 1121) would have to be divided by receiver,
and 16 call sites change shape. The benefit over the file split is a type
boundary, which no reader of the issue asked for.

This is recorded so the reduced scope is a decision, not an oversight.

## Verification

The change is behaviour-neutral by construction, so the bar is that nothing
observable moves:

- `go build ./...` and `go vet ./...` clean.
- `go test ./internal/server/...` green, before and after, with no test edited
  beyond identifiers. `hitl_test.go` and `abort_test.go` are the load-bearing
  ones: they cover fail-closed policy gating, the pairing retry, live-turn
  projection, and the abort/stop path.
- Review with `git diff -M`; the moves should register as renames.
- Note for blame: a pure move breaks `git blame` on the moved lines. `git blame
  -C -M` follows them. This is accepted rather than worked around.

## Non-goals

- No behaviour change, no new methods, no signature change.
- No struct split (above).
- No new tests. The existing ones are the verification.
- Nothing outside `internal/server/`.
- The `hitl`-prefixed log strings and Secret name stay as they are.
