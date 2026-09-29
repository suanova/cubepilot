package controller

import (
	"bytes"
	"context"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/suanova/cubepilot/internal/api/v1alpha1"
	"github.com/suanova/cubepilot/internal/config"
	"github.com/suanova/cubepilot/internal/k8s"
	"github.com/suanova/cubepilot/internal/skill"
)

// seedInstance is the instance a user's identity follows: created by the Portal
// or the API, never by the platform.
func seedInstance(user, namespace string) *v1alpha1.AgentInstance {
	return &v1alpha1.AgentInstance{
		ObjectMeta: metav1.ObjectMeta{Name: k8s.InstanceName(user, BuiltinAgentName), Namespace: namespace},
		Spec:       v1alpha1.AgentInstanceSpec{TemplateRef: BuiltinAgentName, Owner: user},
	}
}

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add to scheme: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add corev1 to scheme: %v", err)
	}
	if err := rbacv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add rbac to scheme: %v", err)
	}
	return scheme
}

// TestBuiltinAgentShape verifies the builtin cubepilot definition
// (design §3.1: builtin: true, auto-instantiated per user, non-deletable;
// model[0] primary).
func TestBuiltinAgentShape(t *testing.T) {
	agent := BuiltinAgentTemplate(config.DefaultLLMEndpoint, config.DefaultLLMModel)
	if agent.Name != "cubepilot" {
		t.Errorf("name = %s, want cubepilot", agent.Name)
	}
	if agent.Labels["cubepilot/builtin"] != "true" {
		t.Error("builtin label missing")
	}
	if agent.Spec.DefaultModel != "platform/deepseek-v4-flash" {
		t.Errorf("defaultModel = %q, want platform/deepseek-v4-flash (design §3.1)", agent.Spec.DefaultModel)
	}
	if len(agent.Spec.Providers) != 1 || agent.Spec.Providers[0].Name != BuiltinProviderName {
		t.Errorf("inline providers = %v, want [%s]", agent.Spec.Providers, BuiltinProviderName)
	}
	if len(agent.Spec.Providers[0].Models) != 1 || agent.Spec.Providers[0].Models[0] != "deepseek-v4-flash" {
		t.Errorf("builtin model ids = %v, want [deepseek-v4-flash]", agent.Spec.Providers[0].Models)
	}
	if agent.Spec.Providers[0].Endpoint != config.DefaultLLMEndpoint {
		t.Errorf("builtin provider endpoint = %q, want %q", agent.Spec.Providers[0].Endpoint, config.DefaultLLMEndpoint)
	}
	if agent.Spec.Providers[0].CredentialRef == nil || agent.Spec.Providers[0].CredentialRef.Name != "cubepilot-llm" {
		t.Errorf("builtin provider credentialRef = %+v, want cubepilot-llm", agent.Spec.Providers[0].CredentialRef)
	}
	if agent.Spec.ApprovalPolicy != v1alpha1.ApprovalPolicyAllowlist {
		t.Errorf("approvalPolicy = %q, want Allowlist (design §3.1)", agent.Spec.ApprovalPolicy)
	}
	if len(agent.Spec.Skills) == 0 {
		t.Error("builtin agent should reference skills/skills")
	}
}

