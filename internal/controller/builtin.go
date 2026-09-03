package controller

import (
	"context"
	"fmt"
	"log"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
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
	"github.com/suanova/cubepilot/internal/skill"
)

// BuiltinAgentName is the preset platform agent (design §5.1: agent-for-cloud
// is the first platform-preset Agent, auto-instantiated per user, non-deletable).
const BuiltinAgentName = "agent-for-cloud"

// BuiltinTaskTemplateName is the preset inspection task template
// (design §3.3.2 the preset inspection template daily-inspection).
const BuiltinTaskTemplateName = "daily-inspection"

// Per-user identity ClusterRoles the platform binds each user's ServiceAccount
// to (issue #19): `view` is the built-in read-only ClusterRole (deliberately
// excludes secrets); cubepilot-user-crds (declared in the chart rbac.yaml)
// grants full ai.cubestack.io CRUD. ClusterRoleBindings reference these by
// name, so the operator needs only get/bind on them.
const (
	UserViewClusterRole = "view"
	UserCRDsClusterRole = "cubepilot-user-crds"
)

// userCRBName builds the per-user ClusterRoleBinding name.
func userCRBName(user, role string) string {
	short := role
	if role == UserCRDsClusterRole {
		short = "crds"
	}
	return "cubepilot-user-" + short + "-" + k8s.Sanitize(user)
}

// BuiltinSkills are the preset domain skills the builtin agent references
// (design §3.3.1 domain layer): the agent gets exactly the skills the platform
// ships. The content lives with the skill package; the API seeds the Skill
// CRDs at startup.
var BuiltinSkills = skill.BuiltinSkillNames()

// BuiltinModels returns the preset inline model entries for the builtin
// AgentTemplate (design §3.3: models are inlined in the template -- no
// standalone Model CRD). The platform default model references the
// cubepilot-llm credential Secret created by setup.sh; its endpoint and model
// name come from the operator config (config.LLMEndpoint / config.LLMModel)
// and can be edited on the CR after install.
func BuiltinModels(endpoint, modelName string) []v1alpha1.TemplateModelSpec {
	return []v1alpha1.TemplateModelSpec{
		{
			Name:          modelName,
			Endpoint:      endpoint,
			CredentialRef: &corev1.LocalObjectReference{Name: "cubepilot-llm"},
		},
	}
}

// BuiltinAgentTemplate returns the builtin agent-for-cloud template
// (design §3.1), with the platform default model at the given endpoint and
// model name.
func BuiltinAgentTemplate(endpoint, modelName string) *v1alpha1.AgentTemplate {
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
			DefaultModel:  modelName,
			Models:        BuiltinModels(endpoint, modelName),
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
			Skills:      []string{"cluster-inspection"},
			DefaultCron: "0 2 * * *",
			//nolint:staticcheck // TaskTemplateDefaults is deprecated (kept for compatibility, see TaskTemplateSpec.DefaultCron).
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
// +kubebuilder:rbac:groups="",resources=serviceaccounts;secrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterrolebindings,verbs=get;list;watch;create;update;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterroles,verbs=get;list;watch;bind,resourceNames=view;cubepilot-user-crds

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
	modelName := r.Cfg.LLMModel
	if modelName == "" {
		modelName = config.DefaultLLMModel
	}
	if err := r.createIfMissing(ctx, BuiltinAgentTemplate(endpoint, modelName)); err != nil {
		return err
	}
	// 2. Task template.
	if err := r.createIfMissing(ctx, BuiltinTaskTemplate()); err != nil {
		return err
	}
	// 3. Per-user builtin agent instances (auto-instantiated per user;
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
		// Platform-generated per-user identity: SA + view/CRD ClusterRole
		// bindings + a kubeconfig Secret the agent mounts as its default
		// credentials (issue #19). Zero operator/admin-supplied kubeconfig.
		if err := r.ensurePerUserKubeconfigAccess(ctx, user); err != nil {
			return err
		}
	}
	return nil
}

// ensurePerUserKubeconfigAccess mints the per-user identity (issue #19): a
// namespaced ServiceAccount, ClusterRoleBindings to `view` and
// `cubepilot-user-crds`, and a kubeconfig Secret (SA token inlined) under
// k8s.UserKubeconfigSecretFor so the AgentInstance controller's existing
// dual-kubeconfig mount picks it up unchanged. Idempotent; when the token
// Secret's token is not yet populated (API server fills it asynchronously) it
// returns an error so the reconcile requeues rather than writing a broken
// kubeconfig.
func (r *BuiltinBootstrapReconciler) ensurePerUserKubeconfigAccess(ctx context.Context, user string) error {
	saName := k8s.UserServiceAccountName(user)
	builtinLabels := map[string]string{"cubepilot/builtin": "true"}
	sa := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Name: saName, Namespace: r.Cfg.Namespace, Labels: builtinLabels},
	}
	if err := r.createIfMissing(ctx, sa); err != nil {
		return err
	}

	for _, role := range []string{UserViewClusterRole, UserCRDsClusterRole} {
		crb := &rbacv1.ClusterRoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: userCRBName(user, role), Labels: builtinLabels},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: role},
			Subjects: []rbacv1.Subject{{
				Kind:      "ServiceAccount",
				Name:      saName,
				Namespace: r.Cfg.Namespace,
			}},
		}
		if err := r.createIfMissing(ctx, crb); err != nil {
			return err
		}
	}

	// Token: a legacy service-account-token Secret (the API server writes the
	// token back asynchronously). Read the token; if not yet present, requeue.
	tokenSecretName := saName + "-token"
	tok := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:        tokenSecretName,
			Namespace:   r.Cfg.Namespace,
			Labels:      builtinLabels,
			Annotations: map[string]string{corev1.ServiceAccountNameKey: saName},
		},
		Type: corev1.SecretTypeServiceAccountToken,
	}
	if err := r.createIfMissing(ctx, tok); err != nil {
		return err
	}
	var got corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Namespace: r.Cfg.Namespace, Name: tokenSecretName}, &got); err != nil {
		return err
	}
	token := string(got.Data[corev1.ServiceAccountTokenKey])
	if token == "" {
		return fmt.Errorf("per-user token for %s not ready yet (token secret %s)", user, tokenSecretName)
	}

	// Kubeconfig Secret consumed by the AgentInstance controller (PR #94).
	kc := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: k8s.UserKubeconfigSecretFor(user), Namespace: r.Cfg.Namespace, Labels: builtinLabels},
		Data:       map[string][]byte{"config": k8s.PerUserKubeconfigYAML(token)},
	}
	if err := r.createIfMissing(ctx, kc); err != nil {
		return err
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
