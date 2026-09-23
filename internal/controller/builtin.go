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
	"github.com/suanova/cubepilot/internal/gateway"
	"github.com/suanova/cubepilot/internal/k8s"
	"github.com/suanova/cubepilot/internal/skill"
)

// BuiltinAgentName is the preset platform agent (design §5.1: cubepilot
// is the first platform-preset Agent, auto-instantiated per user, non-deletable).
const BuiltinAgentName = "cubepilot"

// BuiltinTaskTemplateName is the preset nightly inspection template. It is the
// one preset that carries a schedule of its own (design §3.3.2).
const BuiltinTaskTemplateName = "daily-inspection"

// BuiltinProviderName is the provider key of the platform's own LLM. It is a
// name, not a model id: the platform default is one provider serving one model,
// and an admin can add more model ids to it or add providers beside it.
const BuiltinProviderName = "platform"

// Per-user identity ClusterRoles the platform binds each user's ServiceAccount
// to (issue #19), all declared in the chart rbac.yaml:
//
//   - `view`, the built-in read-only ClusterRole. It is namespaced by design and
//     deliberately excludes secrets, which is why it is safe to bind widely --
//     and also why it is not enough on its own: it grants no cluster-scoped
//     resource, so nothing node-level is readable through it.
//   - cubepilot-cluster-read, the cluster-scoped read `view` does not grant
//     (nodes and the rest), with secrets and the kubelet proxies excluded.
//   - cubepilot-user-crds, full ai.cubestack.io CRUD cluster-wide.
//
// The assistant executes kubectl with the user's identity and must operate
// platform CRs in any namespace (generic CRD discovery creates e.g. CubeStack
// DevEnvironments in arbitrary namespaces, not only the install namespace), so
// even though the six platform CRDs are Namespaced (issue #146) the per-user
// bindings stay ClusterRoleBindings. ClusterRoleBindings reference these roles
// by name, so the operator needs only get/bind on them -- and the chart lists
// every name here in that role's resourceNames.
const (
	UserViewClusterRole        = "view"
	UserClusterReadClusterRole = "cubepilot-cluster-read"
	UserCRDsClusterRole        = "cubepilot-user-crds"
)

// userClusterRoles is the set bound per user, in binding order. One list, so a
// role added here is bound, covered by the bootstrap test, and (once its name
// is in the chart's resourceNames) bindable by the operator.
var userClusterRoles = []string{
	UserViewClusterRole,
	UserClusterReadClusterRole,
	UserCRDsClusterRole,
}

// userCRBName builds the per-user ClusterRoleBinding name. The role segment is
// shortened where the full name is redundant next to the "cubepilot-user-"
// prefix, so binding names stay readable.
func userCRBName(user, role string) string {
	short := role
	switch role {
	case UserCRDsClusterRole:
		short = "crds"
	case UserClusterReadClusterRole:
		short = "cluster-read"
	}
	return "cubepilot-user-" + short + "-" + k8s.Sanitize(user) + "-" + k8s.UserIdentityHash(user)
}

// BuiltinSkills are the preset domain skills the builtin agent references
// (design §3.3.1 domain layer): the agent gets exactly the skills the platform
// ships. The content lives with the skill package; the API seeds the Skill
// CRDs at startup.
var BuiltinSkills = skill.BuiltinSkillNames()

// BuiltinProviders returns the preset provider for the builtin AgentTemplate
// (design §3.3: models are inlined in the template -- no standalone Model CRD).
// The platform default provider references the cubepilot-llm credential Secret
// created by setup.sh; its endpoint and model name come from the operator
// config (config.LLMEndpoint / config.LLMModel) and can be edited on the CR
// after install.
func BuiltinProviders(endpoint, modelName string) []v1alpha1.TemplateProviderSpec {
	return []v1alpha1.TemplateProviderSpec{
		{
			Name:          BuiltinProviderName,
			Endpoint:      endpoint,
			CredentialRef: &corev1.LocalObjectReference{Name: "cubepilot-llm"},
			Models:        []string{modelName},
		},
	}
}

// BuiltinAgentTemplate returns the builtin cubepilot template
// (design §3.1), with the platform default model at the given endpoint and
// model name.
func BuiltinAgentTemplate(endpoint, modelName string) *v1alpha1.AgentTemplate {
	return &v1alpha1.AgentTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Name: BuiltinAgentName,
			Labels: map[string]string{
				"app.kubernetes.io/part-of": "cubepilot",
				"cubepilot/builtin":         "true",
			},
		},
		Spec: v1alpha1.AgentTemplateSpec{
			DisplayName:    "Platform Management Assistant",
			Description:    "Default assistant for managing the CubeStack platform (ChatOps + inspection + reporting)",
			Runtime:        v1alpha1.RuntimeOpenClaw,
			DefaultModel:   gateway.ModelKey(BuiltinProviderName, modelName),
			Providers:      BuiltinProviders(endpoint, modelName),
			ApprovalPolicy: v1alpha1.ApprovalPolicyAllowlist,
			Instructions: "You are the intelligent assistant of the CubeStack platform (CubePilot)." +
				"Use kubectl to query and operate cluster resources; run read-only operations directly, " +
				"and state the action and its blast radius before running write operations. Inspection and reporting use structured output.",
			Skills: BuiltinSkills,
		},
	}
}