// TestBootstrapEnsure verifies the builtin bootstrap creates the Agent,
// TaskTemplate and per-user instances idempotently (design §3.1 / §5.3). The
// builtin Skill CRDs are seeded by the API (covered in internal/server).
func TestBootstrapEnsure(t *testing.T) {
	scheme := testScheme(t)
	users := []string{"zhang.wei", "li.ming"}
	// The token Secrets the generator reads must be pre-populated (the fake
	// token controller does not fill data.token like a real API server).
	var objs []client.Object
	for _, u := range users {
		saName := k8s.UserServiceAccountName(u)
		objs = append(objs, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:        saName + "-token",
				Namespace:   "cubepilot",
				Annotations: map[string]string{corev1.ServiceAccountNameKey: saName},
			},
			Type: corev1.SecretTypeServiceAccountToken,
			Data: map[string][]byte{"token": []byte("tok-" + u)},
		}, seedInstance(u, "cubepilot"))
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()

	r := &BuiltinBootstrapReconciler{
		Client:    cl,
		APIReader: cl,
		Scheme:    scheme,
		Cfg: config.Config{
			Namespace:   "cubepilot",
			LLMEndpoint: config.DefaultLLMEndpoint,
			LLMModel:    config.DefaultLLMModel,
		},
	}
	if err := r.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	// Per-user identity is generated: SA + view/CRD ClusterRoleBindings + a
	// kubeconfig Secret under the dual-kubeconfig naming scheme (the assistant
	// identity stays cluster-scoped -- it operates platform CRs in any
	// namespace; only the component RBAC narrowed with namespaced CRDs).
	for _, u := range users {
		saName := k8s.UserServiceAccountName(u)
		var sa corev1.ServiceAccount
		if err := cl.Get(context.Background(), types.NamespacedName{Name: saName, Namespace: "cubepilot"}, &sa); err != nil {
			t.Errorf("per-user SA %s not created: %v", saName, err)
		}
		for _, role := range userClusterRoles {
			var crb rbacv1.ClusterRoleBinding
			if err := cl.Get(context.Background(), types.NamespacedName{Name: userCRBName("cubepilot", u, role)}, &crb); err != nil {
				t.Errorf("CRB %s not created: %v", userCRBName("cubepilot", u, role), err)
			} else if crb.RoleRef.Name != role {
				t.Errorf("CRB %s roleRef = %s, want %s", userCRBName("cubepilot", u, role), crb.RoleRef.Name, role)
			} else if len(crb.Subjects) != 1 || crb.Subjects[0].Namespace != "cubepilot" || crb.Subjects[0].Name != saName {
				t.Errorf("CRB %s subject = %+v, want %s/cubepilot", userCRBName("cubepilot", u, role), crb.Subjects, saName)
			}
		}
		var kc corev1.Secret
		if err := cl.Get(context.Background(), types.NamespacedName{Name: k8s.UserKubeconfigSecretFor(u), Namespace: "cubepilot"}, &kc); err != nil {
			t.Errorf("per-user kubeconfig Secret not created: %v", err)
		} else if !strings.Contains(string(kc.Data["config"]), "tok-"+u) {
			t.Errorf("kubeconfig Secret for %s does not carry the per-user token", u)
		}
	}

	// Agent definition exists.
	var agent v1alpha1.AgentTemplate
	if err := cl.Get(context.Background(), types.NamespacedName{Name: "cubepilot", Namespace: "cubepilot"}, &agent); err != nil {
		t.Fatalf("cubepilot not created: %v", err)
	}
	if len(agent.Spec.Skills) != len(skill.BuiltinSkillNames()) {
		t.Errorf("agent skills = %v, want the builtin presets", agent.Spec.Skills)
	}

	// Every preset TaskTemplate exists, carries the builtin label, and survives
	// being rendered. Three failures this catches are all silent at runtime:
	// params travel through the instruction and nowhere else, so a param with no
	// default reaches the agent as a literal {{placeholder}}, a declared param
	// the instruction never mentions is dropped without a trace, and a skill the
	// platform does not ship drops that run's skill revision.
	builtinSkills := map[string]bool{}
	for _, n := range skill.BuiltinSkillNames() {
		builtinSkills[n] = true
	}
	presets, err := BuiltinTaskTemplates()
	if err != nil {
		t.Fatalf("load the preset task templates: %v", err)
	}
	if len(presets) == 0 {
		t.Fatal("no builtin task templates")
	}
	seen := map[string]bool{}
	for _, preset := range presets {
		name := preset.Name
		if seen[name] {
			t.Errorf("builtin task template %s declared twice", name)
		}
		seen[name] = true
		var got v1alpha1.TaskTemplate
		if err := cl.Get(context.Background(), types.NamespacedName{Name: name, Namespace: "cubepilot"}, &got); err != nil {
			t.Errorf("task template %s not created: %v", name, err)
			continue
		}
		if got.Labels["cubepilot/builtin"] != "true" {
			t.Errorf("task template %s missing the builtin label", name)
		}
		for _, p := range got.Spec.ParamsSchema {
			if p.Default == "" {
				t.Errorf("task template %s: param %q has no default, so the agent would see a literal {{%s}}", name, p.Name, p.Name)
			}
			if !strings.Contains(got.Spec.Instruction, "{{"+p.Name+"}}") {
				t.Errorf("task template %s: param %q is declared but the instruction never interpolates it, so the wizard's value goes nowhere", name, p.Name)
			}
		}
		for _, s := range got.Spec.Skills {
			if !builtinSkills[s] {
				t.Errorf("task template %s references skill %q, which the platform does not ship", name, s)
			}
		}
	}
	if !seen[BuiltinTaskTemplateName] {
		t.Errorf("the presets no longer include %s", BuiltinTaskTemplateName)
	}

	// Providers are inlined in the template (design §3.3): the builtin template
	// carries the preset inline provider entries.
	tmpl := v1alpha1.AgentTemplate{}
	if err := cl.Get(context.Background(), types.NamespacedName{Name: "cubepilot", Namespace: "cubepilot"}, &tmpl); err != nil {
		t.Fatalf("cubepilot template not found: %v", err)
	}
	if len(tmpl.Spec.Providers) != len(BuiltinProviders(config.DefaultLLMEndpoint, config.DefaultLLMModel)) {
		t.Errorf("inline providers = %d, want %d", len(tmpl.Spec.Providers), len(BuiltinProviders(config.DefaultLLMEndpoint, config.DefaultLLMModel)))
	}

	// The platform creates no instance of its own: the two seeded ones are what
	// the identities above hang off.
	var insts v1alpha1.AgentInstanceList
	if err := cl.List(context.Background(), &insts, client.InNamespace("cubepilot")); err != nil {
		t.Fatalf("list instances: %v", err)
	}
	if len(insts.Items) != len(users) {
		t.Fatalf("instances = %d, want the %d seeded (the bootstrap creates none)", len(insts.Items), len(users))
	}

	// Idempotent: a second Ensure must not fail or duplicate.
	if err := r.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure #2: %v", err)
	}
	var insts2 v1alpha1.AgentInstanceList
	if err := cl.List(context.Background(), &insts2, client.InNamespace("cubepilot")); err != nil {
		t.Fatal(err)
	}
	if len(insts2.Items) != len(users) {
		t.Errorf("instances after re-ensure = %d, want %d (the bootstrap creates none)", len(insts2.Items), len(users))
	}
}

