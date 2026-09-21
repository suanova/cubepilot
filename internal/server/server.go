// Package server exposes the CubePilot Portal and REST/SSE API, routing chat
// turns to per-user OpenClaw instances via the Instance Manager. Instance
// state comes from AgentInstance CRs; the server also serves the platform
// objects (Agent / Skill / Model / Task / TaskRun) over the API (design
// CubePilot-Cloud-for-Agents-Simplified-Design.md §2.1). The API process is
// stateless except for the JSON metadata store (audit / inspect reports /
// agent config) on a single RWO PVC. Conversation content lives only in the
// per-instance runtime (design §3.6) and is served from it; the API never
// keeps a message copy. Task scheduling is owned by the operator's CRD
// scheduler -- the API never runs cron loops.
package server

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/suanova/cubepilot/internal/config"
	"github.com/suanova/cubepilot/internal/instances"
	"github.com/suanova/cubepilot/internal/metrics"
	"github.com/suanova/cubepilot/internal/openclaw/ws"
	"github.com/suanova/cubepilot/internal/skill"
	"github.com/suanova/cubepilot/internal/store"
)

// Server holds shared dependencies for HTTP handlers.
type Server struct {
	cfg          config.Config
	mgr          *instances.Manager
	store        *store.Store
	catalog      *skill.Catalog
	cr           client.Client
	hub          *Hub
	approvals    *ApprovalService
	gatewayConns *gatewayConns   // nil when the channel is not up: gated policies fail closed and the confirm view reports "unconfigured"
	qroutes      *questionRoutes // gateway question id -> session, for ask_user events (issue #161)
}

// deviceRootSecretName is the Secret holding the auto-generated device root key
// (created by the API on first start; every per-user device identity is derived
// from it, so it must be stable across API restarts).
const deviceRootSecretName = "cubepilot-device-root"

// StartGatewayChannel brings up the API's gateway device channel (issue #20):
// approvals, ask-user questions and the live chat stream all ride the one
// connection per user.
//
// The device root key is auto-generated and persisted in a Secret
// (load-or-create), so bringing the channel up needs no operator-supplied key;
// the API's ServiceAccount only needs access to that one Secret -- the per-user
// devices derived from it are auto-paired by the in-pod supervisor.
//
// It returns an error when the channel cannot be brought up: since live chat
// itself runs over the gateway device channel (issue #130), a missing root key
// leaves the API unable to serve turns at all, so the caller must treat a
// failure as fatal rather than start half-configured (issue #127).
func (s *Server) StartGatewayChannel() error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	var rk []byte
	var sec corev1.Secret
	err := s.cr.Get(ctx, types.NamespacedName{Namespace: s.cfg.Namespace, Name: deviceRootSecretName}, &sec)
	switch {
	case err == nil:
		// The key is stored Base64-encoded; decode so a restarted API derives
		// the same device identities as the process that created the Secret.
		encoded := sec.Data["key"]
		// Assign to the function-level rk (not `rk, derr :=`, which would shadow
		// it inside this case and leave the outer rk empty) so a restarted API
		// actually re-uses the persisted key and keeps the channel up (issue #128).
		var derr error
		rk, derr = base64.StdEncoding.DecodeString(string(encoded))
		if derr != nil || len(rk) == 0 {
			return fmt.Errorf("gateway: device root Secret %s has an invalid 'key'", deviceRootSecretName)
		}
	case apierrors.IsNotFound(err):
		rk = make([]byte, 32)
		if _, rerr := rand.Read(rk); rerr != nil {
			return fmt.Errorf("gateway: generate device root key: %w", rerr)
		}
		sec = corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: deviceRootSecretName, Namespace: s.cfg.Namespace},
			Data:       map[string][]byte{"key": []byte(base64.StdEncoding.EncodeToString(rk))},
		}
		if cerr := s.cr.Create(ctx, &sec); cerr != nil {
			if !apierrors.IsAlreadyExists(cerr) {
				return fmt.Errorf("gateway: ensure device root Secret: %w", cerr)
			}
			// Lost the create race to a peer replica: adopt the winner's persisted
			// key so every replica derives the same device identities. Only a
			// successful read + decode replaces rk -- never fall through to
			// ConfigureGateway with the random bytes we just generated, which would
			// leave the API with unstable (unpersisted) device identities.
			var got corev1.Secret
			if rerr := s.cr.Get(ctx, types.NamespacedName{Namespace: s.cfg.Namespace, Name: deviceRootSecretName}, &got); rerr != nil {
				return fmt.Errorf("gateway: read device root Secret after create race: %w", rerr)
			}
			var derr error
			rk, derr = base64.StdEncoding.DecodeString(string(got.Data["key"]))
			if derr != nil || len(rk) == 0 {
				return fmt.Errorf("gateway: winner device root Secret has an invalid 'key'")
			}
		}
	default:
		return fmt.Errorf("gateway: read device root Secret: %w", err)
	}

	m := ConfigureGateway(s.mgr, s.cfg.GatewayToken, rk, s.logf)
	if m == nil {
		return fmt.Errorf("gateway: cannot build the channel manager (missing instance manager or gateway token)")
	}
	s.gatewayConns = m
	s.approvals.SetGateway(m)
	m.bridge = func(user string, ev ws.ApprovalRequested) {
		s.approvals.RelayRequested(user, ev)
	}
	// An approval the gateway ended by itself -- a decision taken anywhere, an
	// expiry, or a run aborted or lost gateway-side -- is relayed to its session's
	// stream from the broadcast. The broadcast carries the request, so it names
	// the session on its own: nothing the platform remembered is consulted, and
	// an approval this process never saw still reaches the right card.
	m.approvalResolved = func(user string, ev ws.ApprovalResolved) {
		s.relayApprovalResolved(user, ev)
	}
	// Ask-user questions ride the same device connection (issue #161): a
	// question the agent is blocked on is relayed onto the parked turn's SSE
	// stream, and its resolution is addressed back to the same session.
	m.questionRequested = func(_ string, rec ws.QuestionRecord) {
		s.relayQuestionRequested(rec)
	}
	m.questionResolved = func(_ string, res ws.QuestionResolved) {
		s.relayQuestionResolved(res)
	}
	s.logf("gateway: device root key %s ensured; chat and write confirmations enabled", deviceRootSecretName)
	return nil
}

