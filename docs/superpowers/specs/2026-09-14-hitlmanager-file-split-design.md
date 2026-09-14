# `hitlManager`: split by file, rename by what it owns (issue #171) -- design

Date: 2026-09-14 · Status: draft for review · Scope: issue #171

## Context

`hitlManager` ([hitl.go:103](https://github.com/suanova/cubepilot/blob/3e24569/internal/server/hitl.go#L103)) is the per-user **gateway
connection** owner. Its name and doc comment
([hitl.go:100](https://github.com/suanova/cubepilot/blob/3e24569/internal/server/hitl.go#L100)) describe only one of the jobs it
carries: the comment still reads "owns the per-user approval connections", which
stopped being true when interactive chat moved onto the same connection
(issue #130) and `ask_user` followed (issue #161).

The result is a reader cost, not a correctness one. Tracing "where does a chat
turn get its data" leads into a file called `hitl.go`, and three of the five job
families found there are not HITL at all.

## Verified facts

Measured against `upstream/main` at `3e24569`. The file and line references
below point at `hitl.go` as it stood at that commit -- the file this change
replaces, and the thing being described here.

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
([hitl.go:127-138](https://github.com/suanova/cubepilot/blob/3e24569/internal/server/hitl.go#L127-L138)). Live turns already have
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
  `ResolveApproval` lives in `hitl.go` ([:767](https://github.com/suanova/cubepilot/blob/3e24569/internal/server/hitl.go#L767)).
- [abort.go](../../../internal/server/abort.go) (418 lines) is the **largest
  consumer** of these methods outside `hitl.go`, with 7 call sites;
  [settle.go](../../../internal/server/settle.go) has 2.
- [livetools.go](../../../internal/server/livetools.go) (448 lines) already holds
  `liveProjector`. Only the live-turn *bookkeeping* remains in `hitl.go`.

**The connection is where the families intersect.** `conn()` installs four event
callbacks, each dispatching into a different family
([hitl.go:341-370](https://github.com/suanova/cubepilot/blob/3e24569/internal/server/hitl.go#L341-L370)):

```
conn() ──▶ m.bridge              ──▶ approval service
       ──▶ m.questionRequested   ──▶ question relay
       ──▶ m.questionResolved    ──▶ question relay
       ──▶ m.routeLive()         ──▶ live turns
```

Three of the four are already **injected `func` fields set by the server**
([hitl.go:112-117](https://github.com/suanova/cubepilot/blob/3e24569/internal/server/hitl.go#L112-L117)), not direct method calls.
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
mechanical identifier substitution plus a file move.

### Target layout

All under `internal/server/`. Sizes are as built.

| File | Contents | lines |
| --- | --- | --- |
| `gateway.go` (renamed from `hitl.go`) | Device identity (`deviceFor`, `DevicePublicKeyFor`, `mustDevice`, `wsURL`), connection pool (`conn`, `liveConn`, `gatewayConnected`, `hasConnectedLocked`, `markConnected`), `openClawLiveRunner`, the three injected hooks, `errNoGatewayChannel`, `channelProbeTimeout` | 412 |
| `live.go` (new) | `liveTurn` and its five methods, `wsRunTail`, `registerLive`, `releaseLive`, `RunLiveTurn`, `routeLive`, `LiveRunID`, `chatTerminalOutcome` | 314 |
| `approvals.go` (existing, 400) | + `PreTurn`, `channelState`, `applyPolicy`, `toWSEntries`, `ResolveApproval` | 550 |
| `abort.go` (existing, 418) | + `Abort`, `SessionBusy`, `SessionBusyEstablished`, `connEstablished`, `InFlightRunID` | 523 |
| `questions.go` (existing, 358) | + `GetQuestion`, `ListQuestions`, `ResolveQuestion`, `CancelQuestion` | 396 |

Each file ends between 314 and 550 lines. After the split, a reader tracing a
chat turn finds the turn lifecycle in `live.go`, the connection it runs over in
`gateway.go`, and the pre-turn policy gate in `approvals.go` -- three files whose
names say what they hold. The connection is still on the path (`RunLiveTurn`
calls `conn`); what is no longer in the way is the `ask_user` relay, which is not
part of a chat turn at all.

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
| `fakeHitlGateway` (test fake) | `fakeGatewayClient` | 96 |
| `newTestHitl` (test helper) | `newTestGatewayConns` | 42 |
| `blockingHitlGateway` (test fake) | `blockingGatewayClient` | 5 |
| `Server.hitl` (the field) | `Server.gatewayConns` | 37 |

The three test fakes were found during implementation, not in the first pass: a
grep for `hitl`-prefixed identifiers missed them because they carry `Hitl`
mid-name. They are reference identifiers -- they name or construct the renamed
types -- so leaving them would show a `fakeGatewayClient`-shaped hole in the
rename.

**`Server.hitl` was added late, against this document's first judgement.** The
first draft listed it under "Deliberately kept", reasoning that it is the binding
of the `EnableHITL` concept and so moves with that name. Reading the call sites
showed the reasoning was wrong: of the field's 37 references, 24 are production
call sites and only one of those (`channelState`) is about approvals at all. The
rest are chat turns, aborts and questions. `EnableHITL` names an *action* -- turn
the feature on -- and keeping it is right; `Server.hitl` names a *resource*
handle, and at 23 of its 24 production uses that name is false. The rule is not
that the two must agree.

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
- **`TestHitl_*` test names**, and the `hitl bool` field of the two case tables
  in `abort_test.go`. Both are scenario labels -- "this case runs with HITL on" --
  rather than references to a type, and renaming them is noise for the reader
  this issue is about.
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
- **`git` does not recognise the split as a rename.** Only 412 of the original
  1008 lines stay in `gateway.go`, so the pair scores ~41% similarity and falls
  under the default 50% threshold: `git diff -M` shows a 1008-line delete plus a
  412-line add. Lowering the threshold (`git diff -M25%`) or reviewing the five
  files directly is the way to read it. This is a consequence of the file
  genuinely shrinking, not of an unclean move.
- Reviewable by construction instead, with the comparison normalizing the
  documented renames: once the substitution is applied to the original, each
  moved block is byte-identical to what landed in its new file, and the multiset
  of the original's lines is a subset of the five files' lines. Both were checked
  mechanically when the change was made -- "identical modulo the renames in the
  table above", not "identical to the literal original bytes".
- Note for blame: a pure move breaks `git blame` on the moved lines. `git blame
  -C -M` follows them. This is accepted rather than worked around.

## Non-goals

- No behaviour change, no new methods, no signature change.
- No struct split (above).
- No new tests. The existing ones are the verification.
- Nothing outside `internal/server/`.
- The `hitl`-prefixed log strings and Secret name stay as they are.