// TestBootstrapEnsureNoDefaultModel verifies that with no LLM endpoint/model
// configured the builtin cubepilot template is created model-less (issue
// #117: "no default LLM" is a first-class install state; LLMs are added later
// from the Portal).
func TestBootstrapEnsureNoDefaultModel(t *testing.T) {
	scheme := testScheme(t)
	cl := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &BuiltinBootstrapReconciler{
		Client:    cl,
		APIReader: cl,
		Scheme:    scheme,
		Cfg:       config.Config{Namespace: "cubepilot"},
	}
	if err := r.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	var agent v1alpha1.AgentTemplate
	if err := cl.Get(context.Background(), types.NamespacedName{Name: "cubepilot", Namespace: "cubepilot"}, &agent); err != nil {
		t.Fatalf("cubepilot not created: %v", err)
	}
	if len(agent.Spec.Providers) != 0 {
		t.Errorf("providers = %v, want none when no LLM configured", agent.Spec.Providers)
	}
	if agent.Spec.DefaultModel != "" {
		t.Errorf("defaultModel = %q, want empty when no LLM configured", agent.Spec.DefaultModel)
	}
}

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

// TestBootstrapRevokesThePreviousNamespacesBindings covers the move: the new
// install's desired set is its own namespace's, so a binding an earlier install
// left behind is outside it and gets revoked.
func TestBootstrapRevokesThePreviousNamespacesBindings(t *testing.T) {
	scheme := testScheme(t)
	users := []string{"admin"}
	var objs []client.Object
	for _, ns := range []string{"ns-a", "ns-b"} {
		for _, u := range users {
			saName := k8s.UserServiceAccountName(u)
			objs = append(objs, &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:        saName + "-token",
					Namespace:   ns,
					Annotations: map[string]string{corev1.ServiceAccountNameKey: saName},
				},
				Type: corev1.SecretTypeServiceAccountToken,
				Data: map[string][]byte{"token": []byte("tok-" + ns)},
			}, seedInstance(u, ns))
		}
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	ensure := func(ns string) {
		t.Helper()
		r := &BuiltinBootstrapReconciler{
			Client:    cl,
			APIReader: cl,
			Scheme:    scheme,
			Cfg:       config.Config{Namespace: ns},
		}
		if err := r.Ensure(context.Background()); err != nil {
			t.Fatalf("Ensure in %s: %v", ns, err)
		}
	}

	ensure("ns-a")
	ensure("ns-b")

	for _, u := range users {
		saName := k8s.UserServiceAccountName(u)
		// The install that just ran owns its own bindings ...
		for _, role := range userClusterRoles {
			var crb rbacv1.ClusterRoleBinding
			name := userCRBName("ns-b", u, role)
			if err := cl.Get(context.Background(), types.NamespacedName{Name: name}, &crb); err != nil {
				t.Errorf("CRB %s missing: %v", name, err)
				continue
			}
			if len(crb.Subjects) != 1 || crb.Subjects[0].Namespace != "ns-b" || crb.Subjects[0].Name != saName {
				t.Errorf("CRB %s subject = %+v, want %s/ns-b", name, crb.Subjects, saName)
			}
		}
		// ... and the namespace it replaced keeps nothing that still grants.
		for _, role := range userClusterRoles {
			name := userCRBName("ns-a", u, role)
			if err := cl.Get(context.Background(), types.NamespacedName{Name: name}, &rbacv1.ClusterRoleBinding{}); !apierrors.IsNotFound(err) {
				t.Errorf("CRB %s from the previous namespace survived (err=%v): it grants that namespace's ServiceAccount whatever this install binds", name, err)
			}
		}
	}
}