// New builds the HTTP handler for the assistant service.
func New(cfg config.Config, mgr *instances.Manager, st *store.Store, catalog *skill.Catalog, cr client.Client) *Server {
	s := &Server{cfg: cfg, mgr: mgr, store: st, catalog: catalog, cr: cr}
	s.hub = NewHub()
	s.approvals = NewApprovalService(s.hub, st, s.logf)
	s.qroutes = newQuestionRoutes()
	return s
}

// apiPrefix is the versioned base of every client-facing endpoint. The version
// is frozen here so a future breaking change can be introduced as a new prefix
// without moving this surface out from under clients that already speak it.
// The cluster-internal endpoints under /internal/ are deliberately unversioned:
// their only consumer is the agent-side supervisor, which is versioned together
// with the API through the agent image.
const apiPrefix = "/api/v1"

// Handler returns the fully wired HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/metrics", metrics.Handler())
	// REST-only services -- no kube-apiserver equivalent, so a client that can
	// read and write the CRDs directly still has to come through this API for
	// them. Conversation content and the HITL endpoints live in the
	// per-instance runtime (design §3.6); audit is API-owned PVC state; a
	// published skill is tar content on the API-owned repository, not a Skill
	// CR.
	mux.HandleFunc("/api/v1/sessions", s.handleSessions)
	mux.HandleFunc("/api/v1/sessions/", s.handleSessionSubresource) // {key}/messages|turn[/events]|approvals[/decision]|questions[/answer|cancel]|abort, or the bare {key} itself
	mux.HandleFunc("/api/v1/audit", s.handleAudit)
	mux.HandleFunc("/api/v1/skills/{name}/publish", s.handlePublishSkill)

	// CRD facade -- the HTTP mirror of the six ai.cubestack.io CRDs, kept for
	// HTTP-only clients (e.g. the open-source reference Portal). A client with
	// kube-apiserver access may perform the same operations directly on the CRs,
	// which are Namespaced (issue #146); the data-plane contract is recorded in
	// issue #148.
	// Three of these read or write more than one CR holds: agent/config and
	// agent/approval recompute the template values the instance inherits
	// (nothing merged is stored in status), approvalView adds the live gateway
	// channel state, and llms writes AgentTemplate.spec.providers together with
	// the credential Secret it references.
	mux.HandleFunc("/api/v1/agenttemplates", s.handleAgentTemplates)
	mux.HandleFunc("/api/v1/agenttemplates/", s.handleAgentTemplateByID)
	mux.HandleFunc("/api/v1/instances", s.handleInstances)
	mux.HandleFunc("/api/v1/agent/config", s.handleAgentConfig)
	mux.HandleFunc("/api/v1/agent/approval", s.handleAgentApproval)
	mux.HandleFunc("/api/v1/agent/status", s.handleAgentStatus)
	mux.HandleFunc("/api/v1/llms", s.handleAddLLM)
	// /api/v1/llms/{name} edits or removes a provider the platform admin already
	// added; the name is immutable, so every mutation is a PUT or a DELETE on
	// an existing provider (issue #170).
	mux.HandleFunc("/api/v1/llms/{name}", s.handleLLMByName)
	mux.HandleFunc("/api/v1/skills", s.handleSkills)
	mux.HandleFunc("/api/v1/skills/{name}/install", s.handleInstallSkill)
	mux.HandleFunc("/api/v1/skills/{name}/uninstall", s.handleUninstallSkill)
	mux.HandleFunc("/api/v1/tasktemplates", s.handleTaskTemplates)
	mux.HandleFunc("/api/v1/tasks", s.handleTasks)
	mux.HandleFunc("/api/v1/tasks/", s.handleTaskByID) // {id}[/run|/toggle|/reports]
	mux.HandleFunc("/api/v1/taskruns", s.handleTaskRuns)
	mux.HandleFunc("/api/v1/taskruns/", s.handleTaskRunByID)
	mux.HandleFunc("/api/v1/kinds", s.handleKinds) // CRD schema discovery
	// Internal (cluster-only) endpoints -- the agent-side supervisor pulls
	// its resolved config and the rendered gateway config here; not exposed
	// through the Portal.
	mux.HandleFunc("/internal/agents/", s.handleInternalAgentConfig)
	mux.HandleFunc("/internal/gateway/config/{user}", s.handleInternalGatewayConfig)
	mux.HandleFunc("/internal/skills/{name}/tar", s.handleInternalSkillTar)
	// Catch-all for anything the patterns above do not match. Without it the mux
	// answers with Go's plain-text "404 page not found", which would be the one
	// response in this API that a client parsing {"error": ...} cannot read.
	// Every pattern above is more specific, so this only sees unknown paths.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeNotFound(w, "no such endpoint")
	})
	return s.logRequests(mux)
}

