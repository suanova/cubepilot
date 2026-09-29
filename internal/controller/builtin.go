package controller

import (
	"context"
	"errors"
	"fmt"
	"log"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
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
//
// The namespace is in the name because a binding grants nothing outside the
// subject's namespace; an owner reference cannot do the job, since a
// cluster-scoped dependent needs a cluster-scoped owner.
func userCRBName(namespace, user, role string) string {
	short := role
	switch role {
	case UserCRDsClusterRole:
		short = "crds"
	case UserClusterReadClusterRole:
		short = "cluster-read"
	}
	return "cubepilot-user-" + short + "-" + k8s.Sanitize(namespace) + "-" + k8s.Sanitize(user) + "-" + k8s.UserIdentityHash(user)
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
				builtinLabel:                builtinLabelValue,
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
	// 3. Per-user identity, for the owners of the instances that exist. An
	// instance is the whole authorization: whoever can create one gets their
	// assistant an identity, and privileges do not outlive the last one.
	users, err := r.usersWithInstances(ctx)
	if err != nil {
		return err
	}
	var minted error
	for _, user := range users {
		// One user's failure must not hold back step 4: revocation is the half
		// that cannot wait for the next pass.
		minted = errors.Join(minted, r.ensurePerUserKubeconfigAccess(ctx, user))
	}
	// 4. Revoke what no instance speaks for any more, so a removed user or a
	// deleted last instance takes the identity with it.
	return errors.Join(minted, r.prunePerUserIdentity(ctx, users))
}

// userIdentityObjects names the per-user identity: what
// ensurePerUserKubeconfigAccess mints and what revoking it deletes. One list, so
// an object cannot be minted without also being revoked.
func userIdentityObjects(namespace, user string) []client.Object {
	saName := k8s.UserServiceAccountName(user)
	objs := []client.Object{
		&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: saName, Namespace: namespace}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: saName + "-token", Namespace: namespace}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: k8s.UserKubeconfigSecretFor(user), Namespace: namespace}},
	}
	for _, role := range userClusterRoles {
		objs = append(objs, &rbacv1.ClusterRoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: userCRBName(namespace, user, role)},
		})
	}
	return objs
}

// usersWithInstances returns the owners of the existing instances, sorted. The
// instance is the authorization: the platform creates none, so a user has
// credentials exactly while someone has asked for their assistant.
func (r *BuiltinBootstrapReconciler) usersWithInstances(ctx context.Context) ([]string, error) {
	var instances v1alpha1.AgentInstanceList
	if err := r.List(ctx, &instances, client.InNamespace(r.Cfg.Namespace)); err != nil {
		return nil, fmt.Errorf("list agent instances: %w", err)
	}
	owners := map[string]bool{}
	for i := range instances.Items {
		inst := &instances.Items[i]
		// A terminating instance does not speak for its owner any more: its
		// finalizer is revoking the identity, and minting it here would undo
		// exactly what a teardown waits for.
		if !inst.DeletionTimestamp.IsZero() {
			continue
		}
		if inst.Spec.Owner != "" {
			owners[inst.Spec.Owner] = true
		}
	}
	out := make([]string, 0, len(owners))
	for owner := range owners {
		out = append(out, owner)
	}
	slices.Sort(out)
	return out, nil
}

// prunePerUserIdentity removes the identity objects whose names the desired set
// no longer computes -- matched by name, since the names are hashed. Instances
// and their data PVCs are left alone: dropping data is an explicit step.
func (r *BuiltinBootstrapReconciler) prunePerUserIdentity(ctx context.Context, users []string) error {
	wantSA := map[string]bool{}
	wantSecret := map[string]bool{}
	wantCRB := map[string]bool{}
	for _, user := range users {
		for _, obj := range userIdentityObjects(r.Cfg.Namespace, user) {
			switch obj.(type) {
			case *corev1.ServiceAccount:
				wantSA[obj.GetName()] = true
			case *corev1.Secret:
				wantSecret[obj.GetName()] = true
			default:
				wantCRB[obj.GetName()] = true
			}
		}
	}

	var sas corev1.ServiceAccountList
	if err := r.List(ctx, &sas, client.InNamespace(r.Cfg.Namespace), client.MatchingLabels{builtinLabel: builtinLabelValue}); err != nil {
		return fmt.Errorf("list per-user serviceaccounts: %w", err)
	}
	for i := range sas.Items {
		if !wantSA[sas.Items[i].Name] {
			if err := r.deleteAndLog(ctx, &sas.Items[i], "ServiceAccount"); err != nil {
				return err
			}
		}
	}

	var secrets corev1.SecretList
	if err := r.List(ctx, &secrets, client.InNamespace(r.Cfg.Namespace), client.MatchingLabels{builtinLabel: builtinLabelValue}); err != nil {
		return fmt.Errorf("list per-user secrets: %w", err)
	}
	for i := range secrets.Items {
		if !wantSecret[secrets.Items[i].Name] {
			if err := r.deleteAndLog(ctx, &secrets.Items[i], "Secret"); err != nil {
				return err
			}
		}
	}

	// Cluster-scoped, so the label is the only thing that ties a binding to this
	// platform; a binding whose name is not in the desired set is either a
	// leftover from another namespace or from a previous user list.
	var crbs rbacv1.ClusterRoleBindingList
	if err := r.List(ctx, &crbs, client.MatchingLabels{builtinLabel: builtinLabelValue}); err != nil {
		return fmt.Errorf("list per-user clusterrolebindings: %w", err)
	}
	for i := range crbs.Items {
		if !wantCRB[crbs.Items[i].Name] {
			if err := r.deleteAndLog(ctx, &crbs.Items[i], "ClusterRoleBinding"); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *BuiltinBootstrapReconciler) deleteAndLog(ctx context.Context, obj client.Object, kind string) error {
	if err := r.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete %s %s: %w", kind, obj.GetName(), err)
	}
	log.Printf("bootstrap: revoked %s/%s (no longer in the desired user set)", kind, obj.GetName())
	return nil
}

// builtinLabel marks every object the platform mints at runtime, so a
// reconciler can tell its own from an admin's (the prune selects on it).
const (
	builtinLabel      = "cubepilot/builtin"
	builtinLabelValue = "true"
)

// builtinLabels returns a fresh label set: the objects travel into the API
// client, and one shared map is a mutation away from a surprising bug.
func builtinLabels() map[string]string {
	return map[string]string{builtinLabel: builtinLabelValue}
}

// ensurePerUserKubeconfigAccess mints the per-user identity (issue #19): SA,
// bindings and kubeconfig Secret. A token the API server has not filled in yet
// returns an error, so the reconcile requeues rather than write nonsense.
func (r *BuiltinBootstrapReconciler) ensurePerUserKubeconfigAccess(ctx context.Context, user string) error {
	saName := k8s.UserServiceAccountName(user)
	sa := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Name: saName, Namespace: r.Cfg.Namespace, Labels: builtinLabels()},
	}
	if err := r.createIfMissing(ctx, sa); err != nil {
		return err
	}

	for _, role := range userClusterRoles {
		crb := &rbacv1.ClusterRoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: userCRBName(r.Cfg.Namespace, user, role), Labels: builtinLabels()},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: role},
			Subjects: []rbacv1.Subject{{
				Kind:      "ServiceAccount",
				Name:      saName,
				Namespace: r.Cfg.Namespace,
			}},
		}
		if err := r.reconcilePerUserCRB(ctx, crb); err != nil {
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
			Labels:      builtinLabels(),
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
		ObjectMeta: metav1.ObjectMeta{Name: k8s.UserKubeconfigSecretFor(user), Namespace: r.Cfg.Namespace, Labels: builtinLabels()},
		Data:       map[string][]byte{"config": k8s.PerUserKubeconfigYAML(token)},
	}
	if err := ensureSecretData(ctx, r.Client, r.APIReader, kc); err != nil {
		return err
	}
	return nil
}

