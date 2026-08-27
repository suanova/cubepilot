package controller

import (
	"context"
	"fmt"
	"log"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/suanova/cubepilot/internal/api/v1alpha1"
	"github.com/suanova/cubepilot/internal/config"
	"github.com/suanova/cubepilot/internal/k8s"
)

// BuiltinAgentName is the preset platform agent (design §5.1: agent-for-cloud
// is the first platform-preset Agent, auto-instantiated per user, non-deletable).
const BuiltinAgentName = "agent-for-cloud"

// BuiltinTaskTemplateName is the preset inspection task template
// (design §3.3.2 the preset inspection template daily-inspection).
const BuiltinTaskTemplateName = "daily-inspection"

// BuiltinSkills are the preset domain skills the builtin agent
// references (design §3.3.1 domain layer). Generated from the embedded
// SKILL.md files: the agent gets exactly the skills the platform ships.
var BuiltinSkills = func() []string {
	names, err := presetSkillNames()
	if err != nil {
		return []string{"cluster-inspection"}
	}
	return names
}()

// BuiltinModels returns the preset inline model entries for the builtin
// AgentTemplate (design §3.3: models are inlined in the template -- no
// standalone Model CRD). The platform default model references the
// cubepilot-llm credential Secret created by setup.sh; its endpoint is the
// configured platform default LLM endpoint (config.LLMEndpoint) and can be
// edited on the CR after install.
func BuiltinModels(endpoint string) []v1alpha1.TemplateModelSpec {
	return []v1alpha1.TemplateModelSpec{
		{
			Name:          "deepseek-v4-flash",
			Endpoint:      endpoint,
			CredentialRef: corev1.LocalObjectReference{Name: "cubepilot-llm"},
		},
	}
}

// BuiltinAgentTemplate returns the builtin agent-for-cloud template
// (design §3.1), with the platform default model at the given endpoint.
func BuiltinAgentTemplate(endpoint string) *v1alpha1.AgentTemplate {
	builtin := true
	return &v1alpha1.AgentTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Name: BuiltinAgentName,
			Labels: map[string]string{
				"app.kubernetes.io/part-of": "cubepilot",
				"cubepilot/builtin":         "true",
			},
		},
		Spec: v1alpha1.AgentTemplateSpec{
			DisplayName:   "Platform Management Assistant",
			Description:   "Default assistant for managing the CubeStack platform (ChatOps + inspection + reporting)",
			Runtime:       v1alpha1.RuntimeOpenClaw,
			DefaultModel:  "deepseek-v4-flash",
			Models:        BuiltinModels(endpoint),
			ConfirmPolicy: v1alpha1.ConfirmPolicyConfirmWrites,
			Instructions: "You are the intelligent assistant of the CubeStack platform (agent-for-cloud)." +
				"Use kubectl to query and operate cluster resources; run read-only operations directly, " +
				"and state the action and its blast radius before running write operations. Inspection and reporting use structured output.",
			Skills: BuiltinSkills,
			Memory: &v1alpha1.MemorySpec{Enabled: true},
			Identity: &v1alpha1.AgentIdentitySpec{
				Mode:  v1alpha1.IdentityModeUser,
				Scope: "project-write",
			},
			Registry: &v1alpha1.AgentRegistrySpec{
				Builtin:    builtin,
				Visibility: "system",
			},
			Quotas: &v1alpha1.QuotaSpec{MaxInstancesPerUser: 1},
		},
	}
}

// BuiltinSkillDefinitions is generated from the embedded SKILL.md files
// (see skill_source.go) -- one source of truth for preset domain skills.

// BuiltinTaskTemplate returns the preset daily-inspection template
// (design §3.3.2).
func BuiltinTaskTemplate() *v1alpha1.TaskTemplate {
	return &v1alpha1.TaskTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Name: BuiltinTaskTemplateName,
			Labels: map[string]string{
				"app.kubernetes.io/part-of": "cubepilot",
				"cubepilot/builtin":         "true",
			},
		},
		Spec: v1alpha1.TaskTemplateSpec{
			DisplayName: "Daily Cluster Inspection",
			Description: "Preset inspection: nodes / Pods / storage / platform components + AI smart inspection",
			Instruction: `Inspect the cluster in read-only mode (get/list/watch/logs):
1. Check node Ready status and pressure (Disk / Mem / PID)
2. Check GPU health and utilization (nvidia.com/gpu)
3. Check abnormal Pods (CrashLoopBackOff / Pending / ImagePullBackOff / OOM)
4. Check storage (PVC usage)
5. Check platform component health (Harbor / Keycloak / Prometheus)
Attach an evidence chain to any finding, classify by P0/P1/P2; no write operations allowed.`,
			ParamsSchema: []v1alpha1.ParamSchema{
				{Name: "scope", Type: "string", Default: "all", Enum: []string{"all", "node-pool", "project"}},
			},
			RequiredPermissions: &v1alpha1.RequiredPermissions{
				Level: "cluster-read",
				Note:  "Full-cluster inspection requires the creator to have cluster-level read permission",
			},
			Skills: []string{"cluster-inspection"},
			DefaultCron:  "0 2 * * *",
			Defaults: &v1alpha1.TaskTemplateDefaults{
				Trigger: v1alpha1.TaskTriggerCron,
				Cron:    "0 2 * * *",
			},
		},
	}
}