// TestBootstrapIgnoresTerminatingInstances: a terminating instance's finalizer is
// revoking its owner's identity, and minting it back here would undo exactly what
// a teardown waits for.
func TestBootstrapIgnoresTerminatingInstances(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	now := metav1.Now()
	inst := seedInstance("admin", "cubepilot")
	inst.DeletionTimestamp = &now
	inst.Finalizers = []string{finalizerName}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(inst).Build()
	r := &BuiltinBootstrapReconciler{
		Client: cl, APIReader: cl, Scheme: scheme,
		Cfg: config.Config{Namespace: "cubepilot"},
	}
	if err := r.Ensure(ctx); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if err := cl.Get(ctx, types.NamespacedName{Namespace: "cubepilot", Name: k8s.UserServiceAccountName("admin")}, &corev1.ServiceAccount{}); !apierrors.IsNotFound(err) {
		t.Errorf("the identity of a terminating instance's owner was minted (err=%v)", err)
	}
}

// TestBootstrapPrunesOnlyTheRemovedUser covers the narrower case: one user's
// instance goes, the other user keeps their identity.
func TestBootstrapPrunesOnlyTheRemovedUser(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	users := []string{"zhang.wei", "li.ming"}
	var objs []client.Object
	for _, u := range users {
		saName := k8s.UserServiceAccountName(u)
		objs = append(objs, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:        saName + "-token",
				Namespace:   "cubepilot",
				Labels:      map[string]string{"cubepilot/builtin": "true"},
				Annotations: map[string]string{corev1.ServiceAccountNameKey: saName},
			},
			Type: corev1.SecretTypeServiceAccountToken,
			Data: map[string][]byte{"token": []byte("tok-" + u)},
		}, seedInstance(u, "cubepilot"))
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	r := &BuiltinBootstrapReconciler{
		Client: cl, APIReader: cl, Scheme: scheme,
		Cfg: config.Config{Namespace: "cubepilot"},
	}
	if err := r.Ensure(ctx); err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	// Delete one user's instance and reconcile again.
	if err := cl.Delete(ctx, seedInstance("li.ming", "cubepilot")); err != nil {
		t.Fatalf("delete instance: %v", err)
	}
	if err := r.Ensure(ctx); err != nil {
		t.Fatalf("Ensure after the instance went: %v", err)
	}

	if err := cl.Get(ctx, types.NamespacedName{Namespace: "cubepilot", Name: k8s.UserServiceAccountName("li.ming")}, &corev1.ServiceAccount{}); !apierrors.IsNotFound(err) {
		t.Errorf("the removed user's ServiceAccount survived (err=%v)", err)
	}
	if err := cl.Get(ctx, types.NamespacedName{Name: userCRBName("cubepilot", "li.ming", UserViewClusterRole)}, &rbacv1.ClusterRoleBinding{}); !apierrors.IsNotFound(err) {
		t.Errorf("the removed user's binding survived (err=%v)", err)
	}
	if err := cl.Get(ctx, types.NamespacedName{Namespace: "cubepilot", Name: k8s.UserServiceAccountName("zhang.wei")}, &corev1.ServiceAccount{}); err != nil {
		t.Errorf("the remaining user's ServiceAccount was removed: %v", err)
	}
	if err := cl.Get(ctx, types.NamespacedName{Name: userCRBName("cubepilot", "zhang.wei", UserViewClusterRole)}, &rbacv1.ClusterRoleBinding{}); err != nil {
		t.Errorf("the remaining user's binding was removed: %v", err)
	}
}