// BuiltinBootstrapReconciler reconciles the builtin platform objects: the
// cubepilot Agent definition, the preset Skills and the preset TaskTemplates.
// It also instantiates the builtin agent for
// every configured user (auto-instantiated per user, design §3.1 / §5.3),
// reconciling the
// AgentInstance CRs on every tick (idempotent: already-present objects are
// left untouched; missing ones are created).
type BuiltinBootstrapReconciler struct {
	client.Client
	// APIReader reads straight from the API server, past the manager's cache.
	// Writing the per-user kubeconfig Secret needs it: see ensureSecretData.
	APIReader client.Reader
	Scheme    *runtime.Scheme
	Cfg       config.Config
}

// +kubebuilder:rbac:groups=ai.cubestack.io,resources=agenttemplates;skills;tasktemplates;agentinstances;tasks;taskruns,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=ai.cubestack.io,resources=agentinstances/status;skills/status;tasks/status;taskruns/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=serviceaccounts;secrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterrolebindings,verbs=get;list;watch;create;update;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterroles,verbs=get;list;watch;bind,resourceNames=view;cubepilot-cluster-read;cubepilot-user-crds

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
	// 1. AgentTemplate definition (with inline providers, design §3.1/§3.3).
	// The platform default provider is included only when an endpoint AND model
	// name are configured (CUBEPILOT_LLM_ENDPOINT / CUBEPILOT_LLM_MODEL); an
	// empty pair ships the template provider-less and LLMs are added from the
	// Portal (Agent Config -> LLM Config). "有就是有，没有就是没有": the provider
	// list is fixed at template creation and never re-synced afterwards.
	agent := BuiltinAgentTemplate(r.Cfg.LLMEndpoint, r.Cfg.LLMModel)
	if r.Cfg.LLMEndpoint == "" || r.Cfg.LLMModel == "" {
		agent.Spec.Providers = nil
		agent.Spec.DefaultModel = ""
	}
	agent.Namespace = r.Cfg.Namespace
	if err := r.createIfMissing(ctx, agent); err != nil {
		return err
	}
	// 2. Task templates: the preset catalog, created where missing. One that is
	// already present is left exactly as it is, so an operator's edit to a
	// preset is never overwritten -- a preset deleted on purpose does come back
	// on the next tick, which is the create-if-missing contract. A parse failure
	// here is a shipped-bug, not a cluster condition, so it stops the whole
	// bootstrap and shows up in the log rather than seeding a partial catalog.
	presets, err := BuiltinTaskTemplates()
	if err != nil {
		return err
	}
	for _, taskTemplate := range presets {
		taskTemplate.Namespace = r.Cfg.Namespace
		if err := r.createIfMissing(ctx, taskTemplate); err != nil {
			return err
		}
	}
	// 3. Per-user builtin agent instances (auto-instantiated per user;
	// resident). Reject identities that would collide on the derived per-user
	// name before provisioning (e.g. "zhang.wei" vs "Zhang Wei").
	seenIdentity := map[string]string{}
	for _, user := range r.Cfg.Users {
		saName := k8s.UserServiceAccountName(user)
		if prev, dup := seenIdentity[saName]; dup {
			return fmt.Errorf("users %q and %q collide on identity %s", prev, user, saName)
		}
		seenIdentity[saName] = user
	}
	for _, user := range r.Cfg.Users {
		// Platform-generated per-user identity first: SA + view/CRD ClusterRole
		// bindings + a kubeconfig Secret the agent mounts as its default
		// credentials (issue #19). Zero operator/admin-supplied kubeconfig. The
		// identity must exist before the AgentInstance is created: the
		// AgentInstance controller requires the per-user kubeconfig Secret
		// before it creates the Pod (issue #100 -- no placeholder-identity Pod),
		// so creating the instance first would leave it waiting on an identity
		// this pass only mints afterwards.
		if err := r.ensurePerUserKubeconfigAccess(ctx, user); err != nil {
			return err
		}
		inst := &v1alpha1.AgentInstance{
			ObjectMeta: metav1.ObjectMeta{
				Name:      InstanceNameFor(user, BuiltinAgentName),
				Namespace: r.Cfg.Namespace,
				Labels:    map[string]string{"cubepilot/builtin": "true"},
			},
			Spec: v1alpha1.AgentInstanceSpec{
				TemplateRef: BuiltinAgentName,
				Owner:       user,
			},
		}
		if err := r.createIfMissing(ctx, inst); err != nil {
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

	for _, role := range userClusterRoles {
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

	// Kubeconfig Secret consumed by the AgentInstance controller (PR #94). It is
	// rewritten when the token it was minted from changed, so a recreated token
	// Secret refreshes the kubeconfig instead of leaving a stale one behind.
	kc := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: k8s.UserKubeconfigSecretFor(user), Namespace: r.Cfg.Namespace, Labels: builtinLabels},
		Data:       map[string][]byte{"config": k8s.PerUserKubeconfigYAML(token)},
	}
	if err := ensureSecretData(ctx, r.Client, r.APIReader, kc); err != nil {
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
	log.Printf("bootstrap: created %s/%s", r.kindOf(obj), obj.GetName())
	return nil
}

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