// reconcilePerUserCRB creates the per-user binding, or brings roleRef and
// subjects back in line: trusting one that exists would make a wrong binding
// permanent. A wrong roleRef is replaced, not updated -- roleRef is immutable.
func (r *BuiltinBootstrapReconciler) reconcilePerUserCRB(ctx context.Context, want *rbacv1.ClusterRoleBinding) error {
	var have rbacv1.ClusterRoleBinding
	err := r.Get(ctx, client.ObjectKeyFromObject(want), &have)
	if apierrors.IsNotFound(err) {
		if err := r.Create(ctx, want); err != nil && !apierrors.IsAlreadyExists(err) {
			return err
		}
		log.Printf("bootstrap: created ClusterRoleBinding/%s", want.Name)
		return nil
	}
	if err != nil {
		return err
	}
	if !equality.Semantic.DeepEqual(have.RoleRef, want.RoleRef) {
		replacement := want.DeepCopy()
		replacement.Labels = mergeLabels(have.Labels, want.Labels)
		replacement.Annotations = have.Annotations
		if err := r.Delete(ctx, &have, deletePrecondition(&have)); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		if err := r.Create(ctx, replacement); err != nil {
			// Our own delete may not have completed: requeue rather than log a
			// replacement that is not there.
			return err
		}
		log.Printf("bootstrap: replaced ClusterRoleBinding/%s: roleRef %s -> %s (roleRef is immutable)",
			want.Name, have.RoleRef.Name, want.RoleRef.Name)
		return nil
	}
	if equality.Semantic.DeepEqual(have.Subjects, want.Subjects) {
		return nil
	}
	have.Subjects = want.Subjects
	if err := r.Update(ctx, &have); err != nil {
		return err
	}
	log.Printf("bootstrap: repaired ClusterRoleBinding/%s: subject %s/%s",
		have.Name, want.Subjects[0].Namespace, want.Subjects[0].Name)
	return nil
}

// mergeLabels returns existing with ours overlaid, so an object we replace
// keeps whatever a cluster admin put on it and still carries the labels the
// platform selects it by.
func mergeLabels(existing, ours map[string]string) map[string]string {
	out := make(map[string]string, len(existing)+len(ours))
	for k, v := range existing {
		out[k] = v
	}
	for k, v := range ours {
		out[k] = v
	}
	return out
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

// SetupWithManager registers the bootstrap reconciler.
func (r *BuiltinBootstrapReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("builtin-bootstrap").
		For(&v1alpha1.AgentTemplate{}).
		// An instance appearing or going is what mints or revokes its owner's
		// identity, so neither may wait for the periodic requeue.
		Watches(&v1alpha1.AgentInstance{}, handler.EnqueueRequestsFromMapFunc(r.mapToBootstrap)).
		Complete(r)
}

// mapToBootstrap wakes the singleton for any instance in its namespace: the set
// of owners is an input to it.
func (r *BuiltinBootstrapReconciler) mapToBootstrap(_ context.Context, obj client.Object) []reconcile.Request {
	if obj.GetNamespace() != r.Cfg.Namespace {
		return nil
	}
	return r.bootstrapRequest()
}

func (r *BuiltinBootstrapReconciler) bootstrapRequest() []reconcile.Request {
	return []reconcile.Request{{NamespacedName: types.NamespacedName{
		Namespace: r.Cfg.Namespace,
		Name:      BuiltinAgentName,
	}}}
}