// TestBootstrapStillPrunesWhenAMintFails pins the order-independence of the two
// halves: an unready token for one user must not stall another user's removal,
// because revocation is the half a teardown waits on.
func TestBootstrapStillPrunesWhenAMintFails(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	// admin has an instance but no token yet, so minting admin fails this pass.
	stale := k8s.UserServiceAccountName("li.ming")
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		seedInstance("admin", "cubepilot"),
		&corev1.ServiceAccount{
			ObjectMeta: metav1.ObjectMeta{Name: stale, Namespace: "cubepilot", Labels: map[string]string{"cubepilot/builtin": "true"}},
		},
	).Build()
	r := &BuiltinBootstrapReconciler{
		Client: cl, APIReader: cl, Scheme: scheme,
		Cfg: config.Config{Namespace: "cubepilot"},
	}
	if err := r.Ensure(ctx); err == nil {
		t.Error("Ensure should report the unready token")
	}
	if err := cl.Get(ctx, types.NamespacedName{Namespace: "cubepilot", Name: stale}, &corev1.ServiceAccount{}); !apierrors.IsNotFound(err) {
		t.Errorf("the revoked user's ServiceAccount survived a failed mint (err=%v)", err)
	}
}

// TestBootstrapIdentityFollowsInstances pins when a user has credentials: the
// platform mints none of its own accord, mints them once the user has an
// instance, and revokes them when the last one is gone.
func TestBootstrapIdentityFollowsInstances(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	saName := k8s.UserServiceAccountName("admin")
	token := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:        saName + "-token",
			Namespace:   "cubepilot",
			Labels:      map[string]string{"cubepilot/builtin": "true"},
			Annotations: map[string]string{corev1.ServiceAccountNameKey: saName},
		},
		Type: corev1.SecretTypeServiceAccountToken,
		Data: map[string][]byte{"token": []byte("tok")},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &BuiltinBootstrapReconciler{
		Client: cl, APIReader: cl, Scheme: scheme,
		Cfg: config.Config{Namespace: "cubepilot"},
	}
	if err := r.Ensure(ctx); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	// No instance, no identity -- not even an idle ServiceAccount.
	if err := cl.Get(ctx, types.NamespacedName{Namespace: "cubepilot", Name: saName}, &corev1.ServiceAccount{}); !apierrors.IsNotFound(err) {
		t.Errorf("a user with no instance got an identity anyway (err=%v)", err)
	}

	// The Portal creates the instance; the identity follows on the next pass,
	// once the API server has filled in the token Secret it mints for the SA.
	inst := seedInstance("admin", "cubepilot")
	if err := cl.Create(ctx, inst); err != nil {
		t.Fatalf("create instance: %v", err)
	}
	if err := cl.Create(ctx, token); err != nil {
		t.Fatalf("create token secret: %v", err)
	}
	if err := r.Ensure(ctx); err != nil {
		t.Fatalf("Ensure with an instance: %v", err)
	}
	for _, tc := range []struct {
		what string
		obj  client.Object
	}{
		{"ServiceAccount", &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Namespace: "cubepilot", Name: saName}}},
		{"kubeconfig Secret", &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "cubepilot", Name: k8s.UserKubeconfigSecretFor("admin")}}},
		{"binding", &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: userCRBName("cubepilot", "admin", UserCRDsClusterRole)}}},
	} {
		if err := cl.Get(ctx, client.ObjectKeyFromObject(tc.obj), tc.obj); err != nil {
			t.Fatalf("%s not minted for the instance's owner: %v", tc.what, err)
		}
	}

	// The last instance going takes the identity with it.
	if err := cl.Delete(ctx, inst); err != nil {
		t.Fatalf("delete instance: %v", err)
	}
	if err := r.Ensure(ctx); err != nil {
		t.Fatalf("Ensure after the instance went: %v", err)
	}
	for _, tc := range []struct {
		what string
		obj  client.Object
	}{
		{"ServiceAccount", &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Namespace: "cubepilot", Name: saName}}},
		{"token Secret", &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "cubepilot", Name: saName + "-token"}}},
		{"kubeconfig Secret", &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "cubepilot", Name: k8s.UserKubeconfigSecretFor("admin")}}},
		{"binding", &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: userCRBName("cubepilot", "admin", UserCRDsClusterRole)}}},
	} {
		if err := cl.Get(ctx, client.ObjectKeyFromObject(tc.obj), tc.obj); !apierrors.IsNotFound(err) {
			t.Errorf("%s outlived the last instance (err=%v)", tc.what, err)
		}
	}
}