// handleSessionSubresource routes the per-session subresources under
// /api/v1/sessions/{key}/: the conversation itself (messages), the turn
// controls and its observation stream (turn, turn/events, abort), and the
// human-in-the-loop resources (approvals, questions). History (messages) is
// served from the live runtime session -- the runtime is the only source of
// truth for conversation content (design §3.6), so reading it requires the
// instance to be warm. A new conversation names itself: the client proposes the
// key on the first POST, which is also the moment the conversation really
// begins, so there is no create route and no server-minted key to report back.
//
// DELETE is matched before the suffixes, because no subresource under this
// prefix accepts it: they are read with GET and acted on with POST. So a DELETE
// can always mean "the session this path names", and the key is the whole
// remainder -- reserved suffix included. A key that itself ends in one of the
// suffixes below ("agent:main:a/messages") is deleted, not answered by the
// subresource of that name.
//
// For every other method the known suffixes come first, because they are what
// makes those paths subresources at all. Everything else under the prefix is the
// session itself: the bare-key route, which only DELETE acts on, so a non-DELETE
// method there gets handleSessionDelete's 405.
//
// Every suffix is a fixed trailing literal, and that is a constraint rather
// than a style: the session key is allowed to contain slashes, so the key is
// recovered by stripping a known suffix off the whole remainder. A path
// parameter in the middle (the shape "/approvals/{id}/decision" would want)
// cannot be told apart from such a key -- "agent:main:a/approvals/xyz" is a
// legal key -- so the ids these routes act on travel in the request body
// instead. See api-conventions.md §3.
//
// That fallthrough is deliberately not a refusal. The key is the whole
// remainder, so a session key containing a slash is reachable exactly as it is
// on the subresource routes ("/api/v1/sessions/a/b" is the session "a/b"), and
// a suffix this router does not know is indistinguishable from such a key. The
// cost of an unknown suffix being answered by the endpoint that names it is
// therefore paid on purpose; nothing here can tell a client's typo from a
// session somebody actually named that way.
func (s *Server) handleSessionSubresource(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodDelete:
		s.handleSessionDelete(w, r)
	case strings.HasSuffix(r.URL.Path, "/messages"):
		// GET reads the transcript; POST appends a message and answers with the
		// SSE stream of the turn that message starts.
		if r.Method == http.MethodPost {
			s.handleMessages(w, r)
			return
		}
		s.handleHistory(w, r)
	case strings.HasSuffix(r.URL.Path, "/turn/events"):
		s.handleSessionStream(w, r)
	case strings.HasSuffix(r.URL.Path, "/turn"):
		s.handleTurnStatus(w, r)
	case strings.HasSuffix(r.URL.Path, "/approvals/decision"):
		s.handleApproval(w, r)
	case strings.HasSuffix(r.URL.Path, "/approvals"):
		s.handlePendingApproval(w, r)
	case strings.HasSuffix(r.URL.Path, "/questions/answer"):
		s.handleQuestion(w, r)
	case strings.HasSuffix(r.URL.Path, "/questions/cancel"):
		s.handleQuestionCancel(w, r)
	case strings.HasSuffix(r.URL.Path, "/questions"):
		s.handlePendingQuestion(w, r)
	case strings.HasSuffix(r.URL.Path, "/abort"):
		s.handleAbort(w, r)
	default:
		s.handleSessionDelete(w, r)
	}
}

