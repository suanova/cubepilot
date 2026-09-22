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

// TestInstanceNameFor verifies the instance key = user + agent (design §3.2),
// with
// DNS-1123 sanitization.
func TestInstanceNameFor(t *testing.T) {
	cases := []struct{ user, agent, want string }{
		{"zhang.wei", "cubepilot", "zhang-wei-cubepilot"},
		{"Zhang Wei", "cubepilot", "zhang-wei-cubepilot"},
		{"li.ming", "cubepilot", "li-ming-cubepilot"},
	}
	for _, c := range cases {
		if got := InstanceNameFor(c.user, c.agent); got != c.want {
			t.Errorf("InstanceNameFor(%q, %q) = %q, want %q", c.user, c.agent, got, c.want)
		}
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
		})
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()

	r := &BuiltinBootstrapReconciler{
		Client:    cl,
		APIReader: cl,
		Scheme:    scheme,
		Cfg: config.Config{
			Namespace:   "cubepilot",
			Users:       users,
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
			if err := cl.Get(context.Background(), types.NamespacedName{Name: userCRBName(u, role)}, &crb); err != nil {
				t.Errorf("CRB %s not created: %v", userCRBName(u, role), err)
			} else if crb.RoleRef.Name != role {
				t.Errorf("CRB %s roleRef = %s, want %s", userCRBName(u, role), crb.RoleRef.Name, role)
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

	// TaskTemplate exists.
	var tpl v1alpha1.TaskTemplate
	if err := cl.Get(context.Background(), types.NamespacedName{Name: "daily-inspection", Namespace: "cubepilot"}, &tpl); err != nil {
		t.Fatalf("daily-inspection template not created: %v", err)
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

	// Per-user instances exist (auto-instantiated per user).
	var insts v1alpha1.AgentInstanceList
	if err := cl.List(context.Background(), &insts, client.InNamespace("cubepilot")); err != nil {
		t.Fatalf("list instances: %v", err)
	}
	if len(insts.Items) != 2 {
		t.Fatalf("instances = %d, want 2", len(insts.Items))
	}
	for _, inst := range insts.Items {
		if inst.Spec.TemplateRef != "cubepilot" {
			t.Errorf("instance %s templateRef = %s", inst.Name, inst.Spec.TemplateRef)
		}
		bound := false
		for _, u := range users {
			if inst.Spec.Owner == u {
				bound = true
				break
			}
		}
		if !bound {
			t.Errorf("instance %s owner = %q, want one of the configured users %v", inst.Name, inst.Spec.Owner, users)
		}
	}

	// Idempotent: a second Ensure must not fail or duplicate.
	if err := r.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure #2: %v", err)
	}
	var insts2 v1alpha1.AgentInstanceList
	if err := cl.List(context.Background(), &insts2, client.InNamespace("cubepilot")); err != nil {
		t.Fatal(err)
	}
	if len(insts2.Items) != 2 {
		t.Errorf("instances after re-ensure = %d, want 2 (idempotent)", len(insts2.Items))
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

// TestBootstrapEnsureRejectsCollidingUsers verifies that identities which would
// derive to the same per-user SA are rejected before provisioning (issue #19:
// zhang.wei vs Zhang Wei must not share one ServiceAccount/revocation boundary).
func TestBootstrapEnsureRejectsCollidingUsers(t *testing.T) {
	scheme := testScheme(t)
	cl := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &BuiltinBootstrapReconciler{
		Client:    cl,
		APIReader: cl,
		Scheme:    scheme,
		Cfg:       config.Config{Namespace: "cubepilot", Users: []string{"zhang.wei", "Zhang Wei"}},
	}
	if err := r.Ensure(context.Background()); err == nil {
		t.Fatal("Ensure should reject sanitize-colliding identities")
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
