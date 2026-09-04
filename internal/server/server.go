// Package server exposes the CubePilot Portal and REST/SSE API, routing chat
// turns to per-user OpenClaw instances via the Instance Manager. Instance
// state comes from AgentInstance CRs; the server also serves the platform
// objects (Agent / Skill / Model / Task / TaskRun) over the API (design
// CubePilot-Cloud-for-Agents-Simplified-Design.md §2.1). The API process is
// stateless except for the JSON metadata store (message ledger / audit /
// inspect reports / agent config) on a single RWO PVC. Task scheduling is
// owned by the operator's CRD scheduler -- the API never runs cron loops.
package server

import (
	"context"
	"crypto/rand"
	"encoding/base64"
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
	cfg       config.Config
	mgr       *instances.Manager
	store     *store.Store
	catalog   *skill.Catalog
	cr        client.Client
	hub       *Hub
	approvals *ApprovalService
	hitl      *hitlManager // nil when HITL is not configured (confirmPolicy stays declarative)
}

// hitlMasterSecretName is the Secret holding the auto-generated device master
// key (created by the API on first enable; per-user devices are derived from
// it, so it must be stable across API restarts).
const hitlMasterSecretName = "cubepilot-hitl-master"

// EnableHITL activates the human-in-the-loop approval channel (issue #20). The
// device master key is auto-generated and persisted in a Secret (load-or-create)
// so enabling needs no operator-supplied key; the API's ServiceAccount only
// needs access to that one Secret. When the Secret cannot be ensured the API
// logs and keeps the channel off (chat behavior unchanged) rather than crash.
func (s *Server) EnableHITL() {
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
		mk, derr := base64.StdEncoding.DecodeString(string(encoded))
		if derr != nil || len(mk) == 0 {
			s.logf("hitl: master Secret %s has an invalid 'key'; disabling HITL", hitlMasterSecretName)
			return
		}
	case apierrors.IsNotFound(err):
		mk = make([]byte, 32)
		if _, rerr := rand.Read(mk); rerr != nil {
			s.logf("hitl: generate master key: %v", rerr)
			return
		}
		sec = corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: hitlMasterSecretName, Namespace: s.cfg.Namespace},
			Data:       map[string][]byte{"key": []byte(base64.StdEncoding.EncodeToString(mk))},
		}
		if cerr := s.cr.Create(ctx, &sec); cerr != nil {
			// Lost a create race to a peer replica: read the winner's key.
			if apierrors.IsAlreadyExists(cerr) {
				var got corev1.Secret
				if rerr := s.cr.Get(ctx, types.NamespacedName{Namespace: s.cfg.Namespace, Name: hitlMasterSecretName}, &got); rerr == nil {
					if k := got.Data["key"]; len(k) > 0 {
						mk, _ = base64.StdEncoding.DecodeString(string(k))
					}
				}
			}
			if len(mk) == 0 {
				s.logf("hitl: ensure master Secret: %v; disabling HITL", cerr)
				return
			}
		}
	default:
		s.logf("hitl: read master Secret: %v; disabling HITL", err)
		return
	}

	m := ConfiguredHITL(s.mgr, s.cfg.GatewayToken, mk, s.logf)
	if m == nil {
		return
	}
	s.hitl = m
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
	s.logf("hitl: master key %s ensured; write confirmations enabled", hitlMasterSecretName)
}

// New builds the HTTP handler for the assistant service.
func New(cfg config.Config, mgr *instances.Manager, st *store.Store, catalog *skill.Catalog, cr client.Client) *Server {
	s := &Server{cfg: cfg, mgr: mgr, store: st, catalog: catalog, cr: cr}
	s.hub = NewHub()
	s.approvals = NewApprovalService(s.hub, st, s.logf)
	return s
}

// Handler returns the fully wired HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/metrics", metrics.Handler())
	mux.HandleFunc("/api/sessions", s.handleSessions)
	mux.HandleFunc("/api/sessions/", s.handleSessionSubresource)
	mux.HandleFunc("/api/messages", s.handleMessages)
	mux.HandleFunc("/api/inspect", s.handleInspect)
	mux.HandleFunc("/api/tasks", s.handleTasks)
	mux.HandleFunc("/api/tasks/", s.handleTaskByID)
	mux.HandleFunc("/api/audit", s.handleAudit)
	mux.HandleFunc("/api/agent/config", s.handleAgentConfig)
	mux.HandleFunc("/api/agent/status", s.handleAgentStatus)
	mux.HandleFunc("/api/agenttemplates", s.handleAgentTemplates)
	mux.HandleFunc("/api/agenttemplates/", s.handleAgentTemplateByID)
	mux.HandleFunc("/api/instances", s.handleInstances)
	mux.HandleFunc("/api/llms", s.handleAddLLM)
	mux.HandleFunc("/api/skills", s.handleSkills)
	mux.HandleFunc("/api/skills/{name}/publish", s.handlePublishSkill)
	mux.HandleFunc("/api/skills/{name}/install", s.handleInstallSkill)
	mux.HandleFunc("/api/skills/{name}/uninstall", s.handleUninstallSkill)
	mux.HandleFunc("/api/tasktemplates", s.handleTaskTemplates)
	mux.HandleFunc("/api/taskruns", s.handleTaskRuns)
	mux.HandleFunc("/api/taskruns/", s.handleTaskRunByID)
	mux.HandleFunc("/api/kinds", s.handleKinds)
	// Internal (cluster-only) endpoints -- the agent-side supervisor pulls
	// its resolved config and the rendered gateway config here; not exposed
	// through the Portal.
	mux.HandleFunc("/internal/agents/", s.handleInternalAgentConfig)
	mux.HandleFunc("/internal/gateway/config/{user}", s.handleInternalGatewayConfig)
	mux.HandleFunc("/internal/skills/{name}/tar", s.handleInternalSkillTar)
	return logRequests(mux)
}

// handleSessionSubresource routes /api/sessions/{key}/{messages|ledger|seed|confirm}.
func (s *Server) handleSessionSubresource(w http.ResponseWriter, r *http.Request) {
	switch {
	case strings.HasSuffix(r.URL.Path, "/messages"):
		s.handleHistory(w, r)
	case strings.HasSuffix(r.URL.Path, "/ledger"):
		s.handleLedger(w, r)
	case strings.HasSuffix(r.URL.Path, "/seed"):
		s.handleSeed(w, r)
	case strings.HasSuffix(r.URL.Path, "/confirm/pending"):
		s.handlePendingConfirm(w, r)
	case strings.HasSuffix(r.URL.Path, "/confirm"):
		s.handleConfirm(w, r)
	default:
		http.NotFound(w, r)
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