// logRequests records one line per request: method, path, status, response
// size and duration.
//
// Two kinds of traffic are skipped rather than logged at a lower level. An
// access log belongs with the component's own messages, which are always
// visible, and both of the following would bury what a human is looking for:
//
//   - Probe paths: the kubelet polls /healthz and /readyz every few seconds,
//     and Prometheus scrapes /metrics on its own interval.
//   - The /internal/ surface: the supervisor's poll loop ticks every 10s
//     (supervisor.go's PollInterval) and each tick makes two calls into this
//     surface, fetchConfig and syncCredentials -- machine traffic, never
//     human- or browser-facing, that would otherwise add up to thousands of
//     lines a day per agent Pod.
func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isProbePath(r.URL.Path) || isInternalAPIPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		next.ServeHTTP(rec, r)
		// EscapedPath, not Path: Path is percent-decoded, so an unauthenticated
		// request to /%0aFAKE_RECORD would arrive here with a real newline and
		// turn one request into two log records, the second forged. The escaped
		// form keeps every request on exactly one line.
		s.logf("%s %s %d %dB %s", r.Method, r.URL.EscapedPath(), rec.status, rec.bytes,
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

// isInternalAPIPath reports whether path is one of the supervisor-to-api
// machine routes registered under /internal/ (server.go:222-224): agent
// config, gateway credentials and skill tarball pulls. None of these are
// human- or browser-facing, so they get the same access-log skip as probes,
// for a different reason -- fixed-interval polling volume rather than
// infrastructure noise.
func isInternalAPIPath(path string) bool {
	return strings.HasPrefix(path, "/internal/")
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
	// final is set once a non-informational status has been recorded. net/http
	// ignores a second WriteHeader and logs "superfluous response.WriteHeader
	// call", so recording the latest value would report a status the client
	// never received.
	final bool
}

func (r *statusRecorder) WriteHeader(status int) {
	// 1xx is provisional, not final: 103 Early Hints precedes the real status,
	// and the handler may still send it. 101 is the exception -- it ends the
	// HTTP exchange rather than preceding anything.
	if status >= 100 && status <= 199 && status != http.StatusSwitchingProtocols {
		r.ResponseWriter.WriteHeader(status)
		return
	}
	if !r.final {
		r.status = status
		r.final = true
	}
	r.ResponseWriter.WriteHeader(status)
}

// Write needs no status bookkeeping: status starts at StatusOK, which is the
// status net/http sends for a body written without a WriteHeader call.

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

// logf is a small helper for handler-side logging.
func (s *Server) logf(format string, args ...any) {
	log.Printf(format, args...)
}
