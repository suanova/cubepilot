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
	gatewayConns *gatewayConns   // nil when the gateway channel is not configured (approvalPolicy stays declarative)
	qroutes      *questionRoutes // gateway question id -> session, for ask_user events (issue #161)
}

// hitlMasterSecretName is the Secret holding the auto-generated device master
// key (created by the API on first enable; per-user devices are derived from
// it, so it must be stable across API restarts).
const hitlMasterSecretName = "cubepilot-hitl-master"

// EnableHITL activates the human-in-the-loop approval channel (issue #20). The
// device master key is auto-generated and persisted in a Secret (load-or-create)
// so enabling needs no operator-supplied key; the API's ServiceAccount only
// needs access to that one Secret. It returns an error when the channel cannot
// be brought up: since live chat itself runs over the gateway device channel
// (issue #130), a missing master key leaves the API unable to serve turns at
// all, so the caller must treat a failure as fatal rather than start
// half-configured (issue #127).
func (s *Server) EnableHITL() error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	var mk []byte
	var sec corev1.Secret
	err := s.cr.Get(ctx, types.NamespacedName{Namespace: s.cfg.Namespace, Name: hitlMasterSecretName}, &sec)
	switch {
	case err == nil:
		// The key is stored Base64-encoded; decode so a restarted API derives
		// the same device identities as the process that created the Secret.
		encoded := sec.Data["key"]
		// Assign to the function-level mk (not `mk, derr :=`, which would shadow
		// it inside this case and leave the outer mk empty) so a restarted API
		// actually re-uses the persisted key and keeps HITL on (issue #128).
		var derr error
		mk, derr = base64.StdEncoding.DecodeString(string(encoded))
		if derr != nil || len(mk) == 0 {
			return fmt.Errorf("hitl: master Secret %s has an invalid 'key'", hitlMasterSecretName)
		}
	case apierrors.IsNotFound(err):
		mk = make([]byte, 32)
		if _, rerr := rand.Read(mk); rerr != nil {
			return fmt.Errorf("hitl: generate master key: %w", rerr)
		}
		sec = corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: hitlMasterSecretName, Namespace: s.cfg.Namespace},
			Data:       map[string][]byte{"key": []byte(base64.StdEncoding.EncodeToString(mk))},
		}
		if cerr := s.cr.Create(ctx, &sec); cerr != nil {
			if !apierrors.IsAlreadyExists(cerr) {
				return fmt.Errorf("hitl: ensure master Secret: %w", cerr)
			}
			// Lost the create race to a peer replica: adopt the winner's persisted
			// key so every replica derives the same device identities. Only a
			// successful read + decode replaces mk -- never fall through to
			// ConfigureGateway with the random bytes we just generated, which would
			// leave the API with unstable (unpersisted) device identities.
			var got corev1.Secret
			if rerr := s.cr.Get(ctx, types.NamespacedName{Namespace: s.cfg.Namespace, Name: hitlMasterSecretName}, &got); rerr != nil {
				return fmt.Errorf("hitl: read master Secret after create race: %w", rerr)
			}
			var derr error
			mk, derr = base64.StdEncoding.DecodeString(string(got.Data["key"]))
			if derr != nil || len(mk) == 0 {
				return fmt.Errorf("hitl: winner master Secret has an invalid 'key'")
			}
		}
	default:
		return fmt.Errorf("hitl: read master Secret: %w", err)
	}

	m := ConfigureGateway(s.mgr, s.cfg.GatewayToken, mk, s.logf)
	if m == nil {
		return fmt.Errorf("hitl: cannot configure the approval channel (missing manager or gateway token)")
	}
	s.gatewayConns = m
	s.approvals.SetResolver(m)
	m.bridge = func(user string, ev ws.ApprovalRequested) {
		s.approvals.Begin(user, pendingApproval{
			ApprovalID: ev.ID,
			SessionKey: ev.Request.SessionKey,
			Tool:       "exec",
			Command:    ev.Request.Command,
			Message:    ev.Request.WarningText,
			CreatedAt:  time.Now(),
		})
	}
	// An approval the gateway ended by itself drops the platform's record of it
	// and, if a view is attached, the card. Without this the record lives on in
	// the ledger that reload recovery reads, so reopening the conversation shows
	// a confirmation for an approval the gateway no longer has, and answering it
	// fails. The gateway broadcasts the resolved event for a decision made
	// anywhere (including the Portal's own, whose record Resolve has already
	// removed) and for an approval that simply expired, so this has to be
	// idempotent -- settleApproval is.
	m.approvalResolved = func(user string, ev ws.ApprovalResolved) {
		s.settleApprovalResolved(user, ev)
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
	s.logf("hitl: master key %s ensured; write confirmations enabled", hitlMasterSecretName)
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
	mux.HandleFunc("/api/v1/sessions/", s.handleSessionSubresource) // {key}/messages|approval[/pending]|question[/pending]|abort|turn
	mux.HandleFunc("/api/v1/messages", s.handleMessages)
	mux.HandleFunc("/api/v1/audit", s.handleAudit)
	mux.HandleFunc("/api/v1/skills/{name}/publish", s.handlePublishSkill)

	// CRD facade -- the HTTP mirror of the six ai.cubestack.io CRDs, kept for
	// HTTP-only clients (e.g. the open-source reference Portal). A client with
	// kube-apiserver access may perform the same operations directly on the CRs,
	// which are Namespaced (issue #146); the data-plane contract is recorded in
	// issue #148.
	// Three of these read or write more than one CR holds: agent/config and
	// agent/approval recompute the template values the instance inherits
	// (nothing merged is stored in status), approvalView adds the live HITL
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
	return logRequests(mux)
}

// handleSessionSubresource routes the per-session subresources under
// /api/v1/sessions/{key}/: the conversation itself (messages), the
// human-in-the-loop endpoints (approval, question), and the turn controls
// (abort, turn). History (messages) is served from the live runtime session --
// the runtime is the only source of truth for conversation content (design
// §3.6), so reading it requires the instance to be warm.
func (s *Server) handleSessionSubresource(w http.ResponseWriter, r *http.Request) {
	switch {
	case strings.HasSuffix(r.URL.Path, "/messages"):
		s.handleHistory(w, r)
	case strings.HasSuffix(r.URL.Path, "/approval/pending"):
		s.handlePendingApproval(w, r)
	case strings.HasSuffix(r.URL.Path, "/approval"):
		s.handleApproval(w, r)
	case strings.HasSuffix(r.URL.Path, "/question/pending"):
		s.handlePendingQuestion(w, r)
	case strings.HasSuffix(r.URL.Path, "/question"):
		s.handleQuestion(w, r)
	case strings.HasSuffix(r.URL.Path, "/abort"):
		s.handleAbort(w, r)
	case strings.HasSuffix(r.URL.Path, "/turn"):
		s.handleTurnStatus(w, r)
	default:
		writeNotFound(w, "unknown session subresource")
	}
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r)
	})
}

// logf is a small helper for handler-side logging.
func (s *Server) logf(format string, args ...any) {
	log.Printf(format, args...)
}