// BuiltinBootstrapReconciler reconciles the builtin platform objects: the
// agent-for-cloud Agent definition, the preset Skills and the
// daily-inspection TaskTemplate. It also instantiates the builtin agent for
// every configured user (auto-instantiated per user, design §3.1 / §5.3),
// reconciling the
// AgentInstance CRs on every tick (idempotent: already-present objects are
// left untouched; missing ones are created).
type BuiltinBootstrapReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Cfg    config.Config
}

// +kubebuilder:rbac:groups=ai.cubestack.io,resources=skills;tasktemplates;agentinstances;tasks;taskruns,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=ai.cubestack.io,resources=agentinstances/status;skills/status;tasks/status;taskruns/status,verbs=get;update;patch

// Reconcile ensures the builtin objects exist (create-if-missing).
func (r *BuiltinBootstrapReconciler) Reconcile(ctx context.Context, req reconcile.Request) (ctrl.Result, error) {
	if err := r.ensureBuiltin(ctx); err != nil {
		log.Printf("bootstrap: %v", err)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
	return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
}

// Ensure runs the full bootstrap once (called at startup).
func (r *BuiltinBootstrapReconciler) Ensure(ctx context.Context) error {
	return r.ensureBuiltin(ctx)
}

func (r *BuiltinBootstrapReconciler) ensureBuiltin(ctx context.Context) error {
	// 1. AgentTemplate definition (with inline models, design §3.1/§3.3).
	endpoint := r.Cfg.LLMEndpoint
	if endpoint == "" {
		endpoint = config.DefaultLLMEndpoint
	}
	if err := r.createIfMissing(ctx, BuiltinAgentTemplate(endpoint)); err != nil {
		return err
	}
	// 2. Domain skills (generated from embedded SKILL.md).
	skills, err := BuiltinSkillDefinitions()
	if err != nil {
		return fmt.Errorf("skill definitions: %w", err)
	}
	for _, skill := range skills {
		if err := r.createIfMissing(ctx, skill); err != nil {
			return err
		}
	}
	// 3. Task template.
	if err := r.createIfMissing(ctx, BuiltinTaskTemplate()); err != nil {
		return err
	}
	// 4. Per-user builtin agent instances (auto-instantiated per user;
	// resident).
	for _, user := range r.Cfg.Users {
		inst := &v1alpha1.AgentInstance{
			ObjectMeta: metav1.ObjectMeta{
				Name:   InstanceNameFor(user, BuiltinAgentName),
				Labels: map[string]string{"cubepilot/builtin": "true"},
			},
			Spec: v1alpha1.AgentInstanceSpec{
				TemplateRef: BuiltinAgentName,
				Owner:       user,
				Identity: v1alpha1.IdentitySpec{
					Mode: v1alpha1.IdentityModeUser,
					PrincipalRef: v1alpha1.PrincipalRef{
						UserRef: user,
					},
				},
				Lifecycle: &v1alpha1.LifecycleSpec{Strategy: "resident"},
			},
		}
		if err := r.createIfMissing(ctx, inst); err != nil {
			return err
		}
	}
	return nil
}

func (r *BuiltinBootstrapReconciler) createIfMissing(ctx context.Context, obj client.Object) error {
	key := types.NamespacedName{Name: obj.GetName()}
	if obj.GetNamespace() != "" {
		key.Namespace = obj.GetNamespace()
	}
	if err := r.Get(ctx, key, obj.DeepCopyObject().(client.Object)); err == nil {
		return nil // already present -- leave untouched (idempotent)
	} else if !apierrors.IsNotFound(err) {
		return err
	}
	if err := r.Create(ctx, obj); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return nil
		}
		return err
	}
	log.Printf("bootstrap: created %s/%s", obj.GetObjectKind().GroupVersionKind().Kind, obj.GetName())
	return nil
}

// InstanceNameFor builds the AgentInstance name for (user, agent) -- the
// instance key is user + agent (design §3.2). Both segments are DNS-1123
// sanitized (consistent with the k8s package resource naming).
func InstanceNameFor(user, agent string) string {
	return k8s.Sanitize(user) + "-" + k8s.Sanitize(agent)
}

// SetupWithManager registers the bootstrap reconciler.
func (r *BuiltinBootstrapReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("builtin-bootstrap").
		For(&v1alpha1.AgentTemplate{}).
		Complete(r)
}