// TestPerUserRolesAreBindable guards the one failure in this area that nothing
// else sees. The operator creates the per-user ClusterRoleBindings, and the API
// server refuses to create a binding whose roleRef its creator may not `bind`.
// The chart lists the bindable names in the `resourceNames` of its
// clusterroles/bind rule, so a role added to userClusterRoles and bound by the
// code but not listed there produces a binding that is never created -- an
// error that only ever surfaces in the operator's log, never in a test or a
// reconcile failure the user sees.
func TestPerUserRolesAreBindable(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "deploy", "charts", "cubepilot-chart", "templates", "rbac.yaml"))
	if err != nil {
		t.Fatalf("read the chart rbac: %v", err)
	}
	bindable := bindableClusterRoleNames(string(raw))
	if len(bindable) == 0 {
		t.Fatal("the chart lists no bindable ClusterRole names -- has the rule moved?")
	}
	for _, role := range userClusterRoles {
		if !bindable[role] {
			t.Errorf("the chart does not let the operator bind %q: add it to the resourceNames of the clusterroles/bind rule", role)
		}
	}
}

// bindableClusterRoleNames collects the names from the chart's `resourceNames`
// arrays, restricted to the rule that grants `bind` -- the one listing `view`.
func bindableClusterRoleNames(chart string) map[string]bool {
	out := map[string]bool{}
	for _, m := range regexp.MustCompile(`resourceNames:\s*\[([^\]]*)\]`).FindAllStringSubmatch(chart, -1) {
		var names []string
		for _, part := range strings.Split(m[1], ",") {
			names = append(names, strings.Trim(strings.TrimSpace(part), `"'`))
		}
		if !slices.Contains(names, UserViewClusterRole) {
			continue // some other rule's resourceNames
		}
		for _, n := range names {
			out[n] = true
		}
	}
	return out
}
